package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Account kinds (accounts.kind).
const (
	AccountCloudflare = "cf_catchall"
	// AccountIMAP is a pull account (§9.5); its source is in
	// account_sources (pull.go).
	AccountIMAP = "imap"
)

// EnsureCloudflareAccount returns the id of the Cloudflare catch-all
// account called name, creating it on first use. The process
// configuration (YUUKA_ACCOUNT) declares that account, so its row follows
// the configuration instead of needing a command of its own. A different
// kind under the same name is an error, not a silent reuse.
//
// A Cloudflare account is readable by default (§13.8): it is the
// deployment's own domains, and every principal read it before
// account_access was enforced.
func (db *DB) EnsureCloudflareAccount(ctx context.Context, name string) (string, error) {
	// Two statements, not one with a CTE: a concurrent first insert makes
	// ON CONFLICT wait and skip, and only a later statement's snapshot sees
	// the row it committed.
	if _, err := db.pool.Exec(ctx, `
		INSERT INTO accounts (id, name, kind, default_read) VALUES ($1, $2, $3, true)
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

// Account is an accounts row. Configuration: the methods returning it
// take no Viewer and are for the host's CLI and human sessions (§13.10);
// what any principal may learn about accounts is VisibleAccounts.
type Account struct {
	ID         string
	Name       string
	Kind       string
	SyncPolicy string
	// DefaultRead is whether a principal without an account_access row
	// may read the account (§13.8).
	DefaultRead bool
	DisabledAt  *time.Time
	CreatedAt   time.Time
}

const accountCols = `a.id, a.name, a.kind, a.sync_policy, a.default_read, a.disabled_at, a.created_at`

func scanAccount(row pgx.Row) (Account, error) {
	var a Account
	err := row.Scan(&a.ID, &a.Name, &a.Kind, &a.SyncPolicy, &a.DefaultRead, &a.DisabledAt, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// Accounts lists every account by name, removed ones included.
func (db *DB) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := db.pool.Query(ctx, `SELECT `+accountCols+` FROM accounts a ORDER BY a.name`)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Account, error) { return scanAccount(r) })
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	return out, nil
}

// AccountByName returns the account called name, removed or not.
func (db *DB) AccountByName(ctx context.Context, name string) (Account, error) {
	a, err := scanAccount(db.pool.QueryRow(ctx, `SELECT `+accountCols+` FROM accounts a WHERE a.name = $1`, name))
	if err != nil {
		return a, fmt.Errorf("account %s: %w", name, err)
	}
	return a, nil
}

// VisibleAccount is what any principal may learn about an account it can
// read (§9.4): its name, kind and, for a pull account, the mailbox
// address. Never the host, username or sync state.
type VisibleAccount struct {
	Name string
	Kind string
	// Address is the pull source's mailbox address; "" for a Cloudflare
	// account, which has many.
	Address string
}

// VisibleAccounts lists the accounts v may read, by name: the account
// list of MCP list_accounts and REST GET /api/v1/accounts (§13.8).
func (db *DB) VisibleAccounts(ctx context.Context, v *auth.Viewer) ([]VisibleAccount, error) {
	if err := needViewer(v, "accounts"); err != nil {
		return nil, err
	}
	rows, err := db.pool.Query(ctx, `
		SELECT a.name, a.kind, coalesce(s.address, '')
		FROM accounts a LEFT JOIN account_sources s ON s.account_id = a.id
		WHERE `+db.readableAccount(v, "a.id")+`
		ORDER BY a.name`)
	if err != nil {
		return nil, fmt.Errorf("visible accounts: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[VisibleAccount])
	if err != nil {
		return nil, fmt.Errorf("visible accounts: %w", err)
	}
	return out, nil
}

// AccessRow is an account_access row: principal may (CanRead) or may
// not read the account, whatever its default (§13.8).
type AccessRow struct {
	PrincipalID   string
	PrincipalKind string
	PrincipalName string
	CanRead       bool
}

// AccountAccess lists the account_access rows of account name, by
// principal, for `yuuka account access <name>`.
func (db *DB) AccountAccess(ctx context.Context, name string) ([]AccessRow, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT p.id, p.kind, p.name, x.can_read
		FROM account_access x JOIN accounts a ON a.id = x.account_id JOIN principals p ON p.id = x.principal_id
		WHERE a.name = $1
		ORDER BY p.kind, p.name`, name)
	if err != nil {
		return nil, fmt.Errorf("access to account %s: %w", name, err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[AccessRow])
	if err != nil {
		return nil, fmt.Errorf("access to account %s: %w", name, err)
	}
	return out, nil
}

// SetAccountAccess writes principal's row for account name and records
// a: canRead true or false overrides the account's default either way;
// nil deletes the row, so the default applies again.
//
// Whether this widens or tightens (§13.10) is the caller's to decide,
// from the account's default and the principal's row as it read them.
// defaultRead and was are those, and the write is made only while both
// still hold (ErrStale otherwise): a change made in between cannot turn
// a write decided as a tightening into an unannounced widening. The
// account row is locked for the check, which orders this with
// SetAccountDefaultRead and every other access write to the account.
// ErrNotFound if the account or the principal does not exist.
func (db *DB) SetAccountAccess(ctx context.Context, name, principalID string, defaultRead bool, was, canRead *bool, a Action) error {
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		var accountID string
		var curDefault bool
		var one int
		// NO KEY UPDATE waits for SetAccountDefaultRead and the other
		// access writes, never for a delivery that references the
		// account (FOR KEY SHARE).
		if err := tx.QueryRow(ctx, `SELECT id, default_read FROM accounts WHERE name = $1 FOR NO KEY UPDATE`, name).
			Scan(&accountID, &curDefault); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT 1 FROM principals WHERE id = $1`, principalID).Scan(&one); errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("principal %s: %w", principalID, ErrNotFound)
		} else if err != nil {
			return err
		}
		var cur *bool
		if err := tx.QueryRow(ctx, `SELECT can_read FROM account_access WHERE account_id = $1 AND principal_id = $2`,
			accountID, principalID).Scan(&cur); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if curDefault != defaultRead || !sameRow(cur, was) {
			return fmt.Errorf("default %v and row %s, read as %v and %s: %w", curDefault, rowText(cur), defaultRead, rowText(was), ErrStale)
		}
		var err error
		if canRead == nil {
			_, err = tx.Exec(ctx, `DELETE FROM account_access WHERE account_id = $1 AND principal_id = $2`, accountID, principalID)
		} else {
			_, err = tx.Exec(ctx, `
				INSERT INTO account_access (account_id, principal_id, can_read) VALUES ($1, $2, $3)
				ON CONFLICT (account_id, principal_id) DO UPDATE SET can_read = excluded.can_read`,
				accountID, principalID, *canRead)
		}
		if err != nil {
			return err
		}
		return insertAction(ctx, tx, a)
	})
	if err != nil {
		return fmt.Errorf("set access to account %s: %w", name, err)
	}
	return nil
}

func sameRow(a, b *bool) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

func rowText(r *bool) string {
	if r == nil {
		return "none"
	}
	return fmt.Sprint(*r)
}

// SetAccountDefaultRead sets whether principals without a row may read
// account name (§13.8), and records a, while its default is still was,
// as the caller read it (ErrStale otherwise; see SetAccountAccess).
// ErrNotFound if it does not exist.
func (db *DB) SetAccountDefaultRead(ctx context.Context, name string, was, defaultRead bool, a Action) error {
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		var cur bool
		if err := tx.QueryRow(ctx, `SELECT default_read FROM accounts WHERE name = $1 FOR NO KEY UPDATE`, name).Scan(&cur); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if cur != was {
			return fmt.Errorf("default %v, read as %v: %w", cur, was, ErrStale)
		}
		if _, err := tx.Exec(ctx, `UPDATE accounts SET default_read = $2 WHERE name = $1`, name, defaultRead); err != nil {
			return err
		}
		return insertAction(ctx, tx, a)
	})
	if err != nil {
		return fmt.Errorf("set default read of account %s: %w", name, err)
	}
	return nil
}
