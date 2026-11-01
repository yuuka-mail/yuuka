package store_test

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// systemID is the one system principal 0003 inserts.
const systemID = "00000000000000000000000000"

// m1 returns an owner pool on a fresh schema migrated to 0004, the last
// M1 migration, as a deployment upgrading to M2 has it.
func m1(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ownerURL, _ := storetest.URLs(t)
	pool, err := pgxpool.New(t.Context(), ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, to, err := store.New(pool).MigrateTo(t.Context(), 4); err != nil || to != 4 {
		t.Fatalf("migrate to 4: %d, %v", to, err)
	}
	return pool
}

// oldAccount inserts a Cloudflare account as EnsureCloudflareAccount did
// before 0019 gave accounts default_read, for tests that set up data on
// an older schema (0005 to 0018).
func oldAccount(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	id := ulid.New(time.Now())
	if _, err := pool.Exec(t.Context(), `INSERT INTO accounts (id, name, kind) VALUES ($1, $2, 'cf_catchall')`, id, name); err != nil {
		t.Fatal(err)
	}
	return id
}

// m1Delivery writes a raw object and its delivery as M1's CommitIngest
// did, keyed by account_key.
func m1Delivery(t *testing.T, pool *pgxpool.Pool, n int, accountKey string, contentHash [32]byte, to string) string {
	t.Helper()
	at := time.Date(2026, 10, 31, 1, 0, n, 0, time.UTC)
	id := ulid.New(at)
	key := fmt.Sprintf("raw/2026/10/%s.eml", id)
	sum := sha256.Sum256(fmt.Appendf(nil, "raw-%d", n))
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO raw_objects (id, r2_key, local_path, size, sha256, received_at, content_hash)
		VALUES ($1, $2, $2, 10, $3, $4, $5)`, id, key, sum[:], at, contentHash[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO deliveries (id, account_key, source_key, raw_id, zone, prefix, envelope_from, envelope_to, received_at)
		VALUES ($1, $2, $3, $1, 'example.com', 'a', 's@example.net', $4, $5)`,
		id, accountKey, store.PushSourceKey(contentHash, to), to, at); err != nil {
		t.Fatal(err)
	}
	return id
}

// Upgrading a database with M1 mail: every account_key becomes an account,
// deliveries and account_access point at it, and the configured account
// resolves to the converted row, so redelivery dedupe holds across the
// upgrade.
func TestMigrateConvertsAccountKeys(t *testing.T) {
	t.Parallel()
	pool := m1(t)
	ctx := t.Context()
	ch := sha256.Sum256([]byte("content"))
	d1 := m1Delivery(t, pool, 1, "cloudflare", ch, "a@example.com")
	d2 := m1Delivery(t, pool, 2, "cloudflare", ch, "b@example.com")
	d3 := m1Delivery(t, pool, 3, "other", ch, "a@example.com")
	// "later" never received mail; it only appears in account_access.
	if _, err := pool.Exec(ctx, `INSERT INTO account_access (account_id, principal_id, can_read)
		VALUES ('cloudflare', $1, true), ('later', $1, false)`, systemID); err != nil {
		t.Fatal(err)
	}

	db := store.New(pool)
	if _, to, err := db.Migrate(ctx); err != nil || to < 5 {
		t.Fatalf("migrate: %d, %v", to, err)
	}

	accounts := map[string]string{} // name -> id
	rows, err := pool.Query(ctx, `SELECT name, id FROM accounts WHERE kind = 'cf_catchall'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name, id string
		if err := rows.Scan(&name, &id); err != nil {
			t.Fatal(err)
		}
		accounts[name] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 3 || accounts["cloudflare"] == "" || accounts["other"] == "" || accounts["later"] == "" {
		t.Fatalf("accounts = %v", accounts)
	}
	for d, want := range map[string]string{d1: "cloudflare", d2: "cloudflare", d3: "other"} {
		var got string
		if err := pool.QueryRow(ctx, `SELECT account_id FROM deliveries WHERE id = $1`, d).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != accounts[want] {
			t.Errorf("delivery %s: account %s, want %s (%s)", d, got, accounts[want], want)
		}
	}
	var access []string
	if err := pool.QueryRow(ctx, `SELECT array_agg(account_id ORDER BY can_read) FROM account_access`).Scan(&access); err != nil {
		t.Fatal(err)
	}
	if len(access) != 2 || access[0] != accounts["later"] || access[1] != accounts["cloudflare"] {
		t.Errorf("account_access = %v", access)
	}
	var gone bool
	if err := pool.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'deliveries' AND column_name = 'account_key')`).Scan(&gone); err != nil || !gone {
		t.Errorf("deliveries.account_key still there (%v)", err)
	}

	// YUUKA_ACCOUNT=cloudflare finds the converted row, and a redelivery
	// of mail committed before the upgrade still collides with it.
	id, err := db.EnsureCloudflareAccount(ctx, "cloudflare")
	if err != nil || id != accounts["cloudflare"] {
		t.Fatalf("ensure cloudflare: %s, %v; want %s", id, err, accounts["cloudflare"])
	}
	out, err := db.CommitIngest(ctx, record(id, 10, ch, "a@example.com"))
	if err != nil || out != store.Redelivery {
		t.Fatalf("redelivery after upgrade: %v, %v", out, err)
	}
	// The key is per account: the same source key elsewhere is new.
	fresh, err := db.EnsureCloudflareAccount(ctx, "fresh")
	if err != nil {
		t.Fatal(err)
	}
	out, err = db.CommitIngest(ctx, record(fresh, 11, ch, "a@example.com"))
	if err != nil || out != store.Delivered {
		t.Fatalf("same source key, new account: %v, %v", out, err)
	}
}

