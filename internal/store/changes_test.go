package store_test

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// insertChanges matches SQL that writes the change log.
var insertChanges = regexp.MustCompile(`(?i)insert\s+into\s+(\w+\.)?"?changes"?[\s(]`)

// TestOneWayIntoChanges: the only SQL in the module that writes the
// change log is in (*writer).flush, which takes contentLock before the
// rows draw their seq. So every seq is drawn under the lock and the log
// commits in seq order, which is what makes changes.seq a cursor (§8.6);
// a new writer that inserts on its own fails here.
func TestOneWayIntoChanges(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var inserts []string
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// As in TestOneWayToTheOutbox: what ./... leaves out, so
			// worktrees under .claude/ are not taken for this module.
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
				name == "testdata" || name == "node_modules" || name == "local") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, decl := range f.Decls {
			where := rel + ": package level"
			if fd, ok := decl.(*ast.FuncDecl); ok {
				where = rel + ": " + funcName(fd)
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				if x, ok := n.(*ast.BasicLit); ok && x.Kind == token.STRING && insertChanges.MatchString(x.Value) {
					inserts = append(inserts, where)
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"internal/store/content.go: (*writer).flush"}; !slices.Equal(inserts, want) {
		t.Errorf("INSERT INTO changes: %q, want %q", inserts, want)
	}
}

// drain follows the log from since to its end as v.
func drain(t *testing.T, db *store.DB, v *auth.Viewer, since int64, q store.ChangeQuery) ([]store.Change, int64) {
	t.Helper()
	var out []store.Change
	for {
		q.Since = &since
		p, err := db.Changes(t.Context(), v, q)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p.Changes...)
		since = p.Cursor
		if !p.More {
			return out, since
		}
	}
}

