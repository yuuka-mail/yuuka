package store_test

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/extract"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
)

// A fold writes the message's extractions with its other content
// columns; a rebuild from the same parse changes nothing and logs
// nothing; Reextract rewrites them alone and records the version.
func TestExtractionsFollowTheContent(t *testing.T) {
	t.Parallel()
	db, pool := storetest.New(t)
	ctx := t.Context()
	acct := storetest.Account(t, db)
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	raw := letter("no-reply@example.net", "a@example.com", "Your verification code is 482913", "",
		"Your verification code is 482913. It expires in 10 minutes.\r\n\r\nSign in: <https://example.net/login>")
	id := inbound(t, db, acct, "a", raw, at)

	v := &auth.Viewer{PrincipalID: store.SystemPrincipalID, PrincipalKind: "human", PrincipalName: "operator"}
	d, err := db.MessageByID(ctx, v, id)
	if err != nil {
		t.Fatal(err)
	}
	want := extract.Message(mimeparse.Parse(raw), at)
	if len(d.Extractions) != len(want) || len(want) < 2 {
		t.Fatalf("extractions %+v, want %+v", d.Extractions, want)
	}
	for i, x := range want {
		got := d.Extractions[i]
		if got.Kind != string(x.Kind) || got.Value != x.Value || got.Confidence != x.Confidence || got.Source != string(x.Source) ||
			(got.ExpiresAt == nil) != x.Expires.IsZero() || got.ExpiresAt != nil && !got.ExpiresAt.Equal(x.Expires) {
			t.Errorf("extraction %d = %+v, want %+v", i, got, x)
		}
	}
	// The summary carries the best of each kind.
	if len(d.Extracted) != 3 || d.Extracted[0].Kind != "otp" || d.Extracted[0].Value != "482913" ||
		d.Extracted[1].Value != "https://example.net/login" || d.Extracted[2].ExpiresAt == nil {
		t.Errorf("summary extractions %+v", d.Extracted)
	}
	// No trusted receiver in this letter: nothing proves the sender.
	if d.SenderVerified == nil || *d.SenderVerified {
		t.Errorf("sender verified %v, want false", d.SenderVerified)
	}

	// The same parse again changes nothing, logs nothing.
	before := count(t, pool, `SELECT count(*) FROM changes`)
	if changed, err := db.Reextract(ctx, id, mimeparse.Parse(raw)); err != nil || changed {
		t.Fatalf("re-extract unchanged: %v %v", changed, err)
	}
	if n := count(t, pool, `SELECT count(*) FROM changes`); n != before {
		t.Errorf("unchanged re-extract logged %d changes", n-before)
	}
	// What an older extractor left: other rows, an older version.
	for _, q := range []string{
		`UPDATE extractions SET confidence = 1 WHERE message_id = $1 AND ord = 0`,
		`UPDATE messages SET extract_version = 0 WHERE id = $1`,
	} {
		if _, err := pool.Exec(ctx, q, id); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := db.UnextractedMessages(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].MessageID != id {
		t.Fatalf("unextracted %+v %v", pending, err)
	}
	if changed, err := db.Reextract(ctx, id, mimeparse.Parse(raw)); err != nil || !changed {
		t.Fatalf("re-extract: %v %v", changed, err)
	}
	if n := count(t, pool, `SELECT count(*) FROM extractions WHERE message_id = $1 AND confidence = 1`, id); n != 0 {
		t.Error("stale row kept")
	}
	if n := count(t, pool, `SELECT count(*) FROM messages WHERE id = $1 AND extract_version = $2`, id, extract.Version); n != 1 {
		t.Error("version not recorded")
	}
	if pending, err := db.UnextractedMessages(ctx, 10); err != nil || len(pending) != 0 {
		t.Fatalf("still unextracted %+v %v", pending, err)
	}
}

// The class reads the extractor (classify.Of): mail that delivers a code
// is a notification, and the re-extraction pass sets the class of mail
// stored before that, thread aggregate included.
func TestReextractClassifies(t *testing.T) {
	t.Parallel()
	db, pool := storetest.New(t)
	ctx := t.Context()
	acct := storetest.Account(t, db)
	raw := letter("security@example.org", "a@example.com", "Example security code", "", "Your verification code is: H7K2QX")
	id := inbound(t, db, acct, "a", raw, time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC))
	if n := count(t, pool, `SELECT count(*) FROM messages m JOIN threads t ON t.id = m.thread_id
		WHERE m.id = $1 AND m.class = 1 AND t.class = 1`, id); n != 1 {
		t.Fatal("a letter-and-digit code did not make a notification")
	}
	// What the M2 classifier left: unknown, and not extracted yet.
	for _, q := range []string{
		`UPDATE messages SET class = 0, extract_version = 0 WHERE id = $1`,
		`UPDATE threads SET class = 0 WHERE id = (SELECT thread_id FROM messages WHERE id = $1)`,
	} {
		if _, err := pool.Exec(ctx, q, id); err != nil {
			t.Fatal(err)
		}
	}
	if changed, err := db.Reextract(ctx, id, mimeparse.Parse(raw)); err != nil || !changed {
		t.Fatalf("re-extract: %v %v", changed, err)
	}
	if n := count(t, pool, `SELECT count(*) FROM messages m JOIN threads t ON t.id = m.thread_id
		WHERE m.id = $1 AND m.class = 1 AND t.class = 1`, id); n != 1 {
		t.Error("class not set by the re-extraction")
	}
}