// What the migration cannot convert, it refuses, and leaves the database
// at 0004 for the operator to fix.
func TestMigrateRefusesUnconvertible(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(*testing.T, *pgxpool.Pool){
		// No identity can exist to point at; grants are decisions, so
		// they are not dropped silently.
		"identity_grants": func(t *testing.T, pool *pgxpool.Pool) {
			if _, err := pool.Exec(t.Context(), `INSERT INTO identity_grants (identity_id, principal_id)
				VALUES ('01JBAAAAAAAAAAAAAAAAAAAAAA', $1)`, systemID); err != nil {
				t.Fatal(err)
			}
		},
		"account name": func(t *testing.T, pool *pgxpool.Pool) {
			m1Delivery(t, pool, 1, "my account", sha256.Sum256([]byte("c")), "a@example.com")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pool := m1(t)
			setup(t, pool)
			_, to, err := store.New(pool).Migrate(t.Context())
			if err == nil || !strings.Contains(err.Error(), "0005") {
				t.Fatalf("migrate: %v", err)
			}
			if to != 4 {
				t.Errorf("stopped at %d, want 4", to)
			}
			t.Log(err)
		})
	}
}

func wantCode(t *testing.T, err error, code, what string) {
	t.Helper()
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != code {
		t.Errorf("%s: %v, want SQLSTATE %s", what, err, code)
	}
}