// A writer that drew its seq but has not committed holds back every
// later one: a reader never sees seq N+1 while N can still commit, so it
// never moves its cursor past N. HoldChange logs without taking the lock
// itself, as a careless writer would: flush takes it.
func TestChangesFollowCommitOrder(t *testing.T) {
	t.Parallel()
	e := newSendEnv(t)
	ctx := t.Context()
	start, err := e.db.Changes(ctx, e.op, store.ChangeQuery{})
	if err != nil {
		t.Fatal(err)
	}
	commit, err := e.db.HoldChange(ctx, "prefix", "held@example.com")
	if err != nil {
		t.Fatal(err)
	}
	// On failure the pool must not wait for the open transaction.
	t.Cleanup(func() { _ = commit() })
	done := make(chan error, 1)
	go func() {
		now := time.Now()
		_, err := e.db.SetPrefixState(ctx, "example.com", "later", store.PrefixClaimed, store.Action{
			ID: ulid.New(now), Actor: store.SystemPrincipalID, Tool: "prefix.test", At: now,
		})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("a later writer committed before the held one: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if got, _ := drain(t, e.db, e.op, start.Cursor, store.ChangeQuery{}); len(got) != 0 {
		t.Fatalf("uncommitted changes listed: %+v", got)
	}
	if err := commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, _ := drain(t, e.db, e.op, start.Cursor, store.ChangeQuery{})
	if len(got) != 2 || got[0].ID != "held@example.com" || got[1].ID != "later@example.com" || got[0].Seq >= got[1].Seq {
		t.Errorf("changes %+v, want held then later", got)
	}
}

// Readers following the log while writers commit concurrently miss
// nothing and see each change once, in seq order.
func TestChangesUnderConcurrentWriters(t *testing.T) {
	t.Parallel()
	e := newSendEnv(t)
	ctx := t.Context()
	start, err := e.db.Changes(ctx, e.op, store.ChangeQuery{})
	if err != nil {
		t.Fatal(err)
	}
	const writers, each = 6, 15
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := range writers {
		wg.Go(func() {
			for i := range each {
				now := time.Now()
				if i%2 == 0 {
					_, err := e.db.SetPrefixState(ctx, "example.com", fmt.Sprintf("w%d-%d", w, i), store.PrefixClaimed, store.Action{
						ID: ulid.New(now), Actor: store.SystemPrincipalID, Tool: "prefix.test", At: now,
					})
					errs <- err
					continue
				}
				_, err := e.db.CreateDraft(ctx, e.arisu, store.Draft{FromAddr: "arisu@example.com", Subject: "x"}, now)
				errs <- err
			}
		})
	}
	stop := make(chan struct{})
	var seen []store.Change
	reader := make(chan struct{})
	go func() {
		defer close(reader)
		since := start.Cursor
		for {
			select {
			case <-stop:
				return
			default:
			}
			p, err := e.db.Changes(ctx, e.op, store.ChangeQuery{Since: &since, Limit: 7})
			if err != nil {
				t.Error(err)
				return
			}
			seen = append(seen, p.Changes...)
			since = p.Cursor
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	<-reader
	last := start.Cursor
	if len(seen) > 0 {
		last = seen[len(seen)-1].Seq
	}
	rest, _ := drain(t, e.db, e.op, last, store.ChangeQuery{})
	seen = append(seen, rest...)

	all, err := e.pool.Query(ctx, `SELECT seq FROM changes WHERE seq > $1 ORDER BY seq`, start.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	var want []int64
	var seq int64
	for all.Next() {
		if err := all.Scan(&seq); err != nil {
			t.Fatal(err)
		}
		want = append(want, seq)
	}
	if err := all.Err(); err != nil {
		t.Fatal(err)
	}
	got := make([]int64, len(seen))
	for i, c := range seen {
		got[i] = c.Seq
	}
	// Each writer logs one change per call, so nothing collapses.
	if len(want) != writers*each || !slices.Equal(got, want) {
		t.Errorf("followed %d changes %v, the log has %d %v", len(got), got, len(want), want)
	}
}

func TestChanges(t *testing.T) {
	t.Parallel()
	e := newSendEnv(t)
	ctx := t.Context()
	page := func(v *auth.Viewer, q store.ChangeQuery) store.ChangePage {
		t.Helper()
		p, err := e.db.Changes(ctx, v, q)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	start := page(e.op, store.ChangeQuery{})
	if len(start.Changes) != 0 || start.More {
		t.Fatalf("from now: %+v", start)
	}

	m := e.deliver(bobMail, "github")
	var delivery string
	if err := e.pool.QueryRow(ctx, `SELECT id FROM deliveries WHERE message_id = $1`, m).Scan(&delivery); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, l := range []string{"one", "two"} {
		if _, _, err := e.db.Annotate(ctx, e.op, m, []store.Annotation{store.SetLabel(l, true)},
			store.Action{ID: ulid.New(now), Actor: e.op.PrincipalID, Tool: "message.annotate", At: now}); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Millisecond)
	}
	since := start.Cursor
	all := page(e.op, store.ChangeQuery{Since: &since})
	kinds := map[string]string{}
	for _, c := range all.Changes {
		kinds[c.Kind] = c.ID
		if c.Kind == "delivery" && c.MessageID != m || c.Kind != "delivery" && c.MessageID != "" {
			t.Errorf("%s %s: message %q", c.Kind, c.ID, c.MessageID)
		}
	}
	if kinds["message"] != m || kinds["delivery"] != delivery || kinds["flag"] != m || kinds["thread"] == "" ||
		kinds["prefix"] != "github@example.com" || all.More {
		t.Errorf("after one delivery and two labels: %+v", all)
	}
	// Two annotations, one flag change in the page: each object once,
	// at its last change.
	flags := 0
	for _, c := range all.Changes {
		if c.Kind == "flag" {
			flags++
			if c.Seq != all.Cursor {
				t.Errorf("flag at %d, want the last change %d", c.Seq, all.Cursor)
			}
		}
	}
	if flags != 1 {
		t.Errorf("%d flag changes in one page, want 1", flags)
	}

	// Kinds keep only theirs; the cursor moves past the rest.
	flagOnly := page(e.op, store.ChangeQuery{Since: &since, Kinds: []string{"flag"}})
	if len(flagOnly.Changes) != 1 || flagOnly.Changes[0].Kind != "flag" || flagOnly.Cursor != all.Cursor {
		t.Errorf("kinds=flag: %+v", flagOnly)
	}
	if _, err := e.db.Changes(ctx, e.op, store.ChangeQuery{Since: &since, Kinds: []string{"mail"}}); err == nil {
		t.Error("an unknown kind was accepted")
	}

	// Paging: a page of one at a time walks the same log, resuming
	// where the last page ended.
	var paged []string
	cur := since
	for range 20 {
		p := page(e.op, store.ChangeQuery{Since: &cur, Limit: 1})
		if len(p.Changes) > 1 {
			t.Fatalf("limit 1: %+v", p)
		}
		for _, c := range p.Changes {
			paged = append(paged, c.Kind)
		}
		cur = p.Cursor
		if !p.More {
			break
		}
	}
	if cur != all.Cursor || len(paged) != len(all.Changes)+1 { // the first flag change too
		t.Errorf("paged %q to %d, want %d changes to %d", paged, cur, len(all.Changes)+1, all.Cursor)
	}
	end := page(e.op, store.ChangeQuery{Since: &cur})
	if len(end.Changes) != 0 || end.More || end.Cursor != cur {
		t.Errorf("at the end: %+v", end)
	}

	// A cursor this log did not issue: ahead of it, or negative.
	for _, bad := range []int64{all.Cursor + 1, -1} {
		if _, err := e.db.Changes(ctx, e.op, store.ChangeQuery{Since: &bad}); !errors.Is(err, store.ErrCursorExpired) {
			t.Errorf("cursor %d: %v, want ErrCursorExpired", bad, err)
		}
	}
	if _, err := e.db.Changes(ctx, nil, store.ChangeQuery{}); err == nil {
		t.Error("nil viewer")
	}

	// Rules are a person's; drafts their author's and people's.
	rule, _, err := e.db.CreateRule(ctx, store.NewRule{
		Name: "r", Spec: []byte(`{"v":1,"match":{"field":"list.id","op":"eq","value":"x.example.net"},"actions":[{"kind":"label","label":"x"}]}`),
	}, store.Action{ID: ulid.New(now), Actor: e.op.PrincipalID, Tool: "rule.create", At: now})
	if err != nil {
		t.Fatal(err)
	}
	d, err := e.db.CreateDraft(ctx, e.arisu, store.Draft{FromAddr: "arisu@example.com"}, e.at())
	if err != nil {
		t.Fatal(err)
	}
	momoi := e.principal("agent", "momoi", auth.MessageRead)
	for v, want := range map[*auth.Viewer][]string{
		e.op:    {"rule " + rule.ID, "draft " + d.ID},
		e.arisu: {"draft " + d.ID},
		momoi:   {},
	} {
		var got []string
		for _, c := range page(v, store.ChangeQuery{Since: &cur, Kinds: []string{"rule", "draft"}}).Changes {
			got = append(got, c.Kind+" "+c.ID)
		}
		if !slices.Equal(got, want) && len(got)+len(want) > 0 {
			t.Errorf("%s: %q, want %q", v, got, want)
		}
	}
}
