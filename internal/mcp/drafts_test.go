package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/ingest"
	"github.com/yuuka-mail/yuuka/internal/localraw"
	"github.com/yuuka-mail/yuuka/internal/mcp"
	"github.com/yuuka-mail/yuuka/internal/outbox"
	"github.com/yuuka-mail/yuuka/internal/r2/r2fake"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/submit"
	"github.com/yuuka-mail/yuuka/internal/transport"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// fakeTransport only answers Caps: nothing here is ever sent.
type fakeTransport struct{}

func (fakeTransport) Caps() transport.Caps { return transport.CloudflareCaps() }

func (fakeTransport) Send(context.Context, transport.Envelope, []byte, func(context.Context) error) (*transport.SendResult, error) {
	return nil, errors.New("not in these tests")
}

// sending gives the env §13.5's identities (the operator owns
// *@example.com, where the corpus arrived; arisu owns arisu@) and a
// server that submits through the real core.
func (e *env) sending() {
	e.t.Helper()
	ctx := e.t.Context()
	op, err := e.db.EnsurePrincipal(ctx, store.Principal{ID: ulid.New(time.Now()), Kind: "human", Name: "operator", DisplayName: "operator", CreatedAt: time.Now()})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, i := range []struct{ addr, owner string }{{"*@example.com", op.ID}, {"arisu@example.com", e.agent.ID}} {
		zone, prefix, _ := store.SplitIdentityAddress(i.addr)
		at := time.Now()
		if err := e.db.AddIdentity(ctx, store.Identity{
			ID: ulid.New(at), AccountID: storetest.Account(e.t, e.db), Zone: zone, Prefix: prefix, Transport: "cf", OwnerID: i.owner, CreatedAt: at,
		}, store.Action{ID: ulid.New(at), Actor: store.SystemPrincipalID, Tool: "identity.add", At: at}); err != nil {
			e.t.Fatal(err)
		}
	}
	local, bucket := localraw.Dir{Root: e.t.TempDir()}, r2fake.New()
	in := ingest.New(ingest.Config{Raw: bucket, Local: local, DB: e.db, Log: quiet})
	out := outbox.New(outbox.Config{
		DB: e.db, Raw: bucket, Local: local, Transports: map[string]outbox.Transport{"cf": fakeTransport{}},
		IndexSent: in.IndexSent, Log: quiet,
	})
	e.srv = mcp.New(mcp.Config{DB: e.db, Log: quiet, Version: "test", Submitter: submit.New(submit.Config{DB: e.db, Outbox: out, Log: quiet})})
}

func structured[T any](t *testing.T, c call) T {
	t.Helper()
	var v T
	if c.isError || json.Unmarshal(c.structured, &v) != nil {
		t.Fatalf("result: %+v", c)
	}
	return v
}

