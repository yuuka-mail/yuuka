package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/openapi"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/submit"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

func (h *harness) post(path, body, bearer string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(http.MethodPost, path, body, bearer)
}

// An agent drafts a reply in the operator's thread: the From defaults
// to the address the mail came in through, which is the operator's, so
// submitting it is held by the owner cap (§8.9) and that is a success.
// The operator then confirms it, and it is queued.
func TestSubmitHeldThenConfirmed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	agent := h.token(auth.MessageRead, auth.DraftCreate, auth.SubmissionSend)
	msg := h.find("Re: Lunch on Friday?")

	w := h.post("/api/v1/drafts", `{"in_reply_to":"`+msg+`","to":["alice@example.org"],"subject":"Re: Lunch",`+
		`"body_markdown":"See you **there**.","cites":[{"uri":"yuuka://message/`+msg+`#part/1/para/1","quote":"usual place"}]}`, agent)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	d := decode[Draft](t, w)
	if d.State != "draft" || !strings.HasSuffix(d.From, "@example.com") || d.Author.Name != "arisu" || len(d.Cites) != 1 ||
		d.InReplyTo != msg || d.BodyMarkdown != "See you **there**." {
		t.Errorf("created %+v", d)
	}
	// The draft names whose address it is, so a composer can say who may
	// confirm it whatever /identities lists for the viewer.
	if o := d.IdentityOwner; o.ID != h.human.ID || o.Kind != "human" || o.Name != "operator" {
		t.Errorf("created draft's identity owner: %+v", o)
	}
	// What a composer shows before sending: as this address the agent
	// is capped at drafts.
	ri := decode[ReplyIdentities](t, h.get("/api/v1/messages/"+msg+"/reply-identities", agent))
	if len(ri.Candidates) == 0 || ri.Candidates[0].From != d.From || ri.Candidates[0].Identity.Sends ||
		ri.Candidates[0].Identity.HoldReason != "owner" || !ri.Candidates[0].Identity.OwnerCapped {
		t.Errorf("reply identities: %+v", ri)
	}

	w = h.post("/api/v1/drafts/"+d.ID+"/submit", "", agent)
	res := decode[SubmitResult](t, w)
	if w.Code != http.StatusOK || res.Queued || res.HoldReason != "owner" || res.HoldDetail == "" || res.DraftID != d.ID || res.OutboxID != "" {
		t.Fatalf("agent submits as the operator: %d %+v", w.Code, res)
	}
	held := decode[Draft](t, h.get("/api/v1/drafts/"+d.ID, agent))
	if held.State != "held" || held.HoldReason != "owner" || held.Outbox != nil || held.IdentityOwner != d.IdentityOwner {
		t.Errorf("held draft: %+v", held)
	}
	list := decode[DraftPage](t, h.get("/api/v1/drafts?state=held", agent))
	if len(list.Drafts) != 1 || list.Drafts[0].ID != d.ID {
		t.Errorf("held list: %+v", list)
	}

	// The operator presses send on the agent's draft: a person's
	// decision, so it is queued, approved by them.
	op := h.tokenFor(h.human, auth.AllScopes...)
	w = h.post("/api/v1/drafts/"+d.ID+"/submit", "", op)
	res = decode[SubmitResult](t, w)
	if w.Code != http.StatusOK || !res.Queued || res.OutboxID == "" || res.HoldReason != "" {
		t.Fatalf("operator confirms: %d %+v", w.Code, res)
	}
	sent := decode[Draft](t, h.get("/api/v1/drafts/"+d.ID+"", agent))
	if sent.State != "outbox" || sent.Outbox == nil || sent.Outbox.OutboxID != res.OutboxID || sent.Outbox.State != "queued" {
		t.Errorf("sent draft: %+v", sent)
	}
	e := decode[OutboxEntry](t, h.get("/api/v1/outbox/"+res.OutboxID, op))
	if e.State != "queued" || e.DraftID != d.ID || e.SubmittedBy.Name != "arisu" || e.ApprovedBy == nil ||
		e.ApprovedBy.Name != "operator" || len(e.Recipients) != 1 || e.Recipients[0].State != "pending" || e.MessageID == "" {
		t.Errorf("outbox entry: %+v", e)
	}
	// The sent copy is a message that says it is ours and how far it got.
	m := decode[Message](t, h.get("/api/v1/messages/"+e.MessageID, agent))
	if m.Outgoing == nil || m.Outgoing.OutboxID != e.ID || m.Outgoing.State != "queued" {
		t.Errorf("sent copy: %+v", m.Outgoing)
	}

	// Pressing send twice, or editing what went out, is a conflict.
	for _, w := range []*httptest.ResponseRecorder{
		h.post("/api/v1/drafts/"+d.ID+"/submit", "", op),
		h.do(http.MethodPut, "/api/v1/drafts/"+d.ID, `{"subject":"x"}`, agent),
		h.do(http.MethodDelete, "/api/v1/drafts/"+d.ID, "", agent),
	} {
		if w.Code != http.StatusConflict || problemOf(t, w).Code != openapi.CodeConflict {
			t.Errorf("in the outbox: %d %s", w.Code, w.Body.String())
		}
	}
}

