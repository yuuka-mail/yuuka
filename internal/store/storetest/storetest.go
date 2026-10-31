// Package storetest gives each test its own migrated Postgres schema, so
// tests can run in parallel against the one test database.
package storetest

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// EnvURL names the variable that points tests at a disposable database.
const EnvURL = "YUUKA_TEST_DATABASE_URL"

// New returns a migrated DB in a fresh schema plus its pool for direct
// queries. It skips the test when EnvURL is unset.
func New(t *testing.T) (*store.DB, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv(EnvURL)
	if url == "" {
		t.Skip(EnvURL + " not set")
	}
	ctx := context.Background()
	schema := "test_" + strings.ToLower(ulid.New(time.Now()))

	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(ctx) }()
	// Extensions are per database, not per schema. Create them once in
	// public, which every test's search_path includes, so the migration's
	// CREATE EXTENSION IF NOT EXISTS is a no-op instead of installing
	// into (and being dropped with) one test's schema. The lock keeps
	// parallel test binaries from racing on the catalog; the statements
	// run separately so the creation commits before the lock is released.
	for _, q := range []string{
		`SELECT pg_advisory_lock(31432)`,
		`CREATE EXTENSION IF NOT EXISTS pg_trgm SCHEMA public`,
		`SELECT pg_advisory_unlock(31432)`,
	} {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), url)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = c.Close(context.Background()) }()
		if _, err := c.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ", public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	db := store.New(pool)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return db, pool
}