// An agent drafts a reply in the operator's mail: From defaults to the
// operator's address, so submitting it is held by the owner cap, and the
// result says so without being an error. As itself, replying in the
// thread, it is held for review (M2 has none automatic).
func TestDraftTools(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.sending()
	cs := e.client(e.viewer(auth.MessageRead, auth.DraftCreate, auth.SubmissionSend))

	ids := structured[mcp.IdentitiesOutput](t, callTool(t, cs, "list_identities", nil))
	if len(ids.Identities) != 2 || ids.Identities[0].Address != "*@example.com" || ids.Identities[0].Sends ||
		ids.Identities[0].HoldReason != "owner" || !ids.Identities[1].Own || ids.Identities[1].Tier != "review" {
		t.Errorf("identities: %+v", ids)
	}

	msgID := e.ids["alternative-reply"]
	uri := agentview.MessageURI(msgID)
	r := callTool(t, cs, "create_draft", map[string]any{
		"in_reply_to": uri, "to": []string{"someone@example.net"}, "subject": "Re: hi",
		"body_markdown": "SECRET-BODY", "cites": []map[string]any{{"uri": uri + "#part/1/para/1", "quote": "hello"}},
	})
	d := structured[mcp.DraftOutput](t, r)
	if d.From != "a@example.com" || d.Sends || d.HoldReason != "owner" || d.InReplyTo != uri ||
		!strings.Contains(r.text, "Nothing is sent") || !strings.Contains(r.text, fenceTag) {
		t.Errorf("create_draft: %+v\n%s", d, r.text)
	}

	r = callTool(t, cs, "submit_draft", map[string]any{"draft_id": d.DraftID})
	res := structured[mcp.SubmitOutput](t, r)
	if res.Queued || res.HoldReason != "owner" || res.HoldDetail == "" || !strings.Contains(r.text, "not an error") {
		t.Errorf("submit as the operator: %+v\n%s", res, r.text)
	}

	// As itself, to the thread's sender: held for review.
	m, err := e.db.MessageByID(t.Context(), e.viewer(auth.MessageRead), msgID)
	if err != nil {
		t.Fatal(err)
	}
	r = callTool(t, cs, "update_draft", map[string]any{
		"draft_id": d.DraftID, "from": "arisu@example.com", "to": []string{m.FromAddr}, "in_reply_to": uri, "body_markdown": "x",
	})
	if u := structured[mcp.DraftOutput](t, r); u.From != "arisu@example.com" || u.HoldReason != "review" || u.Subject != "" {
		t.Errorf("update_draft: %+v", u)
	}
	res = structured[mcp.SubmitOutput](t, callTool(t, cs, "submit_draft", map[string]any{"draft_id": d.DraftID}))
	if res.Queued || res.HoldReason != "review" {
		t.Errorf("submit as itself: %+v", res)
	}

	// The audit log names the draft, never its text.
	var args string
	if err := e.pool.QueryRow(t.Context(), `SELECT args::text FROM actions WHERE tool = 'mcp.create_draft'`).Scan(&args); err != nil ||
		strings.Contains(args, "SECRET-BODY") || strings.Contains(args, "Re: hi") || !strings.Contains(args, `"recipients": 1`) {
		t.Errorf("audit args %s %v", args, err)
	}

	// Another agent cannot tell the draft from a missing one.
	momoi, err := e.db.EnsurePrincipal(t.Context(), store.Principal{ID: ulid.New(time.Now()), Kind: "agent", Name: "momoi", DisplayName: "momoi", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	other := e.client(&auth.Viewer{PrincipalID: momoi.ID, PrincipalKind: "agent", PrincipalName: "momoi", Scopes: auth.AllScopes})
	for tool, args := range map[string]map[string]any{
		"update_draft": {"draft_id": d.DraftID, "from": "arisu@example.com"},
		"submit_draft": {"draft_id": d.DraftID},
	} {
		r := callTool(t, other, tool, args)
		if !r.isError || !strings.Contains(r.text, "no such draft") {
			t.Errorf("%s by another agent: %+v", tool, r)
		}
	}

	for args, want := range map[string]string{
		`{"to":["x@example.net"]}`:                                   "from is required",
		`{"from":"me@example.org"}`:                                  "no identity covers",
		`{"from":"arisu@example.com","to":["bob"]}`:                  "want a bare address",
		`{"from":"arisu@example.com","in_reply_to":"not-a-uri"}`:     "in_reply_to",
		`{"from":"arisu@example.com","cites":[{"uri":"https://x"}]}`: "cites",
	} {
		var a map[string]any
		if err := json.Unmarshal([]byte(args), &a); err != nil {
			t.Fatal(err)
		}
		if r := callTool(t, cs, "create_draft", a); !r.isError || !strings.Contains(r.text, want) {
			t.Errorf("%s: %+v, want %q", args, r, want)
		}
	}

	// draft:create is the scope of every write tool.
	readOnly := e.client(e.viewer(auth.MessageRead, auth.SubmissionSend))
	for tool, args := range map[string]map[string]any{
		"list_identities": {},
		"create_draft":    {"from": "arisu@example.com"},
		"update_draft":    {"draft_id": d.DraftID},
		"submit_draft":    {"draft_id": d.DraftID},
	} {
		if r := callTool(t, readOnly, tool, args); !r.isError ||
			!strings.Contains(r.text, "draft:create") {
			t.Errorf("%s without draft:create: %+v", tool, r)
		}
	}
}

// The operator's own submission through the same tool is queued.
func TestSubmitDraftQueued(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.sending()
	op, err := e.db.PrincipalByName(t.Context(), "human", "operator")
	if err != nil {
		t.Fatal(err)
	}
	cs := e.client(&auth.Viewer{PrincipalID: op.ID, PrincipalKind: "human", PrincipalName: "operator", Scopes: auth.AllScopes})
	d := structured[mcp.DraftOutput](t, callTool(t, cs, "create_draft", map[string]any{"from": "me@example.com", "to": []string{"bob@example.net"}}))
	if !d.Sends {
		t.Errorf("operator's draft: %+v", d)
	}
	r := callTool(t, cs, "submit_draft", map[string]any{"draft_id": d.DraftID})
	if res := structured[mcp.SubmitOutput](t, r); !res.Queued || res.OutboxID == "" || !strings.Contains(r.text, res.OutboxID) {
		t.Errorf("submit: %+v", res)
	}
	if r := callTool(t, cs, "submit_draft", map[string]any{"draft_id": d.DraftID}); !r.isError || !strings.Contains(r.text, "already gone") {
		t.Errorf("submit twice: %+v", r)
	}
}
