package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Migrations are append-only: a committed file is never edited, because
// Postgres holds data that cannot be rebuilt (§4.2). Files are named
// NNNN_description.sql and applied in order, each in its own transaction.
//
//go:embed migrations/*.sql
var migrations embed.FS

// migrationLock serializes concurrent migrators (two processes starting at
// once, or parallel tests) on one database.
const migrationLock = 0x7975756b61 // "yuuka"

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, n := range names {
		base := strings.TrimPrefix(n, "migrations/")
		num, _, ok := strings.Cut(base, "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v <= 0 {
			return nil, fmt.Errorf("migration %s: name must start with a positive number", base)
		}
		body, err := fs.ReadFile(migrations, n)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{v, base, string(body)})
	}
	slices.SortFunc(out, func(a, b migration) int { return a.version - b.version })
	for i := range out {
		if out[i].version != i+1 {
			return nil, fmt.Errorf("migration %s: versions must be 1..n without gaps", out[i].name)
		}
	}
	return out, nil
}

// VersionError says the database's schema version is not the one this
// binary was built for. Ahead, nothing this binary does is safe: it does
// not know the newer tables' invariants. Behind, Migrate fixes it.
type VersionError struct {
	Current int // recorded in schema_migrations; 0 for an empty database
	Latest  int // the newest migration this binary carries
}

func (e *VersionError) Error() string {
	if e.Current > e.Latest {
		return fmt.Sprintf("database is at version %d, this binary only knows %d", e.Current, e.Latest)
	}
	return fmt.Sprintf("database is at version %d, this binary needs %d", e.Current, e.Latest)
}

// Behind reports whether Migrate would bring the database up to date.
func (e *VersionError) Behind() bool { return e.Current < e.Latest }

// CheckVersion returns a *VersionError unless the database is at exactly
// the version this binary knows. It only reads, so any role that may
// read schema_migrations can run it; this is how the commands that never
// migrate find out that they should not start.
func (db *DB) CheckVersion(ctx context.Context) error {
	ms, err := loadMigrations()
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	_, current, err := schemaVersion(ctx, db.pool)
	if err != nil {
		return fmt.Errorf("schema version: %w", err)
	}
	if current != len(ms) {
		return &VersionError{Current: current, Latest: len(ms)}
	}
	return nil
}

// schemaVersion reads the recorded version; 0 when schema_migrations
// does not exist yet.
//
// It looks before reading: CREATE TABLE IF NOT EXISTS needs CREATE on the
// schema even when the table is there, and the app role
// (deploy/postgres-roles.sh) has none. An up-to-date database thus
// migrates as a read-only version check under any role; only the owner
// can apply something.
func schemaVersion(ctx context.Context, q querier) (exists bool, current int, err error) {
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_tables
		WHERE schemaname = current_schema() AND tablename = 'schema_migrations')`).Scan(&exists); err != nil {
		return false, 0, err
	}
	if !exists {
		return false, 0, nil
	}
	if err := q.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return true, 0, err
	}
	return true, current, nil
}

// Migrate applies every migration not yet recorded in schema_migrations
// and returns the version before and after. It refuses (*VersionError)
// to run against a database migrated by a newer binary. Concurrent
// callers on one database queue on an advisory lock, so the first
// applies and the rest find nothing to do.
func (db *DB) Migrate(ctx context.Context) (from, to int, err error) {
	ms, err := loadMigrations()
	if err != nil {
		return 0, 0, fmt.Errorf("load migrations: %w", err)
	}
	conn, err := db.pool.Acquire(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("migrate: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLock); err != nil {
		return 0, 0, fmt.Errorf("migrate: lock: %w", err)
	}
	defer func() {
		// The lock is session-scoped; release it even if ctx is done.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLock)
	}()

	exists, current, err := schemaVersion(ctx, conn)
	if err != nil {
		return 0, 0, fmt.Errorf("migrate: %w", err)
	}
	if !exists {
		if _, err := conn.Exec(ctx, `CREATE TABLE schema_migrations (
			version    integer PRIMARY KEY,
			name       text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
			return current, current, fmt.Errorf("migrate: %w", err)
		}
	}
	if current > len(ms) {
		return current, current, fmt.Errorf("migrate: %w", &VersionError{Current: current, Latest: len(ms)})
	}
	from = current
	for _, m := range ms[current:] {
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, m.version, m.name)
			return err
		})
		if err != nil {
			return from, current, fmt.Errorf("migrate: apply %s: %w", m.name, err)
		}
		current = m.version
	}
	return from, current, nil
}