// Upgrading to 0008: messages already stored have no extractions and
// version 0, which the backlog pass lists; the migration cannot fill
// them, since that takes parsing the raw.
func TestMigrateLeavesExtractionsPending(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	ownerURL, _ := storetest.URLs(t)
	pool, err := pgxpool.New(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	db := store.New(pool)
	if _, to, err := db.MigrateTo(ctx, 7); err != nil || to != 7 {
		t.Fatalf("migrate to 7: %d, %v", to, err)
	}
	acct, err := db.EnsureCloudflareAccount(ctx, "cloudflare")
	if err != nil {
		t.Fatal(err)
	}
	in := record(acct, 1, sha256.Sum256([]byte("m2 content")), "a@example.com")
	if _, err := db.CommitIngest(ctx, in); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO threads (id, subject_norm, participants, first_at, last_at, n, class)
		 SELECT $1, 'hi', '{bob@example.net}', received_at, received_at, 1, 1 FROM raw_objects WHERE id = $2`,
		`INSERT INTO messages (id, canonical_raw_id, content_hash, thread_id, received_at, subject, snippet,
			from_addr, from_name, class, has_attach, body_text, parser_version, to_addrs, cc_addrs, reply_to_addrs)
		 SELECT $1, r.id, r.content_hash, $1, r.received_at, 'Your code is 482913', '', 'bob@example.net', 'Bob', 1,
			false, '', 2, '{}', '{}', '{}'
		 FROM raw_objects r WHERE r.id = $2`,
		`UPDATE deliveries SET message_id = $1 WHERE raw_id = $2`,
	} {
		if _, err := pool.Exec(ctx, q, in.DeliveryID, in.RawID); err != nil {
			t.Fatal(err)
		}
	}
	if from, to, err := db.Migrate(ctx); err != nil || from != 7 || to < 8 {
		t.Fatalf("migrate: %d -> %d, %v", from, to, err)
	}
	if n := count(t, pool, `SELECT count(*) FROM messages WHERE extract_version = 0`); n != 1 {
		t.Fatalf("%d messages at extract_version 0, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM extractions`); n != 0 {
		t.Fatalf("%d extractions before the backlog pass", n)
	}
	pending, err := db.UnextractedMessages(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].MessageID != in.DeliveryID {
		t.Fatalf("unextracted %+v, %v", pending, err)
	}
	// The table's invariants hold whoever writes.
	for _, q := range []string{
		`INSERT INTO extractions VALUES ($1, 0, 'otp', '1', 50, 'body', now())`,
		`INSERT INTO extractions VALUES ($1, 0, 'expiry', 'PT1M', 50, 'body', NULL)`,
		`INSERT INTO extractions VALUES ($1, 0, 'otp', '1', 0, 'body', NULL)`,
		`INSERT INTO extractions VALUES ($1, 0, 'pin', '1', 50, 'body', NULL)`,
		`INSERT INTO extractions VALUES ($1, 0, 'otp', '', 50, 'body', NULL)`,
		`INSERT INTO extractions VALUES ($1, 0, 'otp', '1', 50, 'header', NULL)`,
	} {
		if _, err := pool.Exec(ctx, q, in.DeliveryID); err == nil {
			t.Errorf("accepted: %s", q)
		}
	}
}