// The operator writes from their own address: queued at once.
func TestSubmitQueued(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	op := h.tokenFor(h.human, auth.AllScopes...)
	ids := decode[IdentityList](t, h.get("/api/v1/identities", op))
	if len(ids.Identities) != 1 || ids.Identities[0].Address != "*@example.com" || ids.Identities[0].Tier != "auto" ||
		!ids.Identities[0].Sends || ids.Identities[0].Owner.Name != "operator" {
		t.Errorf("operator's identities: %+v", ids)
	}
	// A recipient is a bare address or {name, addr}, and reads back as
	// {name, addr} either way.
	w := h.post("/api/v1/drafts", `{"from":"Me@Example.com","to":[{"name":"Bob, Jr.","addr":"bob@example.net"}],`+
		`"bcc":["carol@example.org"],"subject":"Hi","body_markdown":"Hello"}`, op)
	d := decode[Draft](t, w)
	if w.Code != http.StatusCreated || d.From != "me@example.com" || d.Bcc[0] != (agentview.Address{Addr: "carol@example.org"}) ||
		d.To[0] != (agentview.Address{Name: "Bob, Jr.", Addr: "bob@example.net"}) {
		t.Fatalf("create: %d %+v", w.Code, d)
	}
	res := decode[SubmitResult](t, h.post("/api/v1/drafts/"+d.ID+"/submit", "", op))
	if !res.Queued || res.OutboxID == "" {
		t.Fatalf("submit: %+v", res)
	}
	page := decode[OutboxPage](t, h.get("/api/v1/outbox?state=queued", op))
	if len(page.Entries) != 1 || page.Entries[0].ID != res.OutboxID || len(page.Entries[0].Recipients) != 2 ||
		page.Entries[0].Recipients[1].Kind != "bcc" || page.Entries[0].ApprovedBy.Name != "operator" {
		t.Errorf("outbox: %+v", page)
	}
	if page := decode[OutboxPage](t, h.get("/api/v1/outbox?state=sent,dead", op)); len(page.Entries) != 0 {
		t.Errorf("outbox state=sent,dead: %+v", page)
	}
	// The outbox holds Bcc recipients: delivery data.
	if w := h.get("/api/v1/outbox", h.token(auth.MessageRead)); w.Code != http.StatusForbidden {
		t.Errorf("outbox without delivery:read: %d", w.Code)
	}
}

// As itself the agent defaults to review + thread (§13.5), and M2 has no
// automatic review: a reply to the thread is held for review, a new
// thread for its recipients. Without submission:send, held for policy.
// None of them is an error.
func TestSubmitHeldForReviewAndPolicy(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	agent := h.token(auth.MessageRead, auth.DraftCreate, auth.SubmissionSend)
	ids := decode[IdentityList](t, h.get("/api/v1/identities", agent))
	got := map[string]string{}
	for _, i := range ids.Identities {
		got[i.Address] = fmt.Sprintf("%s/%s/%v", i.Tier, i.HoldReason, i.OwnerCapped)
	}
	if fmt.Sprint(got) != "map[*@example.com:draft/owner/true arisu@example.com:review/review/false]" {
		t.Errorf("agent's identities: %v", got)
	}
	msg := decode[Message](t, h.get("/api/v1/messages/"+h.find("Re: Lunch on Friday?"), agent))
	reply := `{"from":"arisu@example.com","to":["` + msg.FromAddr + `"],"in_reply_to":"` + msg.ID + `"}`
	for _, c := range []struct {
		tok, body string
		want      HoldReason
	}{
		{agent, reply, "review"},
		{agent, `{"from":"arisu@example.com","to":["bob@example.net"]}`, "recipients"},
		{h.token(auth.MessageRead, auth.DraftCreate), reply, "policy"},
	} {
		d := decode[Draft](t, h.post("/api/v1/drafts", c.body, c.tok))
		w := h.post("/api/v1/drafts/"+d.ID+"/submit", "", c.tok)
		if res := decode[SubmitResult](t, w); w.Code != http.StatusOK || res.Queued || res.HoldReason != c.want {
			t.Errorf("submit, want held for %s: %d %+v", c.want, w.Code, res)
		}
	}
}

