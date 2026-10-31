package store

import "context"

// SearchDoc exposes the search expression to the external tests, which
// check that it still matches the trigram index.
const SearchDoc = searchDoc

// MigrateTo exposes migrateTo to the migration tests.
func (db *DB) MigrateTo(ctx context.Context, target int) (from, to int, err error) {
	return db.migrateTo(ctx, target)
}
