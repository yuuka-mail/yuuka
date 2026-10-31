package store_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/store"
)

func (e *ruleEnv) note(v *auth.Viewer, message, scope, text string) (store.Note, bool) {
	e.t.Helper()
	n, created, err := e.db.WriteNote(e.t.Context(), v, store.NewNote{
		MessageID: message, Scope: scope, Text: text, Cites: []store.Cite{{URI: "yuuka://message/" + message, Quote: "q"}},
	}, e.action("test.note"))
	if err != nil {
		e.t.Fatal(err)
	}
	return n, created
}

func noteTexts(ns []store.Note) []string {
	out := []string{}
	for _, n := range ns {
		out = append(out, n.Text)
	}
	return out
}

// One note per author per target, overwritten in place; a person sees
// every note on the mail, and so does an agent; deleting is the undo.
func TestNotes(t *testing.T) {
	t.Parallel()
	e := newRuleEnv(t)
	ctx := t.Context()
	arisu, momoi := e.principal("agent", "arisu"), e.principal("agent", "momoi")
	operator := e.principal("human", "operator")
	m := e.deliver("a", letter("x@example.net", "a@example.com", "Invoice", "Message-ID: <inv@example.net>\r\n", "pay by friday")).MessageID
	reply := e.deliver("a", letter("x@example.net", "a@example.com", "Re: Invoice", "In-Reply-To: <inv@example.net>\r\n", "reminder")).MessageID

	first, created := e.note(arisu, m, store.ScopeMessage, "arisu one")
	if !created {
		t.Error("first note not created")
	}
	again, created := e.note(arisu, m, store.ScopeMessage, "arisu two")
	if created || again.ID != first.ID || !again.CreatedAt.Equal(first.CreatedAt) || !again.UpdatedAt.After(first.UpdatedAt) ||
		again.Text != "arisu two" || again.Author != "agent:arisu" {
		t.Errorf("overwrite: %+v, first %+v", again, first)
	}
	e.note(momoi, m, store.ScopeMessage, "momoi on message")
	// A thread note anchored on the reply shows on the first message.
	e.note(momoi, reply, store.ScopeThread, "momoi on thread")

	// Whoever sees the message sees every note on it (§8.10).
	for _, v := range []*auth.Viewer{arisu, momoi, operator} {
		want := []string{"arisu two", "momoi on message", "momoi on thread"}
		ns, err := e.db.Notes(ctx, v, m)
		if err != nil || strings.Join(noteTexts(ns), "|") != strings.Join(want, "|") {
			t.Errorf("Notes as %s: %q %v, want %q", v.PrincipalName, noteTexts(ns), err, want)
		}
	}
	// The change log carries each note once, with the message it is on
	// (§8.6), for every reader of that message.
	var since int64
	page, err := e.db.Changes(ctx, arisu, store.ChangeQuery{Since: &since, Kinds: []string{"note"}})
	if err != nil || len(page.Changes) != 3 || page.Changes[0].ID != first.ID || page.Changes[0].MessageID != m {
		t.Errorf("note changes for arisu: %+v %v", page.Changes, err)
	}

	// Invalid writes: no cites, no text, too long, a bad scope.
	for name, n := range map[string]store.NewNote{
		"no cites": {MessageID: m, Scope: store.ScopeMessage, Text: "x"},
		"blank":    {MessageID: m, Scope: store.ScopeMessage, Text: "  \n", Cites: []store.Cite{{URI: "yuuka://message/" + m}}},
		"long":     {MessageID: m, Scope: store.ScopeMessage, Text: strings.Repeat("字", store.MaxNoteChars+1), Cites: []store.Cite{{URI: "yuuka://message/" + m}}},
		"scope":    {MessageID: m, Scope: "account", Text: "x", Cites: []store.Cite{{URI: "yuuka://message/" + m}}},
	} {
		if _, _, err := e.db.WriteNote(ctx, arisu, n, e.action("test.note")); !errors.Is(err, store.ErrInvalidNote) {
			t.Errorf("%s: %v, want ErrInvalidNote", name, err)
		}
	}
	// Exactly the limit is fine; the table's own CHECK agrees on
	// characters, not bytes.
	if _, _, err := e.db.WriteNote(ctx, arisu, store.NewNote{
		MessageID: m, Scope: store.ScopeMessage, Text: strings.Repeat("字", store.MaxNoteChars), Cites: []store.Cite{{URI: "yuuka://message/" + m}},
	}, e.action("test.note")); err != nil {
		t.Errorf("a note of %d characters: %v", store.MaxNoteChars, err)
	}
	// And the table refuses an empty cites list by itself.
	if _, err := e.pool.Exec(ctx, `INSERT INTO notes (id, author_id, message_id, scope, body, cites, created_at, updated_at)
		VALUES ('01ARZ3NDEKTSV4RRFFQ69G5FAV', $1, $2, 'thread', 'x', '[]', now(), now())`, arisu.PrincipalID, m); err == nil {
		t.Error("the table took a note without cites")
	}
	// A message nobody can see is not there to note.
	if _, _, err := e.db.WriteNote(ctx, arisu, store.NewNote{
		MessageID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Scope: store.ScopeMessage, Text: "x", Cites: []store.Cite{{URI: "yuuka://message/" + m}},
	}, e.action("test.note")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("note on a missing message: %v", err)
	}

	// An agent deletes only its own notes: naming another author is not
	// permitted, and the person has none of their own here.
	if err := e.db.DeleteNote(ctx, operator, m, store.ScopeMessage, "", e.action("test.delete")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("person deleting their own note, which does not exist: %v", err)
	}
	if err := e.db.DeleteNote(ctx, momoi, m, store.ScopeMessage, arisu.PrincipalID, e.action("test.delete")); !errors.Is(err, store.ErrNotPermitted) {
		t.Errorf("agent deleting another's note: %v", err)
	}
	if err := e.db.DeleteNote(ctx, arisu, m, store.ScopeMessage, "", e.action("test.delete")); err != nil {
		t.Fatal(err)
	}
	if ns, _ := e.db.Notes(ctx, operator, m); strings.Join(noteTexts(ns), "|") != "momoi on message|momoi on thread" {
		t.Errorf("after delete: %q", noteTexts(ns))
	}
	if err := e.db.DeleteNote(ctx, arisu, m, store.ScopeMessage, "", e.action("test.delete")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleting twice: %v", err)
	}
	// A person deletes an agent's note, here a thread note through
	// another message of the thread, as their own action naming the
	// author.
	byPerson := e.action("test.delete")
	byPerson.Actor = operator.PrincipalID
	if err := e.db.DeleteNote(ctx, operator, m, store.ScopeThread, momoi.PrincipalID, byPerson); err != nil {
		t.Errorf("person deleting an agent's thread note: %v", err)
	}
	if ns, _ := e.db.Notes(ctx, operator, m); strings.Join(noteTexts(ns), "|") != "momoi on message" {
		t.Errorf("after the person's delete: %q", noteTexts(ns))
	}
	var actor, author string
	if err := e.pool.QueryRow(ctx, `SELECT actor, args->>'author' FROM actions WHERE id = $1`, byPerson.ID).Scan(&actor, &author); err != nil ||
		actor != operator.PrincipalID || author != momoi.PrincipalID {
		t.Errorf("the person's delete recorded as %s, author %s: %v", actor, author, err)
	}

	// The audit log records each write's shape, never the text.
	var leaked int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM actions WHERE tool IN ('test.note', 'test.delete')
		AND (args::text || coalesce(result::text, '')) ~ '(arisu|momoi) (one|two|on)'`).Scan(&leaked); err != nil || leaked != 0 {
		t.Errorf("%d audit rows hold a note's text: %v", leaked, err)
	}
	var args, result string
	if err := e.pool.QueryRow(ctx, `SELECT args::text, result::text FROM actions WHERE tool = 'test.note' ORDER BY id LIMIT 1`).Scan(&args, &result); err != nil ||
		!strings.Contains(args, `"cites": 1`) || !strings.Contains(args, first.ID) || !strings.Contains(result, `"chars": 9`) {
		t.Errorf("audit of a note: %s %s %v", args, result, err)
	}
}

// Writing a thread note keeps one per author per thread: after two
// threads merge, the author's next write keeps one note and drops the
// other, moving the kept one onto the message named.
func TestThreadNoteAfterMerge(t *testing.T) {
	t.Parallel()
	e := newRuleEnv(t)
	ctx := t.Context()
	arisu := e.principal("agent", "arisu")
	a := e.deliver("a", letter("x@example.net", "a@example.com", "One", "", "one")).MessageID
	b := e.deliver("a", letter("y@example.net", "a@example.com", "Two", "", "two")).MessageID
	na, _ := e.note(arisu, a, store.ScopeThread, "on a")
	e.note(arisu, b, store.ScopeThread, "on b")
	if _, err := e.pool.Exec(ctx, `UPDATE messages SET thread_id = (SELECT thread_id FROM messages WHERE id = $1) WHERE id = $2`, a, b); err != nil {
		t.Fatal(err)
	}
	if ns, _ := e.db.Notes(ctx, arisu, a); len(ns) != 2 {
		t.Fatalf("merged thread: %d notes", len(ns))
	}
	// Written through b: b's own note is kept (its id), a's is dropped.
	n, created := e.note(arisu, b, store.ScopeThread, "merged")
	ns, err := e.db.Notes(ctx, arisu, a)
	if err != nil || created || len(ns) != 1 || ns[0].ID != n.ID || n.ID == na.ID || ns[0].MessageID != b || ns[0].Text != "merged" {
		t.Errorf("after write: %+v %v", ns, err)
	}
	// Written through a now: the note moves onto a, keeping its id.
	moved, _ := e.note(arisu, a, store.ScopeThread, "moved")
	if moved.ID != n.ID || moved.MessageID != a {
		t.Errorf("moved: %+v", moved)
	}
}

// A note is a decision (§4.2): reparse leaves it alone, and it is no
// fact a rule can match.
func TestNotesSurviveReparseAndStayOutOfRules(t *testing.T) {
	t.Parallel()
	e := newRuleEnv(t)
	ctx := t.Context()
	arisu := e.principal("agent", "arisu")
	raw := letter("orders@example.net", "shop@example.com", "Order", "", "shipped")
	m := e.deliver("shop", raw).MessageID
	n, _ := e.note(arisu, m, store.ScopeMessage, "NOTE-AS-FACT phishing")

	fs, err := e.db.FactsFor(ctx, person, m)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(fs)
	if err != nil || len(fs) == 0 || strings.Contains(string(b), "NOTE-AS-FACT") {
		t.Errorf("rule facts: %s %v", b, err)
	}

	targets, err := e.db.ReparseTargets(ctx, time.Time{}, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range targets {
		if _, err := e.db.Rebuild(ctx, tg, mimeparse.Parse(raw), map[string]authres.Auth{tg.Deliveries[0].DeliveryID: {}}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := e.db.NoteByID(ctx, arisu, n.ID)
	if err != nil || got.Text != n.Text || got.MessageID != m || !got.UpdatedAt.Equal(n.UpdatedAt) {
		t.Errorf("after reparse: %+v %v, want %+v", got, err, n)
	}
}