func TestDraftProblems(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	agent := h.token(auth.MessageRead, auth.DraftCreate, auth.SubmissionSend)
	missingCite := ulid.New(time.Now())
	for body, code := range map[string]openapi.Code{
		`{"to":["bob@example.net"]}`:                                                              openapi.CodeInvalidBody, // no from, not a reply
		`{"from":"me@example.org"}`:                                                               openapi.CodeNoIdentity,
		`{"from":"*@example.com"}`:                                                                openapi.CodeInvalidBody,
		`{"from":"arisu@example.com","to":["bob"]}`:                                               openapi.CodeInvalidBody,
		`{"from":"arisu@example.com","to":["bob@example.net\r\nBcc: x@y.z"]}`:                     openapi.CodeInvalidBody,
		`{"from":"arisu@example.com","to":["Bob <bob@example.net>"]}`:                             openapi.CodeInvalidBody, // a string is a bare address
		`{"from":"arisu@example.com","to":[{"name":"Bob"}]}`:                                      openapi.CodeInvalidBody,
		`{"from":"arisu@example.com","to":[{"addr":"bob@example.net","nick":"B"}]}`:               openapi.CodeInvalidBody,
		`{"from":"arisu@example.com","cc":[{"name":"B\r\nBcc: x@y.z","addr":"bob@example.net"}]}`: openapi.CodeInvalidBody,
		`{"from":"arisu@example.com","cites":[{"uri":"https://example.com"}]}`:                    openapi.CodeInvalidBody,
		`{"from":"arisu@example.com","in_reply_to":"` + ulid.New(time.Now()) + `"}`:               openapi.CodeInvalidBody,
		`{"in_reply_to":"` + ulid.New(time.Now()) + `"}`:                                          openapi.CodeInvalidBody,
		`{"from":"arisu@example.com","html":"<b>"}`:                                               openapi.CodeInvalidBody,
		// Bounded as a proposal's or a note's cites (agentview.CheckCites).
		`{"from":"arisu@example.com","cites":[` + strings.Repeat(`{"uri":"yuuka://message/`+missingCite+`"},`, agentview.MaxCites) +
			`{"uri":"yuuka://message/` + missingCite + `"}]}`: openapi.CodeInvalidBody,
	} {
		w := h.post("/api/v1/drafts", body, agent)
		if w.Code == http.StatusCreated || problemOf(t, w).Code != code {
			t.Errorf("%s: %d %s, want %s", body, w.Code, w.Body.String(), code)
		}
	}
	missing := ulid.New(time.Now())
	for _, w := range []*httptest.ResponseRecorder{
		h.get("/api/v1/drafts/"+missing, agent),
		h.post("/api/v1/drafts/"+missing+"/submit", "", agent),
		h.do(http.MethodPut, "/api/v1/drafts/"+missing, `{"from":"arisu@example.com"}`, agent),
		h.do(http.MethodDelete, "/api/v1/drafts/"+missing, "", agent),
		h.get("/api/v1/outbox/"+missing, h.token(auth.MessageRead, auth.DeliveryRead)),
	} {
		if w.Code != http.StatusNotFound || problemOf(t, w).Code != openapi.CodeNotFound {
			t.Errorf("missing: %d %s", w.Code, w.Body.String())
		}
	}
	for _, bad := range []string{"state=sent", "cursor=x", "limit=0"} {
		if w := h.get("/api/v1/drafts?"+bad, agent); w.Code != http.StatusBadRequest {
			t.Errorf("drafts?%s: %d", bad, w.Code)
		}
	}

	// Update replaces every field; an absent from keeps the draft's.
	d := decode[Draft](t, h.post("/api/v1/drafts", `{"from":"arisu@example.com","to":["bob@example.net"],"subject":"One"}`, agent))
	if w := h.do(http.MethodPut, "/api/v1/drafts/"+d.ID, `{"cites":[{"uri":"yuuka://message/`+missingCite+`","quote":"`+
		strings.Repeat("q", agentview.MaxQuoteChars+1)+`"}]}`, agent); w.Code != http.StatusBadRequest || problemOf(t, w).Code != openapi.CodeInvalidBody {
		t.Errorf("update with a long quote: %d %s", w.Code, w.Body.String())
	}
	w := h.do(http.MethodPut, "/api/v1/drafts/"+d.ID, `{"cc":["carol@example.org"],"subject":"Two"}`, agent)
	u := decode[Draft](t, w)
	if w.Code != http.StatusOK || u.From != "arisu@example.com" || len(u.To) != 0 || u.Cc[0].Addr != "carol@example.org" || u.Subject != "Two" {
		t.Errorf("update: %d %+v", w.Code, u)
	}
	if w := h.do(http.MethodDelete, "/api/v1/drafts/"+d.ID, "", agent); w.Code != http.StatusNoContent {
		t.Errorf("delete: %d %s", w.Code, w.Body.String())
	}
}

