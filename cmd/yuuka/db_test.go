package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/store/storetest"
)

// The deployment shape: migrate runs once as the owner; serve and the
// rest connect as the app role and only check the version.
func TestMigrateThenStart(t *testing.T) {
	t.Parallel()
	ownerURL, appURL := storetest.URLs(t)
	app := dbConfig{URL: appURL}

	// Empty schema, app role only: refused with what to do, and no DDL.
	before := catalog(t, ownerURL)
	if _, _, err := openDB(t.Context(), app); err == nil || !strings.Contains(err.Error(), "yuuka migrate") {
		t.Fatalf("app role on an empty schema: %v", err)
	}
	// A lone URL never migrates, even one that could: that is decided by
	// the configuration, not by the role's privileges.
	if _, _, err := openDB(t.Context(), dbConfig{URL: ownerURL}); err == nil || !strings.Contains(err.Error(), "yuuka migrate") {
		t.Fatalf("owner URL alone on an empty schema: %v", err)
	}
	if after := catalog(t, ownerURL); after != before {
		t.Fatalf("refused start changed the schema: %s -> %s", before, after)
	}

	// migrate with only the app role says what to set.
	var out bytes.Buffer
	if err := migrate(t.Context(), app, &out); err == nil || !strings.Contains(err.Error(), "YUUKA_DATABASE_OWNER_URL") {
		t.Fatalf("migrate as the app role: %v", err)
	}

	// migrate as the owner applies everything, then nothing.
	split := dbConfig{URL: appURL, OwnerURL: ownerURL}
	out.Reset()
	if err := migrate(t.Context(), split, &out); err != nil || !strings.HasPrefix(out.String(), "schema version 0 -> ") {
		t.Fatalf("first migrate: %q %v", out.String(), err)
	}
	out.Reset()
	if err := migrate(t.Context(), split, &out); err != nil || !strings.HasSuffix(out.String(), ": up to date\n") {
		t.Fatalf("second migrate: %q %v", out.String(), err)
	}

	// Up to date, the app role starts and the pool is the app role's.
	pool, _, err := openDB(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	var user, owner string
	err = pool.QueryRow(t.Context(), `SELECT current_user, tableowner FROM pg_tables
		WHERE schemaname = current_schema() AND tablename = 'messages'`).Scan(&user, &owner)
	pool.Close()
	if err != nil || user != storetest.AppRole || owner != storetest.OwnerRole {
		t.Fatalf("connected as %q, messages owned by %q, %v", user, owner, err)
	}

	// An upgrade: the binary knows a migration the database lacks
	// (simulated by forgetting the last one). The app role refuses to
	// start and changes nothing.
	sqlExec(t, ownerURL, `DELETE FROM schema_migrations WHERE version = (SELECT max(version) FROM schema_migrations)`)
	before = catalog(t, ownerURL)
	_, _, err = openDB(t.Context(), app)
	if err == nil || !strings.Contains(err.Error(), "this binary needs") || !strings.Contains(err.Error(), "yuuka migrate") {
		t.Fatalf("app role on an outdated schema: %v", err)
	}
	if after := catalog(t, ownerURL); after != before {
		t.Fatalf("refused start changed the schema: %s -> %s", before, after)
	}

	// Ahead: refused, and migrate does not help.
	sqlExec(t, ownerURL, `INSERT INTO schema_migrations (version, name) VALUES (999, 'future.sql')`)
	if _, _, err := openDB(t.Context(), app); err == nil || !strings.Contains(err.Error(), "only knows") || strings.Contains(err.Error(), "yuuka migrate") {
		t.Fatalf("app role on a newer schema: %v", err)
	}
	if err := migrate(t.Context(), split, io.Discard); err == nil || !strings.Contains(err.Error(), "only knows") {
		t.Fatalf("migrate on a newer schema: %v", err)
	}
}

// With the owner URL set, any command migrates on start as before: a
// convenience for development, not what the deployment templates do.
func TestOpenDBMigratesWithOwnerURL(t *testing.T) {
	t.Parallel()
	ownerURL, appURL := storetest.URLs(t)
	pool, _, err := openDB(t.Context(), dbConfig{URL: appURL, OwnerURL: ownerURL})
	if err != nil {
		t.Fatal(err)
	}
	var user string
	err = pool.QueryRow(t.Context(), `SELECT current_user`).Scan(&user)
	pool.Close()
	if err != nil || user != storetest.AppRole {
		t.Fatalf("connected as %q, %v", user, err)
	}
}

// catalog fingerprints what DDL could change in the test's schema, to
// show that a refused start changed nothing.
func catalog(t *testing.T, url string) string {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(t.Context()) }()
	var rels, rows string
	err = conn.QueryRow(t.Context(), `SELECT count(*) || ':' || coalesce(string_agg(c.relname || '/' || c.relkind::text, ',' ORDER BY c.relname), '')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = current_schema()`).Scan(&rels)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rels, "schema_migrations/") {
		err = conn.QueryRow(t.Context(), `SELECT coalesce(string_agg(version::text, ',' ORDER BY version), '') FROM schema_migrations`).Scan(&rows)
		if err != nil {
			t.Fatal(err)
		}
	}
	return rels + " versions=" + rows
}

func sqlExec(t *testing.T, url, q string) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(t.Context()) }()
	if _, err := conn.Exec(t.Context(), q); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDBConfig(t *testing.T) {
	t.Parallel()
	only := func(k, v string) func(string) string {
		return func(name string) string {
			if name == k {
				return v
			}
			return ""
		}
	}
	if _, err := loadDBConfig(only("YUUKA_DATABASE_OWNER_URL", "postgres://o@localhost/yuuka")); err == nil {
		t.Error("owner URL without YUUKA_DATABASE_URL accepted")
	}
	// migrate alone may get the owner's URL only, and then uses it.
	d, err := loadMigrateConfig(only("YUUKA_DATABASE_OWNER_URL", "postgres://o@localhost/yuuka"))
	if url, name := d.migrateURL(); err != nil || url != "postgres://o@localhost/yuuka" || name != "YUUKA_DATABASE_OWNER_URL" {
		t.Errorf("migrate, owner only: %+v %v", d, err)
	}
	if _, err := loadMigrateConfig(only("", "")); err == nil {
		t.Error("migrate without any URL accepted")
	}
	// Unset, migrate connects as the one URL.
	d, err = loadDBConfig(only("YUUKA_DATABASE_URL", "postgres://a@localhost/yuuka"))
	if url, _ := d.migrateURL(); err != nil || d.OwnerURL != "" || url != d.URL {
		t.Errorf("single DSN: %+v %v", d, err)
	}
	if c, err := loadConfig(env(map[string]string{"YUUKA_DATABASE_OWNER_URL": "postgres://o@localhost/yuuka"}), true); err != nil ||
		c.DB.OwnerURL != "postgres://o@localhost/yuuka" || c.DB.URL != "postgres://localhost/yuuka" {
		t.Errorf("serve config: %+v %v", c.DB, err)
	}
}
