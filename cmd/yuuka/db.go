package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/store"
)

// dbConfig names the database and, optionally, a second role to migrate
// it with.
//
// A deployment splits the two (deploy/postgres-roles.sh): URL connects as
// an app role that may only read and write rows, OwnerURL as the role
// that owns the schema. Only `yuuka migrate`, a one-shot step, is given
// OwnerURL there; the long-running serve and every other command get URL
// alone, so a process that serves requests never holds a credential that
// can change the schema.
type dbConfig struct {
	URL      string
	OwnerURL string
}

func loadDBConfig(getenv func(string) string) (dbConfig, error) {
	d := dbConfig{URL: getenv("YUUKA_DATABASE_URL"), OwnerURL: getenv("YUUKA_DATABASE_OWNER_URL")}
	if d.URL == "" {
		return d, errors.New("YUUKA_DATABASE_URL is required")
	}
	return d, nil
}

// loadMigrateConfig is loadDBConfig for `yuuka migrate`, which needs
// either URL but not both: it never connects as the app role when it has
// the owner's, so a deployment can give it the owner's alone.
func loadMigrateConfig(getenv func(string) string) (dbConfig, error) {
	d := dbConfig{URL: getenv("YUUKA_DATABASE_URL"), OwnerURL: getenv("YUUKA_DATABASE_OWNER_URL")}
	if d.URL == "" && d.OwnerURL == "" {
		return d, errors.New("YUUKA_DATABASE_OWNER_URL (or, for a single-role database, YUUKA_DATABASE_URL) is required")
	}
	return d, nil
}

// migrateURL is who `yuuka migrate` connects as: the owner when there is
// one, else the one URL (a development database where one role does
// everything).
func (d dbConfig) migrateURL() (url, name string) {
	if d.OwnerURL != "" {
		return d.OwnerURL, "YUUKA_DATABASE_OWNER_URL"
	}
	return d.URL, "YUUKA_DATABASE_URL"
}

// migrate is `yuuka migrate`: apply what is pending and say what changed.
// Running it again, or twice at once, is safe (store.Migrate).
func migrate(ctx context.Context, d dbConfig, out io.Writer) error {
	url, name := d.migrateURL()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return fmt.Errorf("database (%s): %w", name, err)
	}
	defer pool.Close()
	from, to, err := store.New(pool).Migrate(ctx)
	if err != nil {
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "42501" {
			return fmt.Errorf("as %s: %w (this role may not change the schema; set YUUKA_DATABASE_OWNER_URL to the owner's URL)", name, err)
		}
		return fmt.Errorf("as %s: %w", name, err)
	}
	if from == to {
		_, err = fmt.Fprintf(out, "schema version %d: up to date\n", to)
	} else {
		_, err = fmt.Fprintf(out, "schema version %d -> %d\n", from, to)
	}
	return err
}

// openDB returns a pool on URL for a command that works with the data,
// once the schema is at the version this binary knows.
//
// These commands do not migrate, with one exception: when OwnerURL is set
// they first apply pending migrations through it, as `yuuka migrate`
// would. That is a convenience for development and tests; the deployment
// templates never give OwnerURL to serve. Otherwise a schema behind the
// binary is refused with the command that fixes it, and one ahead is
// refused because this binary does not know it.
//
// Whether to migrate is decided by the configuration alone, not by
// probing what the URL's role may do: a lone URL never runs DDL, even
// when it could. A deployment that skips the migrate step then fails the
// same loud way whichever role it connects as, and nothing about serve's
// behaviour depends on a privilege guess.
func openDB(ctx context.Context, d dbConfig) (*pgxpool.Pool, *store.DB, error) {
	if d.OwnerURL != "" {
		if err := migrate(ctx, d, io.Discard); err != nil {
			return nil, nil, err
		}
	}
	pool, err := pgxpool.New(ctx, d.URL)
	if err != nil {
		return nil, nil, fmt.Errorf("database: %w", err)
	}
	db := store.New(pool)
	if err := db.CheckVersion(ctx); err != nil {
		pool.Close()
		if v, ok := errors.AsType[*store.VersionError](err); ok && v.Behind() {
			return nil, nil, fmt.Errorf("%w: run `yuuka migrate` as the schema owner (YUUKA_DATABASE_OWNER_URL), then start again", err)
		}
		return nil, nil, err
	}
	return pool, db, nil
}
