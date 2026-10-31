package mcp_test

import (
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/mcp"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// An agent starts from now, sees a person's change to old mail as a
// kind and a URI, never content, and resumes from the cursor it got.
func TestChangesSince(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	cs := e.client(e.viewer(auth.MessageRead))

	start := structured[mcp.ChangesOutput](t, callTool(t, cs, "changes_since", nil))
	if len(start.Changes) != 0 || start.Cursor == "" || start.More {
		t.Fatalf("from now: %+v", start)
	}
	quiet := callTool(t, cs, "changes_since", map[string]any{"cursor": start.Cursor})
	if out := structured[mcp.ChangesOutput](t, quiet); len(out.Changes) != 0 || out.Cursor != start.Cursor ||
		!strings.Contains(quiet.text, "Nothing changed") {
		t.Errorf("nothing yet: %+v\n%s", out, quiet.text)
	}
	if n := e.count("mcp.changes_since"); n != 0 {
		t.Errorf("%d empty polls recorded", n)
	}

	id := e.ids["plain-otp"]
	person, err := e.db.EnsurePrincipal(ctx, store.Principal{ID: ulid.New(time.Now()), Kind: "human", Name: "operator", DisplayName: "operator", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.db.Annotate(ctx, &auth.Viewer{PrincipalID: person.ID, PrincipalKind: "human", PrincipalName: "operator"}, id,
		[]store.Annotation{store.SetLabel("later", true)},
		store.Action{ID: ulid.New(time.Now()), Actor: person.ID, Tool: "message.annotate", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	r := callTool(t, cs, "changes_since", map[string]any{"cursor": start.Cursor})
	out := structured[mcp.ChangesOutput](t, r)
	if len(out.Changes) != 1 || out.Changes[0].Kind != "flag" || out.Changes[0].URI != agentview.MessageURI(id) ||
		out.Cursor == start.Cursor || !strings.Contains(r.text, out.Cursor) {
		t.Errorf("after a label: %+v\n%s", out, r.text)
	}
	// Ids, not what the mail says.
	if strings.Contains(r.text, "482913") || strings.Contains(r.text, fenceTag) {
		t.Errorf("content in changes_since:\n%s", r.text)
	}
	if n := e.count("mcp.changes_since"); n != 1 {
		t.Errorf("%d calls recorded, want the one that listed something", n)
	}
	if again := structured[mcp.ChangesOutput](t, callTool(t, cs, "changes_since", map[string]any{"cursor": out.Cursor})); len(again.Changes) != 0 {
		t.Errorf("resumed: %+v", again)
	}

	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"kinds": []string{"delivery"}}, "delivery:read"},
		{map[string]any{"cursor": "nonsense"}, "cursor"},
		{map[string]any{"cursor": agentview.ChangeCursor(1 << 40)}, "cursor_expired"},
	} {
		if r := callTool(t, cs, "changes_since", c.args); !r.isError || !strings.Contains(r.text, c.want) {
			t.Errorf("%v: %+v", c.args, r)
		}
	}
}
