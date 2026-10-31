package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/openapi"
	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
)

// The review tier over REST (§8.9): mail the checks pass is held with
// its window's end; its submitter or a person may withdraw it while it
// is held, another agent may not, and nobody can once it is released.
func TestWithdraw(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	first := h.held() // the grant and a known recipient
	agent := h.token(auth.MessageRead, auth.DraftCreate, auth.SubmissionSend, auth.DeliveryRead)
	op := h.tokenFor(h.human, auth.AllScopes...)

	var hold *int
	for _, i := range decode[IdentityList](t, h.get("/api/v1/identities", agent)).Identities {
		if i.Address == "arisu@example.com" {
			hold = i.HoldSeconds
		} else if i.HoldSeconds != nil {
			t.Errorf("%s has a recall window: %d", i.Address, *i.HoldSeconds)
		}
	}
	if hold == nil || *hold != sendpolicy.DefaultHoldSeconds {
		t.Errorf("arisu's recall window: %v", hold)
	}

	d := decode[Draft](t, h.post("/api/v1/drafts", `{"from":"arisu@example.com","to":["bob@example.net"],"body_markdown":"Noon."}`, agent))
	before := time.Now()
	w := h.post("/api/v1/drafts/"+d.ID+"/submit", "", agent)
	res := decode[SubmitResult](t, w)
	if w.Code != http.StatusOK || res.State != "held" || res.Queued || res.OutboxID == "" || res.HoldReason != "review" ||
		res.HoldUntil == nil || res.HoldUntil.Sub(before) < 9*time.Minute || res.HoldUntil.Sub(before) > 11*time.Minute {
		t.Fatalf("submit: %d %+v", w.Code, res)
	}
	e := decode[OutboxEntry](t, h.get("/api/v1/outbox/"+res.OutboxID, agent))
	if e.State != "held" || e.HoldUntil == nil || !e.HoldUntil.Equal(*res.HoldUntil) || e.ApprovedBy != nil || e.WithdrawnBy != nil ||
		e.Review == nil || len(e.Review.Checks) != 5 || slices.ContainsFunc(e.Review.Checks, func(c ReviewCheck) bool { return !c.Pass }) {
		t.Errorf("held entry: %+v", e)
	}

	// Another agent sees the row and may not recall it.
	momoi := h.tokenFor(principalNamed(t, h, "agent", "momoi"), auth.AllScopes...)
	if w := h.post("/api/v1/outbox/"+res.OutboxID+"/withdraw", "", momoi); w.Code != http.StatusForbidden ||
		problemOf(t, w).Code != openapi.CodeForbidden {
		t.Errorf("another agent: %d %s", w.Code, w.Body.String())
	}

	// arisu recalls its own: back to the draft, editable again.
	w = h.post("/api/v1/outbox/"+res.OutboxID+"/withdraw", "", h.token(auth.SubmissionSend))
	wd := decode[Withdrawal](t, w)
	if w.Code != http.StatusOK || wd.OutboxID != res.OutboxID || wd.DraftID != d.ID || wd.Suspended != nil {
		t.Fatalf("own recall: %d %s", w.Code, w.Body.String())
	}
	e = decode[OutboxEntry](t, h.get("/api/v1/outbox/"+res.OutboxID, op))
	if e.State != "withdrawn" || e.WithdrawnBy == nil || e.WithdrawnBy.Name != "arisu" {
		t.Errorf("withdrawn entry: %+v", e)
	}
	if back := decode[Draft](t, h.get("/api/v1/drafts/"+d.ID, agent)); back.State != "held" || back.HoldReason != "withdrawn" || back.Review != nil {
		t.Errorf("draft after recall: %+v", back)
	}
	if w := h.post("/api/v1/outbox/"+res.OutboxID+"/withdraw", "", op); w.Code != http.StatusConflict ||
		!strings.Contains(problemOf(t, w).Detail, "already withdrawn") {
		t.Errorf("recalled twice: %d %s", w.Code, w.Body.String())
	}

	// A person recalls the agent's mail.
	if w := h.post("/api/v1/outbox/"+first+"/withdraw", "", op); w.Code != http.StatusOK || decode[Withdrawal](t, w).Suspended != nil {
		t.Errorf("person's recall: %d %s", w.Code, w.Body.String())
	}

	// Released, it is too late: queued mail may be on the wire.
	late := h.held()
	if _, err := h.pool.Exec(t.Context(), `UPDATE outbox SET hold_until = now() - interval '1 second' WHERE id = $1`, late); err != nil {
		t.Fatal(err)
	}
	if out, _, err := h.db.PromoteHeld(t.Context()); err != nil || len(out) != 1 || out[0].State != "queued" {
		t.Fatalf("promote: %+v %v", out, err)
	}
	if w := h.post("/api/v1/outbox/"+late+"/withdraw", "", op); w.Code != http.StatusConflict ||
		!strings.Contains(problemOf(t, w).Detail, "already released") {
		t.Errorf("after release: %d %s", w.Code, w.Body.String())
	}
	if w := h.post("/api/v1/outbox/"+h.queued()+"/withdraw", "", op); w.Code != http.StatusConflict {
		t.Errorf("queued mail: %d %s", w.Code, w.Body.String())
	}

	if n := h.count(`SELECT count(*) FROM actions WHERE tool = 'outbox.withdraw' AND args->>'via' = 'rest'`); n != 2 {
		t.Errorf("%d recalls audited via rest, want 2", n)
	}
	if len(h.suspended.ss) != 0 {
		t.Errorf("suspended: %+v", h.suspended.ss)
	}
}

