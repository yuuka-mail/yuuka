package mcp_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/mcp"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// An agent writes, overwrites and deletes its note; get_message gives
// it back inside the untrusted boundary, apart from the facts, and never
// another agent's. Cites are required, and the audit log keeps the
// note's shape, never its text.
func TestNoteTools(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	cs := e.client(e.viewer(auth.MessageAnnotate, auth.MessageRead))
	id := e.ids["plain-otp"]
	uri := agentview.MessageURI(id)
	cite := []map[string]any{{"uri": uri + "#part/1/para/1", "quote": "your code"}}

	// The input schema requires cites; an empty list is refused too.
	for _, args := range []map[string]any{{"uri": uri, "text": "no cites"}, {"uri": uri, "text": "no cites", "cites": []any{}}} {
		if r := callTool(t, cs, "note", args); !r.isError || !strings.Contains(r.text, "cites") {
			t.Errorf("note without cites: %+v", r)
		}
	}
	if r := callTool(t, cs, "note", map[string]any{"uri": uri, "text": strings.Repeat("x", store.MaxNoteChars+1), "cites": cite}); !r.isError ||
		!strings.Contains(r.text, "4000") {
		t.Errorf("note too long: %+v", r)
	}
	r := callTool(t, cs, "note", map[string]any{"uri": uri, "text": "first SECRET-NOTE-TEXT", "cites": cite})
	first := structured[mcp.Note](t, r)
	if r.isError || first.ID == "" || first.Target != uri || first.Scope != "message" || !strings.HasPrefix(r.text, "Wrote") {
		t.Fatalf("note: %+v\n%s", first, r.text)
	}
	r = callTool(t, cs, "note", map[string]any{"uri": uri, "text": "second SECRET-NOTE-TEXT", "cites": cite})
	second := structured[mcp.Note](t, r)
	if second.ID != first.ID || second.Text != "second SECRET-NOTE-TEXT" || !strings.HasPrefix(r.text, "Replaced") {
		t.Errorf("overwrite: %+v\n%s", second, r.text)
	}

	// Another agent's note on the same message stays out of view.
	other, err := e.db.EnsurePrincipal(ctx, store.Principal{ID: ulid.New(time.Now()), Kind: "agent", Name: "momoi", DisplayName: "momoi", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.db.WriteNote(ctx, &auth.Viewer{PrincipalID: other.ID, PrincipalKind: "agent", PrincipalName: "momoi"},
		store.NewNote{MessageID: id, Scope: store.ScopeThread, Text: "MOMOI-NOTE", Cites: []store.Cite{{URI: uri}}},
		store.Action{ID: ulid.New(time.Now()), Actor: other.ID, Tool: "test.note", At: time.Now()}); err != nil {
		t.Fatal(err)
	}

	r = callTool(t, cs, "get_message", map[string]any{"uri": uri})
	var got struct {
		Facts json.RawMessage `json:"facts"`
		Notes []mcp.Note      `json:"notes"`
	}
	if err := json.Unmarshal(r.structured, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Notes) != 1 || got.Notes[0].ID != first.ID || strings.Contains(string(got.Facts), "SECRET-NOTE-TEXT") {
		t.Errorf("get_message notes %+v, facts %s", got.Notes, got.Facts)
	}
	if strings.Contains(r.text, "MOMOI-NOTE") || strings.Contains(string(r.structured), "MOMOI-NOTE") {
		t.Errorf("get_message shows another agent's note:\n%s", r.text)
	}
	// The text sits in its own untrusted boundary after the mail's, with
	// the author named outside it.
	at := strings.Index(r.text, "second SECRET-NOTE-TEXT")
	fence := strings.LastIndex(r.text[:max(at, 0)], "<untrusted_mail_content")
	if at < 0 || fence < 0 || !strings.Contains(r.text[fence:at], agentview.NoteNotice) ||
		!strings.Contains(r.text[:fence], "by agent:arisu") || strings.Count(r.text, "<untrusted_mail_content") != 2 {
		t.Errorf("get_message text:\n%s", r.text)
	}

	// A thread note is a second target; deleting takes one back at a
	// time.
	if r := callTool(t, cs, "note", map[string]any{"uri": uri, "scope": "thread", "text": "thread SECRET-NOTE-TEXT", "cites": cite}); r.isError {
		t.Fatal(r.text)
	}
	if r := callTool(t, cs, "delete_note", map[string]any{"uri": uri}); r.isError || !structured[mcp.DeleteNoteOutput](t, r).Deleted {
		t.Errorf("delete_note: %+v", r)
	}
	if r := callTool(t, cs, "delete_note", map[string]any{"uri": uri}); !r.isError || !strings.Contains(r.text, "no message note") {
		t.Errorf("delete_note again: %+v", r)
	}
	ns, err := e.db.Notes(ctx, &auth.Viewer{PrincipalID: e.agent.ID, PrincipalKind: "agent", PrincipalName: "arisu"}, id)
	if err != nil || len(ns) != 1 || ns[0].Scope != store.ScopeThread {
		t.Errorf("left: %+v %v", ns, err)
	}

	var leaked int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM actions WHERE (args::text || coalesce(result::text, '')) LIKE '%SECRET-NOTE%'`).Scan(&leaked); err != nil || leaked != 0 {
		t.Errorf("%d audit rows hold a note's text: %v", leaked, err)
	}
	if n := e.count("mcp.note"); n != 5 {
		t.Errorf("%d mcp.note actions, want 5 (two refused, three written)", n)
	}
}
