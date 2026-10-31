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

func outcomes(out mcp.AnnotateOutput) []agentview.AnnotateOutcome {
	var os []agentview.AnnotateOutcome
	for _, m := range out.Messages {
		for _, o := range m.Outcomes {
			os = append(os, o.Outcome)
		}
	}
	return os
}

func (e *env) count(tool string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.t.Context(), `SELECT count(*) FROM actions WHERE tool = $1 AND actor = $2`, tool, e.agent.ID).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// The agent annotates; a person overrides; the agent's next try is
// overruled, which the result says is normal; undo takes back what is
// still the agent's. Each call is one audit row, the store's when it
// changed something.
func TestAnnotateTools(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	cs := e.client(e.viewer(auth.MessageAnnotate, auth.MessageRead))
	id := e.ids["plain-otp"]
	uri := agentview.MessageURI(id)

	r := callTool(t, cs, "annotate", map[string]any{"uri": uri, "actions": []map[string]any{
		{"kind": "label", "label": "otp"}, {"kind": "archive"},
	}})
	out := structured[mcp.AnnotateOutput](t, r)
	if out.ActionID == "" || len(out.Messages) != 1 || out.Messages[0].URI != uri ||
		strings.Join(toStrings(outcomes(out)), ",") != "applied,applied" || !strings.Contains(r.text, out.ActionID) {
		t.Fatalf("annotate: %+v\n%s", out, r.text)
	}
	r = callTool(t, cs, "search_messages", map[string]any{"label": "otp"})
	if hits := structured[mcp.SearchOutput](t, r); len(hits.Hits) != 1 || hits.Hits[0].URI != uri ||
		len(hits.Hits[0].Labels) != 1 || !hits.Hits[0].Archived || !strings.Contains(r.text, `label "otp"`) {
		t.Errorf("search by label: %+v\n%s", hits, r.text)
	}

	person, err := e.db.EnsurePrincipal(ctx, store.Principal{ID: ulid.New(time.Now()), Kind: "human", Name: "operator", DisplayName: "operator", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.db.Annotate(ctx, &auth.Viewer{PrincipalID: person.ID, PrincipalKind: "human", PrincipalName: "operator"}, id,
		[]store.Annotation{store.SetArchived(false)},
		store.Action{ID: ulid.New(time.Now()), Actor: person.ID, Tool: "message.annotate", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	r = callTool(t, cs, "annotate", map[string]any{"uri": uri, "actions": []map[string]any{{"kind": "archive"}}})
	again := structured[mcp.AnnotateOutput](t, r)
	if again.ActionID != "" || strings.Join(toStrings(outcomes(again)), ",") != "overruled" ||
		!strings.Contains(r.text, "not an error") {
		t.Errorf("overruled: %+v\n%s", again, r.text)
	}

	u := structured[mcp.UndoOutput](t, callTool(t, cs, "undo", map[string]any{"action_id": out.ActionID}))
	if u.Reverted != 1 || u.Superseded != 1 {
		t.Errorf("undo: %+v", u)
	}
	if r := callTool(t, cs, "undo", map[string]any{"action_id": ulid.New(time.Now())}); !r.isError || !strings.Contains(r.text, "no action") {
		t.Errorf("undo of nothing: %+v", r)
	}
	// annotate: one by the store (it changed something), one by the
	// tool (it did not); undo: the store's, and the refusal.
	if a, u := e.count("mcp.annotate"), e.count("mcp.undo"); a != 2 || u != 2 {
		t.Errorf("audit rows: annotate %d, undo %d", a, u)
	}

	for _, bad := range []map[string]any{
		{"uri": uri, "actions": []map[string]any{}},
		{"uri": uri, "actions": []map[string]any{{"kind": "label", "label": "$Seen"}}},
		{"uri": uri, "actions": []map[string]any{{"kind": "forward"}}},
		{"uri": "https://example.com/x", "actions": []map[string]any{{"kind": "archive"}}},
		{"uri": agentview.MessageURI(ulid.New(time.Now())), "actions": []map[string]any{{"kind": "archive"}}},
	} {
		if r := callTool(t, cs, "annotate", bad); !r.isError {
			t.Errorf("%v: %+v", bad, r)
		}
	}

	// The thread at once.
	thread := callTool(t, cs, "annotate", map[string]any{"uri": uri, "scope": "thread", "actions": []map[string]any{{"kind": "classify", "class": "machine"}}})
	if th := structured[mcp.AnnotateOutput](t, thread); len(th.Messages) < 1 {
		t.Errorf("thread: %+v", th)
	}
}

func toStrings[T ~string](xs []T) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = string(x)
	}
	return out
}

// The agent proposes; the proposal waits for a person; invalid ones say
// where; nothing outbound can be proposed; the agent lists its own.
func TestProposeTools(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cs := e.client(e.viewer(auth.MessageAnnotate, auth.MessageRead))
	uri := agentview.MessageURI(e.ids["plain-otp"])

	r := callTool(t, cs, "propose", map[string]any{
		"kind": "rule", "target": uri, "rationale": "codes from this service",
		"payload": map[string]any{
			"match":   map[string]any{"field": "from.addr", "op": "eq", "value": "noreply@example.net"},
			"actions": []map[string]any{{"kind": "label", "label": "codes"}, {"kind": "notify", "level": "silent"}},
		},
		"cites": []map[string]any{{"uri": uri, "quote": "verification code"}},
	})
	p := structured[mcp.Proposal](t, r)
	if p.State != "pending" || p.Kind != "rule" || p.Target != uri || len(p.Payload.Actions) != 2 ||
		len(p.Warnings) != 1 || !strings.Contains(r.text, "a person accepts or rejects") {
		t.Fatalf("propose: %+v\n%s", p, r.text)
	}
	if r := callTool(t, cs, "propose", map[string]any{
		"kind": "annotate", "target": uri, "rationale": "x",
		"payload": map[string]any{"actions": []map[string]any{{"kind": "label", "label": "codes"}}},
	}); r.isError {
		t.Errorf("annotate proposal: %+v", r)
	}

	for want, args := range map[string]map[string]any{
		"payload.match.value": {"kind": "rule", "rationale": "x", "payload": map[string]any{
			"match": map[string]any{"field": "subject", "op": "eq", "value": 1}, "actions": []map[string]any{{"kind": "archive"}},
		}},
		"payload.actions": {"kind": "annotate", "target": uri, "rationale": "x", "payload": map[string]any{
			"actions": []map[string]any{{"kind": "notify", "level": "normal"}},
		}},
		// Not a value of the input schema at all.
		"kind": {"kind": "rule", "rationale": "x", "payload": map[string]any{
			"match": map[string]any{"field": "to.prefix", "op": "eq", "value": "a"}, "actions": []map[string]any{{"kind": "forward"}},
		}},
		"pending": {"kind": "annotate", "target": uri, "rationale": "x", "payload": map[string]any{
			"actions": []map[string]any{{"kind": "label", "label": "codes"}},
		}},
	} {
		if r := callTool(t, cs, "propose", args); !r.isError || !strings.Contains(r.text, want) {
			t.Errorf("%v: %+v, want %q", args, r, want)
		}
	}

	r = callTool(t, cs, "list_proposals", map[string]any{"state": "pending"})
	list := structured[mcp.ProposalsOutput](t, r)
	if len(list.Proposals) != 2 || !strings.Contains(r.text, fenceTag) {
		t.Errorf("list_proposals: %+v\n%s", list, r.text)
	}
	// Another agent sees none of them (§13.8).
	momoi, err := e.db.EnsurePrincipal(t.Context(), store.Principal{ID: ulid.New(time.Now()), Kind: "agent", Name: "momoi", DisplayName: "momoi", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	other := e.client(&auth.Viewer{PrincipalID: momoi.ID, PrincipalKind: "agent", PrincipalName: "momoi", Scopes: []auth.Scope{auth.MessageRead}})
	if l := structured[mcp.ProposalsOutput](t, callTool(t, other, "list_proposals", nil)); len(l.Proposals) != 0 {
		t.Errorf("another agent lists %+v", l)
	}
}

// The tools say what they cannot do, and there is no tool to manage
// rules: the descriptions are the contract the model reads.
func TestAnnotationToolsDescribeTheirLimits(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cs := e.client(e.viewer(auth.AllScopes...))
	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"annotate":       {"nothing is sent", "overruled", "not an error", "message:annotate"},
		"undo":           {"your own", "message:annotate"},
		"propose":        {"never send", "A person accepts", "not an error", "subject or body", "message:annotate"},
		"list_proposals": {"a person decided", "message:read"},
	}
	for _, tl := range tools.Tools {
		if strings.Contains(tl.Name, "rule") {
			t.Errorf("tool %s: agents get no rule tools", tl.Name)
		}
		for _, w := range want[tl.Name] {
			if !strings.Contains(tl.Description, w) {
				t.Errorf("%s: description lacks %q", tl.Name, w)
			}
		}
		delete(want, tl.Name)
	}
	if len(want) != 0 {
		t.Errorf("missing tools %v", want)
	}

	// Scopes: annotating needs message:annotate.
	ro := e.client(e.viewer(auth.MessageRead))
	for name, args := range map[string]map[string]any{
		"annotate": {"uri": agentview.MessageURI(e.ids["plain-otp"]), "actions": []map[string]any{{"kind": "archive"}}},
		"undo":     {"action_id": ulid.New(time.Now())},
		"propose":  {"kind": "annotate", "rationale": "x", "payload": map[string]any{"actions": []map[string]any{{"kind": "archive"}}}},
	} {
		if r := callTool(t, ro, name, args); !r.isError || !strings.Contains(r.text, "message:annotate") {
			t.Errorf("%s without message:annotate: %+v", name, r)
		}
	}
}
