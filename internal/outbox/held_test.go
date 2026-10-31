package outbox

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/store"
)

// Held mail (the review tier) is never claimed; the executor releases it
// once its window ends, then sends it, and idles no longer than until
// the next window ends.
func TestHeldReleasedThenSent(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(c *Config) { c.PollInterval = time.Hour })
	ctx := t.Context()
	first := e.enqueue("bob@example.net") // bob is known from here on
	for e.step() {
	}
	if r := e.row(first.OutboxID); r.state != store.OutboxSent {
		t.Fatalf("first: %+v", r)
	}
	hold := func(seconds int) Enqueued {
		if _, err := e.pool.Exec(ctx, `INSERT INTO identity_grants (identity_id, principal_id, send_policy, recipient_scope, hold_seconds)
			VALUES ($1, $2, 'review', 'any', $3) ON CONFLICT (identity_id, principal_id) DO UPDATE SET hold_seconds = $3`,
			e.identity, e.operator.PrincipalID, seconds); err != nil {
			t.Fatal(err)
		}
		var q Enqueued
		err := e.db.WithDecision(ctx, e.operator, store.Submission{
			FromAddr: "me@example.com", Recipients: []string{"bob@example.net"}, Now: time.Now(), Content: &store.ReviewInput{},
		}, func(ctx context.Context, p *store.PolicyTx) error {
			if !p.Decision().Held {
				return fmt.Errorf("not held: %+v", p.Decision())
			}
			var err error
			q, err = e.svc.Enqueue(ctx, p, Submission{
				Raw: []byte(testRaw), Recipients: to("bob@example.net"),
				HeldDraft: &store.Draft{FromAddr: "me@example.com", To: []string{"bob@example.net"}},
			})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return q
	}

	// A window long enough that a loaded test run cannot see it end
	// before the step below, which would release and claim the row.
	q := hold(60)
	if q.DraftID == "" {
		t.Error("no draft for the held submission")
	}
	if e.step() {
		t.Fatal("claimed a held row")
	}
	if w := e.svc.idle(); w > 61*time.Second {
		t.Errorf("idle %s with a window ending in 60s", w)
	}
	if r := e.row(q.OutboxID); r.state != store.OutboxHeld {
		t.Fatalf("held: %+v", r)
	}

	q = hold(0)
	for e.step() {
	}
	var approved string
	if err := e.pool.QueryRow(ctx, `SELECT approved_by FROM outbox WHERE id = $1`, q.OutboxID).Scan(&approved); err != nil {
		t.Fatal(err)
	}
	if r := e.row(q.OutboxID); r.state != store.OutboxSent || approved != e.operator.PrincipalID {
		t.Errorf("released and sent: %+v, approved by %s", r, approved)
	}
}
