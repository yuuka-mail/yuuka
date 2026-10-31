package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/store"
)

// dbConfig names the database and, optionally, a second role to migrate
// it with.
//
// A deployment can split the two (deploy/postgres-roles.sh): URL connects
// as an app role that may only read and write rows, OwnerURL as the role
// that owns the schema. Migrations then run as the owner and everything
// else, including any SQL a bug lets a request influence, as the app
// role. Without OwnerURL, URL does both, as before the split.
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

// openDB brings the schema up to date and returns a pool on URL. Every
// command does this on start, so a newer binary migrates and an older
// one refuses a database it does not know (store.Migrate).
//
// Without OwnerURL the migration runs on the URL pool itself. If that is
// an app role it still works while there is nothing to apply, since an
// up-to-date Migrate only reads; otherwise it fails on the first DDL.
func openDB(ctx context.Context, d dbConfig) (*pgxpool.Pool, *store.DB, error) {
	if d.OwnerURL != "" {
		owner, err := pgxpool.New(ctx, d.OwnerURL)
		if err != nil {
			return nil, nil, fmt.Errorf("database (owner): %w", err)
		}
		err = store.New(owner).Migrate(ctx)
		owner.Close()
		if err != nil {
			return nil, nil, fmt.Errorf("as YUUKA_DATABASE_OWNER_URL: %w", err)
		}
	}
	pool, err := pgxpool.New(ctx, d.URL)
	if err != nil {
		return nil, nil, fmt.Errorf("database: %w", err)
	}
	db := store.New(pool)
	if d.OwnerURL == "" {
		if err := db.Migrate(ctx); err != nil {
			pool.Close()
			if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "42501" {
				return nil, nil, fmt.Errorf("%w (YUUKA_DATABASE_URL may not change the schema; set YUUKA_DATABASE_OWNER_URL)", err)
			}
			return nil, nil, err
		}
	}
	return pool, db, nil
}
