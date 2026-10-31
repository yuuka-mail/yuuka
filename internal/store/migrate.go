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

// Migrate applies every migration not yet recorded in schema_migrations.
// It refuses to run against a database migrated by a newer binary.
func (db *DB) Migrate(ctx context.Context) error {
	ms, err := loadMigrations()
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	conn, err := db.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLock); err != nil {
		return fmt.Errorf("migrate: lock: %w", err)
	}
	defer func() {
		// The lock is session-scoped; release it even if ctx is done.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLock)
	}()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    integer PRIMARY KEY,
		name       text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	var current int
	if err := conn.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if current > len(ms) {
		return fmt.Errorf("migrate: database is at version %d, this binary only knows %d", current, len(ms))
	}
	for _, m := range ms[current:] {
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, m.version, m.name)
			return err
		})
		if err != nil {
			return fmt.Errorf("migrate: apply %s: %w", m.name, err)
		}
	}
	return nil
}