// The invariants the sending schema holds by itself, whatever the code
// on top gets wrong.
func TestSendingInvariants(t *testing.T) {
	t.Parallel()
	db, pool := storetest.New(t)
	ctx := t.Context()
	exec := func(q string, args ...any) error {
		_, err := pool.Exec(ctx, q, args...)
		return err
	}
	account := storetest.Account(t, db)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

	// One active identity per address, one pattern per zone; disabling
	// frees the address.
	idn := func(n int, prefix any) (string, error) {
		id := ulid.New(now.Add(time.Duration(n) * time.Second))
		if err := exec(`INSERT INTO identities (id, account_id, zone, prefix, owner_id)
			VALUES ($1, $2, 'example.com', $3, $4)`, id, account, prefix, systemID); err != nil {
			return id, err
		}
		storetest.Route(t, pool, id, "cf")
		return id, nil
	}
	star, err := idn(1, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = idn(2, nil)
	wantCode(t, err, "23505", "second *@zone")
	alerts, err := idn(3, "alerts")
	if err != nil {
		t.Fatal(err)
	}
	_, err = idn(4, "alerts")
	wantCode(t, err, "23505", "second alerts@")
	if err := exec(`UPDATE identities SET disabled_at = now() WHERE id = $1`, star); err != nil {
		t.Fatal(err)
	}
	if _, err := idn(5, nil); err != nil {
		t.Errorf("*@zone after disabling the old one: %v", err)
	}

	// Grants must name an identity now.
	wantCode(t, exec(`INSERT INTO identity_grants (identity_id, principal_id) VALUES ($1, $2)`,
		ulid.New(now), systemID), "23503", "grant on a missing identity")
	if err := exec(`INSERT INTO identity_grants (identity_id, principal_id, send_policy) VALUES ($1, $2, 'auto')`,
		alerts, systemID); err != nil {
		t.Fatal(err)
	}

	// A sent copy and its outbox row.
	raw := ulid.New(now)
	sum := sha256.Sum256([]byte("sent"))
	if err := exec(`INSERT INTO raw_objects (id, r2_key, local_path, size, sha256, received_at, content_hash)
		VALUES ($1, $2, $2, 4, $3, $4, $3)`, raw, "sent/2026/10/"+raw+".eml", sum[:], now); err != nil {
		t.Fatal(err)
	}
	outbox := ulid.New(now)
	insertOutbox := func(state string, next any) error {
		return exec(`INSERT INTO outbox (id, state, next_retry_at, identity_id, from_addr, submitted_by, approved_by, raw_id)
			VALUES ($1, $2, $3, $4, 'alerts@example.com', $5, $5, $6)`, outbox, state, next, alerts, systemID, raw)
	}
	wantCode(t, insertOutbox("queued", nil), "23514", "queued without next_retry_at")
	wantCode(t, insertOutbox("sending", now), "23514", "sending with next_retry_at")
	if err := insertOutbox("queued", now); err != nil {
		t.Fatal(err)
	}

	// Recipients keep their spelling for RCPT TO and dedupe on the fold.
	if err := exec(`INSERT INTO outbox_recipients (outbox_id, addr, addr_fold, kind)
		VALUES ($1, 'Bob@Example.net', 'bob@example.net', 'to')`, outbox); err != nil {
		t.Fatal(err)
	}
	wantCode(t, exec(`INSERT INTO outbox_recipients (outbox_id, addr, addr_fold, kind)
		VALUES ($1, 'bob@example.net', 'bob@example.net', 'cc')`, outbox), "23505", "same mailbox twice")
	var addr string
	if err := pool.QueryRow(ctx, `SELECT addr FROM outbox_recipients WHERE addr_fold = 'bob@example.net'`).Scan(&addr); err != nil || addr != "Bob@Example.net" {
		t.Errorf("addr = %q, %v; want the spelling given", addr, err)
	}

	// One open attempt per message, and the commit point is evidence.
	attempt := func(n int) (string, error) {
		id := ulid.New(now.Add(time.Duration(n) * time.Minute))
		return id, exec(`INSERT INTO send_attempts (id, outbox_id, recipients, transport, owner, lease_until)
			VALUES ($1, $2, '{x@example.net}', 'cf', 'test', $3)`, id, outbox, now.Add(time.Hour))
	}
	a1, err := attempt(1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = attempt(2)
	wantCode(t, err, "23505", "second open attempt")
	wantCode(t, exec(`UPDATE send_attempts SET finished_at = now(), outcome = 'accepted' WHERE id = $1`, a1),
		"23514", "accepted without committed_at")
	wantCode(t, exec(`UPDATE send_attempts SET committed_at = now(), finished_at = now(), outcome = 'abandoned' WHERE id = $1`, a1),
		"23514", "abandoned after committed_at")
	if err := exec(`UPDATE send_attempts SET committed_at = now(), finished_at = now(), outcome = 'unknown' WHERE id = $1`, a1); err != nil {
		t.Fatal(err)
	}
	if _, err := attempt(3); err != nil {
		t.Errorf("attempt after the first finished: %v", err)
	}

	// The change log admits the new kinds.
	if err := exec(`INSERT INTO changes (kind, target_id) VALUES ('outbox', $1), ('draft', $1)`, outbox); err != nil {
		t.Error(err)
	}
}
