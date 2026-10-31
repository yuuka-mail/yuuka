package ingest

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/contenthash"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// sendCopy stores raw as a sent copy the way the enqueuer does (local
// copy, raw bucket, raw_objects) and, unless draftOnly, points a queued
// outbox row of identity at it. It returns the raw and outbox ids.
func (e *env) sendCopy(t *testing.T, identity string, at time.Time, raw []byte, local, draftOnly bool) (rawID, outboxID string) {
	t.Helper()
	ctx := t.Context()
	rawID = ulid.New(at)
	key := fmt.Sprintf("sent/%04d/%02d/%s.eml", at.Year(), int(at.Month()), rawID)
	if local {
		if err := e.local.Put(key, raw); err != nil {
			t.Fatal(err)
		}
	}
	e.raw.Put(key, raw, nil)
	sum, hash := sha256.Sum256(raw), contenthash.V1(raw)
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO raw_objects (id, r2_key, local_path, size, sha256, received_at, content_hash)
		VALUES ($1, $2, $2, $3, $4, $5, $6)`, rawID, key, len(raw), sum[:], at, hash[:]); err != nil {
		t.Fatal(err)
	}
	if draftOnly {
		return rawID, ""
	}
	outboxID = ulid.New(at)
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO outbox (id, state, next_retry_at, identity_id, from_addr, submitted_by, approved_by, raw_id)
		VALUES ($1, 'queued', $2, $3, 'me@example.com', $4, $4, $5)`,
		outboxID, at, identity, store.SystemPrincipalID, rawID); err != nil {
		t.Fatal(err)
	}
	return rawID, outboxID
}

func (e *env) identity(t *testing.T) string {
	t.Helper()
	acct, err := e.in.account(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	id := ulid.New(e.now)
	if _, err := e.pool.Exec(t.Context(), `
		INSERT INTO identities (id, account_id, zone, prefix, owner_id)
		VALUES ($1, $2, 'example.com', 'me', $3)`, id, acct, store.SystemPrincipalID); err != nil {
		t.Fatal(err)
	}
	storetest.Route(t, e.pool, id, "cf")
	return id
}

func (e *env) sentMessage(t *testing.T, rawID string) (id, thread, canonical string) {
	t.Helper()
	err := e.pool.QueryRow(t.Context(), `
		SELECT m.id, m.thread_id, m.canonical_raw_id FROM raw_objects r JOIN messages m ON m.content_hash = r.content_hash
		WHERE r.id = $1`, rawID).Scan(&id, &thread, &canonical)
	if err != nil {
		t.Fatalf("message of sent copy %s: %v", rawID, err)
	}
	return id, thread, canonical
}

// Sent copies reach messages through the enqueuer's hook or, when that
// did not run, the backlog pass; replies thread with them; reparse
// rebuilds them from outbox.raw_id alone and a second reparse changes
// nothing.
func TestIndexSentAndReparse(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	me := e.identity(t)
	sent := func(to, subject, body string) []byte {
		return fmt.Appendf(nil, "From: Me <me@example.com>\r\nTo: %s\r\nSubject: %s\r\n"+
			"Date: Sat, 31 Oct 2026 12:00:00 +0000\r\nMIME-Version: 1.0\r\n"+
			"Content-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", to, subject, body)
	}

	first := sent("bob@example.net", "Lunch on Friday", "Shall we meet at noon?")
	rawID, outbox := e.sendCopy(t, me, e.now, first, true, false)
	if err := e.in.IndexSent(t.Context(), rawID); err != nil {
		t.Fatal(err)
	}
	sentID, sentThread, canonical := e.sentMessage(t, rawID)
	if canonical != rawID {
		t.Fatalf("canonical raw %s, want the sent copy %s", canonical, rawID)
	}

	// The hook never ran for this one, and only the raw bucket has it.
	second := sent("carol@example.net", "Plans", "Saturday?")
	rawID2, _ := e.sendCopy(t, me, e.now.Add(time.Minute), second, false, false)
	// Nor for a draft's raw, which is no sent copy yet.
	draft := sent("dave@example.net", "Draft", "not yet")
	draftRaw, _ := e.sendCopy(t, me, e.now.Add(2*time.Minute), draft, true, true)
	if err := e.in.IndexSent(t.Context(), draftRaw); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("IndexSent of a draft's raw: %v", err)
	}
	st, err := e.in.ProcessBacklog(t.Context())
	if err != nil || st != (BacklogStats{Processed: 1}) {
		t.Fatalf("backlog %+v %v", st, err)
	}
	e.sentMessage(t, rawID2)
	if n := e.count(t, `SELECT count(*) FROM messages`); n != 2 {
		t.Fatalf("%d messages, want the two sent copies", n)
	}

	// Cloudflare answered "250 Ok <cf-1@example.com>"; the reply names it.
	if _, err := e.pool.Exec(t.Context(), `
		INSERT INTO send_attempts (id, outbox_id, recipients, transport, owner, lease_until, committed_at, finished_at, outcome, provider_id)
		VALUES ($1, $2, '{bob@example.net}', 'cf', 'test', $3, $3, $3, 'accepted', 'cf-1@example.com')`,
		ulid.New(e.now), outbox, e.now); err != nil {
		t.Fatal(err)
	}
	reply := e.ingest(t, time.Hour, "me@example.com", mail("me@example.com", "pass",
		"From: Bob <bob@example.net>\r\nSubject: Re: Lunch on Friday\r\nMessage-ID: <r1@example.net>\r\nIn-Reply-To: <cf-1@example.com>\r\n", "yes"))
	if _, th := e.messageOf(t, reply.ID); th != sentThread {
		t.Fatalf("reply in thread %s, want the sent copy's %s", th, sentThread)
	}
	e.in.Wait()
	if got := e.notes.got(); len(got) != 0 {
		// me@ is discovered, so the reply is not notified either; sent
		// copies never are.
		t.Errorf("notified %q", got)
	}

	// What an older parser left behind.
	if _, err := e.pool.Exec(t.Context(), `UPDATE messages SET subject = 'stale', body_text = '', parser_version = 0`); err != nil {
		t.Fatal(err)
	}
	rs, err := e.in.Reparse(t.Context(), time.Time{})
	if err != nil || rs.Messages != 3 || rs.Changed != 3 || rs.Failed != 0 || rs.Threads.Moved != 0 {
		t.Fatalf("reparse %+v %v", rs, err)
	}
	var subject string
	if err := e.pool.QueryRow(t.Context(), `SELECT subject FROM messages WHERE id = $1`, sentID).Scan(&subject); err != nil || subject != "Lunch on Friday" {
		t.Errorf("sent copy rebuilt as %q %v", subject, err)
	}
	if id, th, c := e.sentMessage(t, rawID); id != sentID || th != sentThread || c != rawID {
		t.Errorf("after reparse: message %s thread %s canonical %s", id, th, c)
	}
	before := e.count(t, `SELECT count(*) FROM changes`)
	rs, err = e.in.Reparse(t.Context(), time.Time{})
	if err != nil || rs.Messages != 3 || rs.Changed != 0 || rs.Threads.Moved != 0 {
		t.Fatalf("idempotent reparse %+v %v", rs, err)
	}
	if n := e.count(t, `SELECT count(*) FROM changes`); n != before {
		t.Errorf("idempotent reparse logged %d changes", n-before)
	}
	if n := e.count(t, `SELECT count(*) FROM messages`); n != 3 {
		t.Errorf("%d messages after reparse; the draft's raw must stay out", n)
	}
}
