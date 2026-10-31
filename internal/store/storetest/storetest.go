// Package storetest gives each test its own migrated Postgres schema, so
// tests can run in parallel against the one test database.
//
// Like a deployment (deploy/postgres-roles.sh), the schema is migrated by
// an owner role and the returned pool connects as an app role that may
// only read and write rows. Every test using this package therefore also
// shows that what it exercises needs no DDL.
package storetest

import (
	"context"
	"net/url"
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

// The test roles. Roles are per cluster, so these are shared by every
// test and created once; what each test owns is its schema.
const (
	OwnerRole = "yuuka_test_owner"
	AppRole   = "yuuka_test_app"
)

// New returns a migrated DB in a fresh schema plus its pool for direct
// queries, both connected as AppRole. It skips the test when EnvURL is
// unset.
func New(t *testing.T) (*store.DB, *pgxpool.Pool) {
	t.Helper()
	ownerURL, appURL := URLs(t)
	ctx := context.Background()
	owner, err := pgxpool.New(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	err = store.New(owner).Migrate(ctx)
	owner.Close()
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, appURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return store.New(pool), pool
}

// URLs creates a fresh, empty schema and returns connection URLs to it as
// OwnerRole and as AppRole. It skips the test when EnvURL is unset.
func URLs(t *testing.T) (ownerURL, appURL string) {
	t.Helper()
	base := os.Getenv(EnvURL)
	if base == "" {
		t.Skip(EnvURL + " not set")
	}
	ctx := context.Background()
	schema := "test_" + strings.ToLower(ulid.New(time.Now()))
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	// The roles log in with the superuser's password: the test database
	// is disposable, and this needs no second secret.
	password, _ := u.User.Password()

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(ctx) }()
	exec := func(q string) {
		t.Helper()
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	// Extensions are per database, not per schema. Create them once in
	// public, which every test's search_path includes, so the migration's
	// CREATE EXTENSION IF NOT EXISTS is a no-op instead of installing
	// into (and being dropped with) one test's schema. Roles are per
	// cluster and created once too. The lock keeps parallel test
	// binaries from racing on the catalog; the statements run separately
	// so each commits before the lock is released.
	exec(`SELECT pg_advisory_lock(31432)`)
	exec(`CREATE EXTENSION IF NOT EXISTS pg_trgm SCHEMA public`)
	for _, role := range []string{OwnerRole, AppRole} {
		var exists bool
		if err := admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, role).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			exec("CREATE ROLE " + ident(role) + " LOGIN PASSWORD " + literal(password))
		}
	}
	exec(`SELECT pg_advisory_unlock(31432)`)
	// The grants of deploy/postgres-roles.sh, on this schema instead of
	// the database's public one.
	exec("CREATE SCHEMA " + schema + " AUTHORIZATION " + ident(OwnerRole))
	exec("GRANT USAGE ON SCHEMA " + schema + " TO " + ident(AppRole))
	exec("ALTER DEFAULT PRIVILEGES FOR ROLE " + ident(OwnerRole) + " IN SCHEMA " + schema +
		" GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO " + ident(AppRole))
	exec("ALTER DEFAULT PRIVILEGES FOR ROLE " + ident(OwnerRole) + " IN SCHEMA " + schema +
		" GRANT USAGE ON SEQUENCES TO " + ident(AppRole))
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), base)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = c.Close(context.Background()) }()
		if _, err := c.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})

	as := func(role string) string {
		r := *u
		r.User = url.UserPassword(role, password)
		q := r.Query()
		// pgx passes unknown parameters to the server as settings. No
		// space: pgx does not decode the "+" Encode would write for it.
		q.Set("search_path", schema+",public")
		r.RawQuery = q.Encode()
		return r.String()
	}
	return as(OwnerRole), as(AppRole)
}

func ident(s string) string { return pgx.Identifier{s}.Sanitize() }

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
