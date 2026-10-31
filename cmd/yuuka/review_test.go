package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// A press of the recall button withdraws as the configured human, via
// telegram and without a credential; a replay is told what happened.
func TestRecaller(t *testing.T) {
	t.Parallel()
	e := newHostEnv(t)
	ctx := t.Context()
	now := time.Now()
	principal := func(kind, name string) *auth.Viewer {
		p := store.Principal{ID: ulid.New(time.Now()), Kind: kind, Name: name, DisplayName: name, CreatedAt: now}
		if err := e.db.CreatePrincipal(ctx, p, store.Action{ID: ulid.New(now), Tool: "principal.create", At: now}); err != nil {
			t.Fatal(err)
		}
		return &auth.Viewer{PrincipalID: p.ID, PrincipalKind: kind, PrincipalName: name, Scopes: []auth.Scope{auth.SubmissionSend}}
	}
	op, arisu := principal("human", "operator"), principal("agent", "arisu")
	for _, a := range []struct{ prefix, owner string }{{"me", op.PrincipalID}, {"arisu", arisu.PrincipalID}} {
		if _, err := e.pool.Exec(ctx, `INSERT INTO identities (id, account_id, zone, prefix, transport, owner_id)
			VALUES ($1, $2, 'example.com', $3, 'cf', $4)`, ulid.New(time.Now()), storetest.Account(t, e.db), a.prefix, a.owner); err != nil {
			t.Fatal(err)
		}
	}
	enqueue := func(v *auth.Viewer, from string, s store.Submission, draft *store.Draft) string {
		t.Helper()
		id, rawID := ulid.New(time.Now()), ulid.New(time.Now())
		raw := []byte("From: " + from + "\r\nSubject: hi\r\n\r\nhi\r\n")
		s.FromAddr, s.Recipients, s.Now = from, []string{"bob@example.net"}, time.Now()
		err := e.db.WithDecision(ctx, v, s, func(ctx context.Context, p *store.PolicyTx) error {
			return p.Enqueue(ctx, store.NewOutbox{
				ID: id, Raw: store.NewSentRaw(rawID, "sent/2026/11/"+rawID+".eml", raw, time.Now()),
				Message: raw, Recipients: []store.OutboxRecipient{{Addr: "bob@example.net", Kind: "to"}}, HeldDraft: draft,
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	enqueue(op, "me@example.com", store.Submission{}, nil) // bob is known now
	if _, err := e.pool.Exec(ctx, `INSERT INTO identity_grants (identity_id, principal_id, send_policy, recipient_scope)
		SELECT id, $1, 'review', 'any' FROM identities WHERE prefix = 'arisu'`, arisu.PrincipalID); err != nil {
		t.Fatal(err)
	}
	held := enqueue(arisu, "arisu@example.com", store.Submission{Content: &store.ReviewInput{}},
		&store.Draft{FromAddr: "arisu@example.com", To: []store.Recipient{{Addr: "bob@example.net"}}})

	recall := recaller(e.db, &auth.Viewer{PrincipalID: op.PrincipalID, PrincipalKind: "human", PrincipalName: "operator"})
	answer, done, suspended, err := recall(ctx, held, 12345)
	if err != nil || !done || suspended != nil || answer != "Recalled. It is a draft again." {
		t.Fatalf("recall: %q %v %v %v", answer, done, suspended, err)
	}
	var actor, args string
	var cred *string
	if err := e.pool.QueryRow(ctx, `SELECT actor, credential_id, args::text FROM actions WHERE tool = 'outbox.withdraw'`).
		Scan(&actor, &cred, &args); err != nil {
		t.Fatal(err)
	}
	if actor != op.PrincipalID || cred != nil || !strings.Contains(args, `"via": "telegram"`) || !strings.Contains(args, `"telegram_user_id": 12345`) {
		t.Errorf("audit: %s %v %s", actor, cred, args)
	}
	if answer, done, _, err := recall(ctx, held, 12345); err != nil || !done || answer != "Already recalled." {
		t.Errorf("replay: %q %v %v", answer, done, err)
	}
	if answer, done, _, err := recall(ctx, ulid.New(time.Now()), 12345); err != nil || !done || answer != "No such message." {
		t.Errorf("unknown: %q %v %v", answer, done, err)
	}
}
