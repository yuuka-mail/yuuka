package main

import (
	"strings"
	"testing"

	"github.com/yuuka-mail/yuuka/internal/store/storetest"
)

func TestOpenDBSplitRoles(t *testing.T) {
	t.Parallel()
	ownerURL, appURL := storetest.URLs(t)

	// The app role alone cannot create the schema, and says what to set.
	if _, _, err := openDB(t.Context(), dbConfig{URL: appURL}); err == nil || !strings.Contains(err.Error(), "YUUKA_DATABASE_OWNER_URL") {
		t.Fatalf("app role on an empty schema: %v", err)
	}

	// With the owner URL, migrations run as the owner and the pool
	// handed out is the app role's.
	pool, _, err := openDB(t.Context(), dbConfig{URL: appURL, OwnerURL: ownerURL})
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

	// Once up to date, the app role alone starts too: Migrate only reads.
	pool, _, err = openDB(t.Context(), dbConfig{URL: appURL})
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
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
	// Unset, the one URL does both, as before the split.
	d, err := loadDBConfig(only("YUUKA_DATABASE_URL", "postgres://a@localhost/yuuka"))
	if err != nil || d.OwnerURL != "" {
		t.Errorf("single DSN: %+v %v", d, err)
	}
	if c, err := loadConfig(env(map[string]string{"YUUKA_DATABASE_OWNER_URL": "postgres://o@localhost/yuuka"}), true); err != nil ||
		c.DB.OwnerURL != "postgres://o@localhost/yuuka" || c.DB.URL != "postgres://localhost/yuuka" {
		t.Errorf("serve config: %+v %v", c.DB, err)
	}
}
