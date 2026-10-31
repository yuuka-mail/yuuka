package store

import (
	"context"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/search"
)

// CompileSearch exposes the filter compiler to the external tests, which
// check that its text conditions still match the trigram index.
func (db *DB) CompileSearch(v *auth.Viewer, f *search.Filter) (where string, args []any, err error) {
	c := &compiler{db: db, v: v}
	where, err = c.filter(f)
	return where, c.args, err
}

// EnforceAccountAccess makes db filter every read by account_access, as
// M3.5 will, so the visibility tests can use restricted viewers now.
func (db *DB) EnforceAccountAccess() { db.enforceAccountAccess = true }

// MigrateTo exposes migrateTo to the migration tests.
func (db *DB) MigrateTo(ctx context.Context, target int) (from, to int, err error) {
	return db.migrateTo(ctx, target)
}

// HoldChange logs a change of kind/id through the writer every change
// goes through, without taking contentLock first, and leaves its
// transaction open until commit is called: a writer that commits late.
func (db *DB) HoldChange(ctx context.Context, kind, id string) (commit func() error, err error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	w := writer{tx: tx}
	w.change(kind, id)
	if err := w.flush(ctx); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return func() error { return tx.Commit(ctx) }, nil
}
