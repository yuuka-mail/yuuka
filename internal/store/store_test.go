package store_test

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

func record(n int, contentHash [32]byte, to string) store.Ingest {
	at := time.Date(2026, 10, 31, 1, 0, n, 0, time.UTC)
	id := ulid.New(at)
	key := fmt.Sprintf("raw/2026/10/%s.eml", id)
	return store.Ingest{
		RawID: id, R2Key: key, LocalPath: key, Size: 10,
		SHA256:      sha256.Sum256(fmt.Appendf(nil, "raw-%d", n)),
		ContentHash: contentHash, ReceivedAt: at,
		DeliveryID: ulid.New(at), Account: "cf", Zone: "example.com", Prefix: "a",
		EnvelopeFrom: "s@example.net", EnvelopeTo: to,
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	t.Parallel()
	ownerURL, _ := storetest.URLs(t)
	pool, err := pgxpool.New(t.Context(), ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	db := store.New(pool)

	from, latest, err := db.Migrate(t.Context())
	if err != nil || from != 0 || latest == 0 {
		t.Fatalf("first migrate: %d -> %d, %v", from, latest, err)
	}
	from, to, err := db.Migrate(t.Context())
	if err != nil || from != latest || to != latest {
		t.Fatalf("second migrate: %d -> %d, want %d -> %[3]d, %v", from, to, latest, err)
	}
	// Applied once each: one row per version, versions 1..n.
	var n, maxVersion int
	err = pool.QueryRow(t.Context(), `SELECT count(*), max(version) FROM schema_migrations`).Scan(&n, &maxVersion)
	if err != nil || n != latest || maxVersion != latest {
		t.Fatalf("schema_migrations rows = %d, max version %d, want %d, %v", n, maxVersion, latest, err)
	}
}

// Two migrators starting at once (two `yuuka migrate` runs, or migrate
// racing a dev serve) queue on the advisory lock: one applies everything,
// the others find it done, and none fails.
func TestConcurrentMigrate(t *testing.T) {
	t.Parallel()
	ownerURL, _ := storetest.URLs(t)
	const n = 4
	var wg sync.WaitGroup
	froms, tos := make([]int, n), make([]int, n)
	errs := make([]error, n)
	for i := range n {
		// A pool each: separate sessions, as separate processes have.
		pool, err := pgxpool.New(t.Context(), ownerURL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		wg.Go(func() { froms[i], tos[i], errs[i] = store.New(pool).Migrate(t.Context()) })
	}
	wg.Wait()
	applied := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if tos[i] != tos[0] {
			t.Errorf("migrator %d ended at %d, another at %d", i, tos[i], tos[0])
		}
		if froms[i] == 0 {
			applied++
		} else if froms[i] != tos[i] {
			t.Errorf("migrator %d applied %d -> %d after another started", i, froms[i], tos[i])
		}
	}
	if applied != 1 {
		t.Errorf("%d migrators started from an empty schema, want 1", applied)
	}
}

// CheckVersion is what every command but migrate runs on start: it reads
// and never writes, so the app role can run it, and it tells behind from
// ahead.
func TestCheckVersion(t *testing.T) {
	t.Parallel()
	ownerURL, appURL := storetest.URLs(t)
	owner, err := pgxpool.New(t.Context(), ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	app, err := pgxpool.New(t.Context(), appURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	db := store.New(app)

	// Empty: version 0, behind.
	v, ok := errors.AsType[*store.VersionError](db.CheckVersion(t.Context()))
	if !ok || v.Current != 0 || !v.Behind() {
		t.Fatalf("empty schema: %+v", v)
	}
	_, latest, err := store.New(owner).Migrate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CheckVersion(t.Context()); err != nil {
		t.Fatalf("up to date: %v", err)
	}
	// Ahead: a newer binary migrated it. Migrate refuses too.
	if _, err := owner.Exec(t.Context(), `INSERT INTO schema_migrations (version, name) VALUES ($1, 'future.sql')`, latest+1); err != nil {
		t.Fatal(err)
	}
	v, ok = errors.AsType[*store.VersionError](db.CheckVersion(t.Context()))
	if !ok || v.Current != latest+1 || v.Behind() {
		t.Fatalf("ahead: %+v", v)
	}
	if _, _, err := store.New(owner).Migrate(t.Context()); !errors.As(err, &v) {
		t.Fatalf("migrate ahead: %v", err)
	}
}

// storetest connects as an app role with row privileges only, as a
// deployment does (deploy/postgres-roles.sh). Every other test in the
// tree shows that runtime operations work under it; this one shows that
// it really is that narrow: no DDL, and so no migrating either.
func TestAppRoleRefusesDDL(t *testing.T) {
	t.Parallel()
	db, pool := storetest.New(t)
	var super bool
	if err := pool.QueryRow(t.Context(), `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super); err != nil || super {
		t.Fatalf("app role is superuser: %v %v", super, err)
	}
	for _, q := range []string{
		`CREATE TABLE intruder (a int)`,
		`CREATE INDEX ON messages (id)`,
		`ALTER TABLE messages ADD COLUMN intruder int`,
		`DROP TABLE messages`,
		`TRUNCATE deliveries`,
		`CREATE SCHEMA intruder`,
		`CREATE FUNCTION intruder() RETURNS int LANGUAGE sql AS 'SELECT 1'`,
	} {
		_, err := pool.Exec(t.Context(), q)
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != "42501" {
			t.Errorf("%s: %v, want insufficient_privilege", q, err)
		}
	}

	// Up to date, Migrate only reads, so even the app role passes it.
	// With something left to apply it fails instead of half-applying.
	if _, _, err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("up-to-date migrate as app role: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `DELETE FROM schema_migrations WHERE version = (SELECT max(version) FROM schema_migrations)`); err != nil {
		t.Fatal(err)
	}
	_, _, err := db.Migrate(t.Context())
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != "42501" {
		t.Errorf("migrate as app role: %v, want insufficient_privilege", err)
	}
}

// Two copies of one message to one address, committed concurrently, must
// yield exactly one delivery whichever transaction wins.
func TestConcurrentRedeliveries(t *testing.T) {
	t.Parallel()
	db, pool := storetest.New(t)
	ch := sha256.Sum256([]byte("content"))
	const n = 8
	var wg sync.WaitGroup
	outcomes := make([]store.Outcome, n)
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			outcomes[i], errs[i] = db.CommitIngest(t.Context(), record(i, ch, "a@example.com"))
		})
	}
	wg.Wait()
	delivered := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if outcomes[i] == store.Delivered {
			delivered++
		}
	}
	if delivered != 1 {
		t.Fatalf("delivered %d times, want 1 (%v)", delivered, outcomes)
	}
	var deliveries, dups int
	err := pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM deliveries),
		(SELECT count(*) FROM raw_objects r JOIN deliveries d ON d.raw_id = r.duplicate_of)`).Scan(&deliveries, &dups)
	if err != nil || deliveries != 1 || dups != n-1 {
		t.Fatalf("deliveries=%d duplicates=%d err=%v", deliveries, dups, err)
	}
}

func TestConcurrentSameRaw(t *testing.T) {
	t.Parallel()
	db, _ := storetest.New(t)
	rec := record(1, sha256.Sum256([]byte("c")), "a@example.com")
	var wg sync.WaitGroup
	outcomes := make([]store.Outcome, 4)
	for i := range outcomes {
		wg.Go(func() {
			var err error
			if outcomes[i], err = db.CommitIngest(t.Context(), rec); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	n := 0
	for _, o := range outcomes {
		if o == store.Delivered {
			n++
		} else if o != store.AlreadyIngested {
			t.Errorf("unexpected outcome %v", o)
		}
	}
	if n != 1 {
		t.Fatalf("outcomes %v", outcomes)
	}
}

func TestSameIDDifferentBytesFails(t *testing.T) {
	t.Parallel()
	db, _ := storetest.New(t)
	rec := record(1, sha256.Sum256([]byte("c")), "a@example.com")
	if _, err := db.CommitIngest(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	rec.SHA256 = sha256.Sum256([]byte("other bytes"))
	if _, err := db.CommitIngest(t.Context(), rec); err == nil {
		t.Fatal("same id with different bytes was accepted")
	}
}
