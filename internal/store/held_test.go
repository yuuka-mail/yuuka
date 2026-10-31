package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// reviewGrant gives arisu review + any on identity id with a window of
// hold seconds, and makes bob a known address, so the review passes.
func (e *sendEnv) reviewGrant(id store.Identity, hold int) {
	e.t.Helper()
	at := e.at()
	if err := e.db.SetGrant(e.t.Context(), store.Grant{
		IdentityID: id.ID, PrincipalID: e.arisu.PrincipalID, Policy: sendpolicy.Review,
		RecipientScope: sendpolicy.ScopeAny, HoldSeconds: &hold,
	}, store.Action{ID: ulid.New(at), Tool: "grant.set", At: at}); err != nil {
		e.t.Fatal(err)
	}
}

// knowBob makes bob@example.net an address mail went to before.
func (e *sendEnv) knowBob() {
	e.outbox(outboxRow{identity: e.star, by: e.op.PrincipalID, created: e.at(), to: []string{"bob@example.net"}})
}

// hold submits a draft by arisu as from to bob and enqueues it; it
// returns the outbox and draft ids, and the decision.
func (e *sendEnv) hold(from string) (string, string, sendpolicy.Decision) {
	e.t.Helper()
	ctx := e.t.Context()
	dr, err := e.db.CreateDraft(ctx, e.arisu, store.Draft{FromAddr: from, To: []string{"bob@example.net"}, BodyMD: "x"}, e.at())
	if err != nil {
		e.t.Fatal(err)
	}
	var d sendpolicy.Decision
	id := ulid.New(e.at())
	err = e.db.WithDecision(ctx, e.arisu, store.Submission{
		FromAddr: from, IdentityID: dr.IdentityID, Recipients: []string{"bob@example.net"}, Now: e.at(),
		Content: &store.ReviewInput{},
	}, func(ctx context.Context, p *store.PolicyTx) error {
		d = p.Decision()
		if !d.Held {
			return nil
		}
		at := e.at()
		msg := letterFrom(from, "bob@example.net")
		return p.Enqueue(ctx, store.NewOutbox{
			ID: id, DraftID: dr.ID, Raw: store.NewSentRaw(ulid.New(at), "sent/2026/11/"+ulid.New(at)+".eml", msg, at), Message: msg,
			Recipients: []store.OutboxRecipient{{Addr: "bob@example.net", Kind: store.RecipientTo}},
		})
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return id, dr.ID, d
}

func (e *sendEnv) outboxState(id string) (state, approved, withdrawn string) {
	e.t.Helper()
	if err := e.pool.QueryRow(e.t.Context(), `SELECT state, coalesce(approved_by, ''), coalesce(withdrawn_by, '')
		FROM outbox WHERE id = $1`, id).Scan(&state, &approved, &withdrawn); err != nil {
		e.t.Fatal(err)
	}
	return state, approved, withdrawn
}

func (e *sendEnv) draftHold(id string) string {
	e.t.Helper()
	var r string
	if err := e.pool.QueryRow(e.t.Context(), `SELECT coalesce(hold_reason, '') FROM drafts WHERE id = $1`, id).Scan(&r); err != nil {
		e.t.Fatal(err)
	}
	return r
}

// Held rows: written from the decision only, never claimed, released
// by PromoteHeld when the window ends, withdrawn by whoever may.
func TestHeld(t *testing.T) {
	t.Parallel()
	e := newSendEnv(t)
	ctx := t.Context()
	e.knowBob()
	e.reviewGrant(e.arisuID, 600)

	id, draft, d := e.hold("arisu@example.com")
	if !d.Held || d.HoldSeconds != 600 || d.Review == nil {
		t.Fatalf("decision %+v", d)
	}
	var secs float64
	var review, next *string
	if err := e.pool.QueryRow(ctx, `SELECT extract(epoch FROM hold_until - created_at), review::text, next_retry_at::text
		FROM outbox WHERE id = $1`, id).Scan(&secs, &review, &next); err != nil {
		t.Fatal(err)
	}
	if state, approved, _ := e.outboxState(id); state != "held" || approved != "" || secs < 599 || secs > 601 || review == nil || next != nil {
		t.Errorf("held row: %s approved %q window %.0fs review %v next %v", state, approved, secs, review, next)
	}

	// Without a draft to go back to, a held row is refused.
	err := e.db.WithDecision(ctx, e.arisu, store.Submission{
		FromAddr: "arisu@example.com", Recipients: []string{"bob@example.net"}, Now: e.at(), Content: &store.ReviewInput{},
	}, func(ctx context.Context, p *store.PolicyTx) error {
		at := e.at()
		msg := letterFrom("arisu@example.com", "bob@example.net")
		return p.Enqueue(ctx, store.NewOutbox{
			ID: ulid.New(at), Raw: store.NewSentRaw(ulid.New(at), "sent/2026/11/"+ulid.New(at)+".eml", msg, at), Message: msg,
			Recipients: []store.OutboxRecipient{{Addr: "bob@example.net", Kind: store.RecipientTo}},
		})
	})
	if !errors.Is(err, store.ErrNotApproved) {
		t.Errorf("held without a draft: %v", err)
	}

	// Never claimed, not released before its window ends.
	if c, err := e.db.ClaimOutbox(ctx, "test", []string{"cf"}, time.Minute); err != nil || c != nil {
		t.Errorf("claimed a held row: %+v %v", c, err)
	}
	rel, wait, err := e.db.PromoteHeld(ctx)
	if err != nil || len(rel) != 0 || wait < 590*time.Second || wait > 600*time.Second {
		t.Errorf("promote before the window ends: %+v %s %v", rel, wait, err)
	}

	// Another agent cannot withdraw it; arisu can, and that does not
	// count toward the breaker.
	momoi := e.principal("agent", "momoi", auth.SubmissionSend)
	if _, err := e.db.WithdrawOutbox(ctx, momoi, id, nil, e.at()); !errors.Is(err, store.ErrNotYours) {
		t.Errorf("someone else's agent withdrew: %v", err)
	}
	w, err := e.db.WithdrawOutbox(ctx, e.arisu, id, map[string]any{"via": "test"}, e.at())
	if err != nil || w.DraftID != draft || w.Suspended != nil {
		t.Fatalf("withdraw own: %+v %v", w, err)
	}
	if state, _, by := e.outboxState(id); state != "withdrawn" || by != e.arisu.PrincipalID || e.draftHold(draft) != "withdrawn" {
		t.Errorf("withdrawn: %s by %s, draft %s", state, by, e.draftHold(draft))
	}
	if _, err := e.db.WithdrawOutbox(ctx, e.op, id, nil, e.at()); !errors.Is(err, store.ErrOutboxState) {
		t.Errorf("withdrawn twice: %v", err)
	}
	if n := e.actions("outbox.withdraw"); n != 1 {
		t.Errorf("%d outbox.withdraw actions", n)
	}

	// A window of 0: released by the next pass, approved by system, due
	// now; a recall then is too late.
	e.reviewGrant(e.arisuID, 0)
	id, _, _ = e.hold("arisu@example.com")
	rel, wait, err = e.db.PromoteHeld(ctx)
	if err != nil || len(rel) != 1 || rel[0].OutboxID != id || rel[0].State != store.OutboxQueued || wait >= 0 {
		t.Fatalf("promote: %+v %s %v", rel, wait, err)
	}
	if state, approved, _ := e.outboxState(id); state != "queued" || approved != store.SystemPrincipalID {
		t.Errorf("released: %s approved by %s", state, approved)
	}
	if _, err := e.db.WithdrawOutbox(ctx, e.op, id, nil, e.at()); !errors.Is(err, store.ErrOutboxState) {
		t.Errorf("withdraw after release: %v", err)
	}
	if rel, _, err := e.db.PromoteHeld(ctx); err != nil || len(rel) != 0 {
		t.Errorf("promote again: %+v %v", rel, err)
	}
	if e.actions("outbox.release") != 1 {
		t.Error("no outbox.release action")
	}

	// Suspended or disabled before the window ends: withdrawn by system.
	id, draft, _ = e.hold("arisu@example.com")
	now := e.at()
	if _, err := e.pool.Exec(ctx, `UPDATE identity_grants SET suspended_at = $1 WHERE identity_id = $2`, now, e.arisuID.ID); err != nil {
		t.Fatal(err)
	}
	rel, _, err = e.db.PromoteHeld(ctx)
	if err != nil || len(rel) != 1 || rel[0].State != store.OutboxWithdrawn || rel[0].Reason != sendpolicy.HoldSuspended {
		t.Fatalf("promote suspended: %+v %v", rel, err)
	}
	if state, _, by := e.outboxState(id); state != "withdrawn" || by != store.SystemPrincipalID || e.draftHold(draft) != "suspended" {
		t.Errorf("suspended: %s by %s, draft %s", state, by, e.draftHold(draft))
	}
}

// Two recalls by a person within a day suspend the grant and withdraw
// its other held mail.
func TestHeldBreaker(t *testing.T) {
	t.Parallel()
	e := newSendEnv(t)
	ctx := t.Context()
	e.knowBob()
	e.reviewGrant(e.arisuID, 600)

	a, _, _ := e.hold("arisu@example.com")
	b, _, _ := e.hold("arisu@example.com")
	c, cDraft, _ := e.hold("arisu@example.com")
	w, err := e.db.WithdrawOutbox(ctx, e.op, a, nil, e.at())
	if err != nil || w.Suspended != nil {
		t.Fatalf("first recall: %+v %v", w, err)
	}
	w, err = e.db.WithdrawOutbox(ctx, e.op, b, nil, e.at())
	if err != nil || w.Suspended == nil || w.Suspended.Recalls != 2 || len(w.Suspended.Withdrawn) != 1 || w.Suspended.Withdrawn[0] != c ||
		w.Suspended.Identity == "" || w.Suspended.Principal.ID != w.Suspended.PrincipalID || w.Suspended.Principal.Name == "" {
		t.Fatalf("second recall: %+v %v", w.Suspended, err)
	}
	if state, _, by := e.outboxState(c); state != "withdrawn" || by != store.SystemPrincipalID || e.draftHold(cDraft) != "suspended" {
		t.Errorf("held row of the suspended grant: %s by %s", state, by)
	}
	g, err := e.db.GrantFor(ctx, e.arisuID.ID, e.arisu.PrincipalID)
	if err != nil || g.SuspendedAt == nil || g.Policy != sendpolicy.Review {
		t.Errorf("grant after the breaker: %+v %v", g, err)
	}
	if e.actions("grant.suspend") != 1 {
		t.Error("no grant.suspend action")
	}

	// Without a grant row, the breaker writes the default, suspended.
	e2 := newSendEnv(t)
	e2.knowBob()
	m := e2.deliver("\nFrom: bob@example.net\nTo: arisu@example.com\nSubject: hi\nMessage-ID: <b1@example.net>\n\nhello\n", "arisu")
	holdReply := func() string {
		dr, err := e2.db.CreateDraft(ctx, e2.arisu, store.Draft{FromAddr: "arisu@example.com", InReplyToID: m, To: []string{"bob@example.net"}, BodyMD: "x"}, e2.at())
		if err != nil {
			t.Fatal(err)
		}
		id := ulid.New(e2.at())
		err = e2.db.WithDecision(ctx, e2.arisu, store.Submission{
			FromAddr: "arisu@example.com", Recipients: []string{"bob@example.net"}, InReplyToID: m, Now: e2.at(), Content: &store.ReviewInput{},
		}, func(ctx context.Context, p *store.PolicyTx) error {
			if !p.Decision().Held {
				return fmt.Errorf("not held: %+v", p.Decision())
			}
			at := e2.at()
			msg := letterFrom("arisu@example.com", "bob@example.net")
			return p.Enqueue(ctx, store.NewOutbox{
				ID: id, DraftID: dr.ID,
				Raw: store.NewSentRaw(ulid.New(at), "sent/2026/11/"+ulid.New(at)+".eml", msg, at), Message: msg,
				Recipients: []store.OutboxRecipient{{Addr: "bob@example.net", Kind: store.RecipientTo}},
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	x, y := holdReply(), holdReply()
	if _, err := e2.db.WithdrawOutbox(ctx, e2.op, x, nil, e2.at()); err != nil {
		t.Fatal(err)
	}
	if w, err := e2.db.WithdrawOutbox(ctx, e2.op, y, nil, e2.at()); err != nil || w.Suspended == nil {
		t.Fatalf("second recall without a grant row: %+v %v", w, err)
	}
	g, err = e2.db.GrantFor(ctx, e2.arisuID.ID, e2.arisu.PrincipalID)
	if err != nil || g.SuspendedAt == nil || g.Policy != sendpolicy.Review || g.RecipientScope != sendpolicy.ScopeThread || g.HoldSeconds != nil {
		t.Errorf("written grant: %+v %v", g, err)
	}
}

// A recall and the release of the same row race: whichever takes the
// content lock first wins, and the other finds nothing to do. Never a
// withdrawn row with a send attempt, never a released one whose draft
// says withdrawn.
func TestHeldRecallRace(t *testing.T) {
	t.Parallel()
	e := newSendEnv(t)
	ctx := t.Context()
	e.knowBob()
	const n = 100
	type row struct{ id, draft string }
	rows := make([]row, n)
	for i := range rows {
		// One identity per row: a person's recalls of one grant would
		// trip the breaker.
		ident := e.identity(fmt.Sprintf("race%d@example.com", i), e.arisu.PrincipalID)
		e.reviewGrant(ident, 0)
		rows[i].id, rows[i].draft, _ = e.hold(fmt.Sprintf("race%d@example.com", i))
	}
	withdrew := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range rows {
		wg.Go(func() {
			<-start
			_, withdrew[i] = e.db.WithdrawOutbox(ctx, e.op, rows[i].id, nil, time.Now())
		})
		wg.Go(func() {
			<-start
			if _, _, err := e.db.PromoteHeld(ctx); err != nil {
				t.Error(err)
			}
			if _, err := e.db.ClaimOutbox(ctx, "race", []string{"cf"}, time.Minute); err != nil {
				t.Error(err)
			}
		})
	}
	close(start)
	wg.Wait()
	if _, _, err := e.db.PromoteHeld(ctx); err != nil {
		t.Fatal(err)
	}
	var won, lost int
	for i, r := range rows {
		state, _, _ := e.outboxState(r.id)
		var attempts int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM send_attempts WHERE outbox_id = $1`, r.id).Scan(&attempts); err != nil {
			t.Fatal(err)
		}
		hold := e.draftHold(r.draft)
		switch {
		case withdrew[i] == nil:
			won++
			if state != "withdrawn" || attempts != 0 || hold != "withdrawn" {
				t.Errorf("%d: recalled, but %s with %d attempts, draft %q", i, state, attempts, hold)
			}
		case errors.Is(withdrew[i], store.ErrOutboxState):
			lost++
			if state == "withdrawn" || hold == "withdrawn" {
				t.Errorf("%d: released, but %s, draft %q", i, state, hold)
			}
		default:
			t.Errorf("%d: %v", i, withdrew[i])
		}
	}
	t.Logf("%d recalled, %d released first", won, lost)
}
