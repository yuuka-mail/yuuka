package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yuuka-mail/yuuka/internal/store"
)

// The lock order of held.go under load: recalls, releases, claims and
// every draft writer run at once on the same held rows and drafts. A
// path that took the locks in another order deadlocks here, and Postgres
// aborts one side with 40P01 after deadlock_timeout; every other outcome
// is one the paths are allowed to have.
func TestHeldLockOrder(t *testing.T) {
	t.Parallel()
	e := newSendEnv(t)
	ctx := t.Context()
	e.knowBob()
	const n = 24
	type row struct{ id, draft, from string }
	rows := make([]row, n)
	for i := range rows {
		from := fmt.Sprintf("lock%d@example.com", i)
		ident := e.identity(from, e.arisu.PrincipalID)
		e.reviewGrant(ident, i%2) // half released at once, half after a second
		rows[i].id, rows[i].draft, _ = e.hold(from)
		rows[i].from = from
	}

	var mu sync.Mutex
	var failures []string
	check := func(what string, err error, allowed ...error) {
		if err == nil {
			return
		}
		if pe, ok := errors.AsType[*pgconn.PgError](err); ok && pe.Code == "40P01" {
			mu.Lock()
			failures = append(failures, what+": deadlock: "+err.Error())
			mu.Unlock()
			return
		}
		for _, a := range allowed {
			if errors.Is(err, a) {
				return
			}
		}
		mu.Lock()
		failures = append(failures, what+": "+err.Error())
		mu.Unlock()
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, r := range rows {
		wg.Go(func() {
			<-start
			_, err := e.db.WithdrawOutbox(ctx, e.op, r.id, nil, time.Now())
			check("withdraw", err, store.ErrOutboxState)
		})
		wg.Go(func() {
			<-start
			_, _, err := e.db.PromoteHeld(ctx)
			check("promote", err)
			_, err = e.db.ClaimOutbox(ctx, "lock", []string{"cf"}, time.Minute)
			check("claim", err)
		})
		wg.Go(func() {
			<-start
			_, err := e.db.UpdateDraft(ctx, e.arisu, store.Draft{ID: r.draft, FromAddr: r.from, To: []store.Recipient{{Addr: "bob@example.net"}}, BodyMD: "y"}, time.Now())
			check("update draft", err, store.ErrInFlight)
		})
		wg.Go(func() {
			<-start
			check("delete draft", e.db.DeleteDraft(ctx, e.arisu, r.draft, time.Now()), store.ErrInFlight)
		})
		wg.Go(func() {
			<-start
			// The author submitting the draft again: without content the
			// review holds it as a draft, through holdDraft.
			err := e.db.WithDecision(ctx, e.arisu, store.Submission{
				FromAddr: r.from, IdentityID: "", Recipients: []string{"bob@example.net"}, Now: time.Now(),
			}, func(ctx context.Context, p *store.PolicyTx) error {
				if _, err := p.LockDraft(ctx, r.draft); err != nil {
					return err
				}
				return p.HoldDraft(ctx, r.draft, time.Now())
			})
			check("submit draft", err, store.ErrInFlight)
		})
	}
	close(start)
	wg.Wait()
	for _, f := range failures {
		t.Error(f)
	}
}