// A person's second recall in a day suspends the grant and withdraws
// its other held mail; the response and the notifier both say so.
func TestWithdrawSuspends(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a, b, c := h.held(), h.held(), h.held()
	op := h.tokenFor(h.human, auth.SubmissionSend)

	if w := h.post("/api/v1/outbox/"+a+"/withdraw", "", op); w.Code != http.StatusOK || decode[Withdrawal](t, w).Suspended != nil {
		t.Fatalf("first recall: %d %s", w.Code, w.Body.String())
	}
	w := h.post("/api/v1/outbox/"+b+"/withdraw", "", op)
	s := decode[Withdrawal](t, w).Suspended
	if w.Code != http.StatusOK || s == nil || s.PrincipalID != h.agent.ID || s.Recalls != 2 || !slices.Equal(s.Withdrawn, []string{c}) {
		t.Fatalf("second recall: %d %s", w.Code, w.Body.String())
	}
	if len(h.suspended.ss) != 1 || h.suspended.ss[0].IdentityID != s.IdentityID {
		t.Errorf("notified: %+v", h.suspended.ss)
	}
	ids := decode[IdentityList](t, h.get("/api/v1/identities", h.token(auth.DraftCreate, auth.SubmissionSend)))
	for _, i := range ids.Identities {
		if i.Address == "arisu@example.com" && (!i.Suspended || i.HoldReason != "suspended" || i.HoldSeconds != nil) {
			t.Errorf("suspended identity: %+v", i)
		}
	}
}

// A submission the review escalates stays a draft that carries the
// verdict, so a person sees which check failed and why.
func TestEscalatedDraftReview(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.held()
	agent := h.token(auth.MessageRead, auth.DraftCreate, auth.SubmissionSend)
	d := decode[Draft](t, h.post("/api/v1/drafts", `{"from":"arisu@example.com","to":["carol@example.org"],"body_markdown":"Hi."}`, agent))
	res := decode[SubmitResult](t, h.post("/api/v1/drafts/"+d.ID+"/submit", "", agent))
	if res.State != "draft" || res.HoldReason != "review" || res.OutboxID != "" || res.HoldUntil != nil {
		t.Fatalf("submit: %+v", res)
	}
	got := decode[Draft](t, h.get("/api/v1/drafts/"+d.ID, agent))
	if got.Review == nil || len(got.Review.Checks) != 5 {
		t.Fatalf("escalated draft: %+v", got)
	}
	for _, c := range got.Review.Checks {
		if c.Pass != (c.Name != "first_contact") {
			t.Errorf("check %+v", c)
		}
		if c.Name == "first_contact" && !slices.Equal(c.Recipients, []string{"carol@example.org"}) {
			t.Errorf("first contact %+v", c)
		}
	}
}