// The draft list pages by cursor, most recently changed first.
func TestDraftPaging(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	agent := h.token(auth.MessageRead, auth.DraftCreate)
	var made []string
	for i := range 5 {
		d := decode[Draft](t, h.post("/api/v1/drafts", fmt.Sprintf(`{"from":"arisu@example.com","subject":"%d"}`, i), agent))
		made = append([]string{d.ID}, made...)
	}
	var ids []string
	next := "/api/v1/drafts?limit=2"
	for next != "" {
		p := decode[DraftPage](t, h.get(next, agent))
		for _, d := range p.Drafts {
			ids = append(ids, d.ID)
		}
		next = ""
		if p.NextCursor != "" {
			next = "/api/v1/drafts?limit=2&cursor=" + p.NextCursor
		}
	}
	if !slices.Equal(ids, made) {
		t.Errorf("paged %v, want %v", ids, made)
	}
}

// Resolving unknown is a person's decision, recorded like the CLI's.
func TestResolveOutbox(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id := h.unknown()
	body := `{"resolution":"sent"}`
	agent := h.token(auth.AllScopes...)
	if w := h.post("/api/v1/outbox/"+id+"/resolve", body, agent); w.Code != http.StatusForbidden || problemOf(t, w).Code != openapi.CodeForbidden {
		t.Errorf("an agent resolves: %d %s", w.Code, w.Body.String())
	}
	op := h.tokenFor(h.human, auth.AllScopes...)
	for b, code := range map[string]int{`{}`: http.StatusBadRequest, `{"resolution":"maybe"}`: http.StatusBadRequest} {
		if w := h.post("/api/v1/outbox/"+id+"/resolve", b, op); w.Code != code {
			t.Errorf("%s: %d", b, w.Code)
		}
	}
	w := h.post("/api/v1/outbox/"+id+"/resolve", body, op)
	if e := decode[OutboxEntry](t, w); w.Code != http.StatusOK || e.State != "sent" || e.Recipients[0].State != "accepted" {
		t.Errorf("resolve: %d %+v", w.Code, e)
	}
	if w := h.post("/api/v1/outbox/"+id+"/resolve", body, op); w.Code != http.StatusConflict || problemOf(t, w).Code != openapi.CodeConflict {
		t.Errorf("resolve twice: %d %s", w.Code, w.Body.String())
	}
	var actor string
	var args map[string]any
	if err := h.pool.QueryRow(t.Context(), `SELECT actor, args FROM actions WHERE tool = 'outbox.resolve'`).Scan(&actor, &args); err != nil ||
		actor != h.human.ID || args["resolution"] != "sent" || args["outbox_id"] != id || args["via"] != "api" {
		t.Errorf("recorded %s %v %v", actor, args, err)
	}
}

type rejecting struct{ err error }

func (r rejecting) SubmitDraft(context.Context, *auth.Viewer, string) (submit.Result, error) {
	return submit.Result{}, r.err
}

// The core's refusals map to stable codes; without a core there is no
// submit route at all.
func TestSubmitRefusals(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := h.draft(h.agent, store.Draft{FromAddr: "arisu@example.com"})
	tok := h.token(auth.DraftCreate)
	for sub, want := range map[Submitter]struct {
		status int
		code   openapi.Code
	}{
		rejecting{fmt.Errorf("x: %w", submit.ErrRejected)}:   {http.StatusForbidden, openapi.CodeSubmissionRejected},
		rejecting{fmt.Errorf("x: %w", submit.ErrNoIdentity)}: {http.StatusUnprocessableEntity, openapi.CodeNoIdentity},
	} {
		h.h = New(h.db, nil, quiet).WithSubmitter(sub).Handler(h.svc)
		if w := h.post("/api/v1/drafts/"+d.ID+"/submit", "", tok); w.Code != want.status || problemOf(t, w).Code != want.code {
			t.Errorf("%v: %d %s", sub, w.Code, w.Body.String())
		}
	}
	h.h = New(h.db, nil, quiet).Handler(h.svc)
	if w := h.post("/api/v1/drafts/"+d.ID+"/submit", "", tok); w.Code != http.StatusNotFound || problemOf(t, w).Code != openapi.CodeNotFound {
		t.Errorf("no core: %d %s", w.Code, w.Body.String())
	}
}
