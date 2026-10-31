package store

import (
	"context"
	"fmt"
	"time"

	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Account kinds (accounts.kind).
const AccountCloudflare = "cf_catchall"

// EnsureCloudflareAccount returns the id of the Cloudflare catch-all
// account called name, creating it on first use. The process
// configuration (YUUKA_ACCOUNT) declares that account, so its row follows
// the configuration instead of needing a command of its own. A different
// kind under the same name is an error, not a silent reuse.
func (db *DB) EnsureCloudflareAccount(ctx context.Context, name string) (string, error) {
	// Two statements, not one with a CTE: a concurrent first insert makes
	// ON CONFLICT wait and skip, and only a later statement's snapshot sees
	// the row it committed.
	if _, err := db.pool.Exec(ctx, `
		INSERT INTO accounts (id, name, kind) VALUES ($1, $2, $3)
		ON CONFLICT (name) DO NOTHING`,
		ulid.New(time.Now()), name, AccountCloudflare); err != nil {
		return "", fmt.Errorf("account %q: %w", name, err)
	}
	var id, kind string
	err := db.pool.QueryRow(ctx, `SELECT id, kind FROM accounts WHERE name = $1`, name).Scan(&id, &kind)
	if err != nil {
		return "", fmt.Errorf("account %q: %w", name, err)
	}
	if kind != AccountCloudflare {
		return "", fmt.Errorf("account %q is a %s account, not %s", name, kind, AccountCloudflare)
	}
	return id, nil
}
