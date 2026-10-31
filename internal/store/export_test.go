package store

import "context"

// SearchDoc exposes the search expression to the external tests, which
// check that it still matches the trigram index.
const SearchDoc = searchDoc

// EnforceAccountAccess makes db filter every read by account_access, as
// M3.5 will, so the visibility tests can use restricted viewers now.
func (db *DB) EnforceAccountAccess() { db.enforceAccountAccess = true }

// MigrateTo exposes migrateTo to the migration tests.
func (db *DB) MigrateTo(ctx context.Context, target int) (from, to int, err error) {
	return db.migrateTo(ctx, target)
}
