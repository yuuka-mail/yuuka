package main

import (
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// unknownOutbox writes an outbox row that an attempt left unknown, the
// way the executor and recovery leave it.
func (e *hostEnv) unknownOutbox() string {
	e.t.Helper()
	ctx := e.t.Context()
	identity := ulid.New(time.Now())
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO identities (id, account_id, zone, prefix, transport, owner_id)
		VALUES ($1, $2, 'example.com', $3, 'cf', $4)`,
		identity, storetest.Account(e.t, e.db), strings.ToLower(identity), store.SystemPrincipalID); err != nil {
		e.t.Fatal(err)
	}
	raw := []byte("Subject: hi\r\n\r\nhi\r\n")
	now := time.Now()
	id, rawID := ulid.New(now), ulid.New(now)
	if err := e.db.EnqueueOutbox(ctx, store.NewOutbox{
		ID: id, IdentityID: identity, FromAddr: "me@example.com",
		SubmittedBy: store.SystemPrincipalID, ApprovedBy: store.SystemPrincipalID,
		Raw:        store.SentRaw{ID: rawID, Key: "sent/2026/10/" + rawID + ".eml", Size: int64(len(raw)), SHA256: sha256.Sum256(raw), At: now},
		Recipients: []store.OutboxRecipient{{Addr: "a@example.net", Kind: "to"}, {Addr: "b@example.net", Kind: "cc"}},
	}); err != nil {
		e.t.Fatal(err)
	}
	c, err := e.db.ClaimOutbox(ctx, "gone", []string{"cf"}, time.Minute)
	if err != nil || c == nil {
		e.t.Fatalf("claim: %v %v", c, err)
	}
	if err := e.db.CommitAttempt(ctx, c.AttemptID, "gone"); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE send_attempts SET lease_until = now() - interval '1 second' WHERE id = $1`, c.AttemptID); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.db.RecoverAttempts(ctx, time.Minute); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func TestOutboxCommands(t *testing.T) {
	t.Parallel()
	e := newHostEnv(t)
	id := e.unknownOutbox()

	out := e.must("outbox", "ls")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "ID") || !strings.Contains(lines[1], id) ||
		!strings.Contains(lines[1], "unknown") || !strings.Contains(lines[1], "a@example.net +1 [unknown 2]") ||
		!strings.Contains(lines[1], "1 (unknown)") || !strings.Contains(lines[1], "commit point") {
		t.Errorf("ls:\n%s", out)
	}
	if out := e.must("outbox", "ls", "--state", "sent"); strings.Count(out, "\n") != 1 {
		t.Errorf("ls --state sent:\n%s", out)
	}

	if out := e.must("outbox", "resolve", strings.ToLower(id), "--sent"); !strings.Contains(out, id+" is now sent") {
		t.Errorf("resolve: %q", out)
	}
	actor, cred, args := e.lastAction("outbox.resolve")
	if actor != store.SystemPrincipalID || cred != nil || args["via"] != "cli" || args["host_user"] != "tester" ||
		args["outbox_id"] != id || args["resolution"] != "sent" || args["from"] != "unknown" || args["to"] != "sent" {
		t.Errorf("recorded %s %v %v", actor, cred, args)
	}
	if _, _, err := e.run("outbox", "resolve", id, "--abandon"); err == nil || !strings.Contains(err.Error(), "only unknown") {
		t.Errorf("resolve a sent row: %v", err)
	}

	for _, bad := range [][]string{
		{"resolve", id},
		{"resolve", id, "--sent", "--abandon"},
		{"resolve", "--sent", id},
		{"resolve", ulid.New(time.Now()), "--sent"},
		{"ls", "--state", "maybe"},
		{"ls", "extra"},
		{"retry"},
		{},
	} {
		if _, _, err := e.run("outbox", bad...); err == nil {
			t.Errorf("outbox %v: no error", bad)
		}
	}
}
