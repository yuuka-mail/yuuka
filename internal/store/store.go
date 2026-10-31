// Package store is the Postgres layer: hand-written SQL over pgx.
package store

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB wraps a connection pool.
type DB struct {
	pool *pgxpool.Pool
	// enforceAccountAccess filters reads by account_access (§13.8). Off
	// until M3.5; only the tests turn it on (see readableAccount).
	enforceAccountAccess bool
	// progs caches compiled rules for ingest (rules.go).
	progs programCache
}

// New wraps pool. The caller keeps ownership of the pool.
func New(pool *pgxpool.Pool) *DB { return &DB{pool: pool} }

// Outcome says what an ingest commit did.
type Outcome int

const (
	// Delivered: new raw object and a new delivery.
	Delivered Outcome = iota + 1
	// Redelivery: new raw object, marked duplicate_of the raw of the
	// existing delivery with the same source key (§4.1); no new delivery.
	Redelivery
	// AlreadyIngested: this raw object (same id and bytes) was committed
	// before, e.g. by both the webhook and reconcile; nothing was written.
	AlreadyIngested
)

func (o Outcome) String() string {
	switch o {
	case Delivered:
		return "delivered"
	case Redelivery:
		return "redelivery"
	case AlreadyIngested:
		return "already_ingested"
	}
	return fmt.Sprintf("Outcome(%d)", int(o))
}

// Ingest is everything the ingest commit writes.
type Ingest struct {
	RawID       string
	R2Key       string
	LocalPath   string
	Size        int64
	SHA256      [32]byte
	ContentHash [32]byte
	ReceivedAt  time.Time

	DeliveryID string
	// AccountID is accounts.id of the source; EnsureCloudflareAccount
	// gives it for the configured Cloudflare account.
	AccountID    string
	Zone         string
	Prefix       string
	EnvelopeFrom string
	EnvelopeTo   string
}

// PushSourceKey is deliveries.source_key for push sources (§10.1): the
// lowercase hex content hash, "/", and envelope_to verbatim.
func PushSourceKey(contentHash [32]byte, envelopeTo string) string {
	return hex.EncodeToString(contentHash[:]) + "/" + envelopeTo
}

// CommitIngest is the Go-side commit point of §2.1: one transaction that
// records the raw object and either a delivery or the fact that it is a
// redelivery. Dedupe relies on unique constraints only, so concurrent
// ingests of two copies (webhook and reconcile) cannot both deliver: the
// second insert waits for the first transaction and then conflicts.
func (db *DB) CommitIngest(ctx context.Context, in Ingest) (Outcome, error) {
	var out Outcome
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		// No conflict target: a concurrent insert of the same object can
		// collide on id or r2_key first, and both mean the same.
		tag, err := tx.Exec(ctx, `
			INSERT INTO raw_objects (id, r2_key, local_path, size, sha256, received_at, completeness, content_hash)
			VALUES ($1, $2, $3, $4, $5, $6, 1, $7)
			ON CONFLICT DO NOTHING`,
			in.RawID, in.R2Key, in.LocalPath, in.Size, in.SHA256[:], in.ReceivedAt, in.ContentHash[:])
		if err != nil {
			return fmt.Errorf("insert raw_objects: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return alreadyIngested(ctx, tx, in, &out)
		}

		sourceKey := PushSourceKey(in.ContentHash, in.EnvelopeTo)
		tag, err = tx.Exec(ctx, `
			INSERT INTO deliveries (id, account_id, source_key, raw_id, zone, prefix, envelope_from, envelope_to, received_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (account_id, source_key) DO NOTHING`,
			in.DeliveryID, in.AccountID, sourceKey, in.RawID, in.Zone, in.Prefix, in.EnvelopeFrom, in.EnvelopeTo, in.ReceivedAt)
		if err != nil {
			return fmt.Errorf("insert deliveries: %w", err)
		}
		if tag.RowsAffected() == 1 {
			out = Delivered
			return nil
		}
		// Under READ COMMITTED this statement sees the delivery that the
		// conflicting (now committed) transaction wrote.
		tag, err = tx.Exec(ctx, `
			UPDATE raw_objects r SET duplicate_of = d.raw_id
			FROM deliveries d
			WHERE r.id = $1 AND d.account_id = $2 AND d.source_key = $3`,
			in.RawID, in.AccountID, sourceKey)
		if err != nil {
			return fmt.Errorf("mark duplicate: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("mark duplicate: delivery %q vanished", sourceKey)
		}
		out = Redelivery
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("commit ingest %s: %w", in.RawID, err)
	}
	return out, nil
}

// alreadyIngested confirms that a raw insert that did nothing collided
// with the same object, and not with different bytes under the same id or
// key, which would mean the immutable raw store was violated.
func alreadyIngested(ctx context.Context, tx pgx.Tx, in Ingest, out *Outcome) error {
	var id string
	err := tx.QueryRow(ctx, `
		SELECT id FROM raw_objects
		WHERE id = $1 AND r2_key = $2 AND sha256 = $3`,
		in.RawID, in.R2Key, in.SHA256[:]).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("raw %s or key %s already recorded with different content", in.RawID, in.R2Key)
	}
	if err != nil {
		return fmt.Errorf("look up raw %s: %w", in.RawID, err)
	}
	*out = AlreadyIngested
	return nil
}

// RawKeys returns the r2_key of every recorded raw object, for the full
// raw-vs-database scan.
func (db *DB) RawKeys(ctx context.Context) (map[string]struct{}, error) {
	rows, err := db.pool.Query(ctx, `SELECT r2_key FROM raw_objects`)
	if err != nil {
		return nil, fmt.Errorf("list raw keys: %w", err)
	}
	keys := map[string]struct{}{}
	var k string
	_, err = pgx.ForEachRow(rows, []any{&k}, func() error {
		keys[k] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list raw keys: %w", err)
	}
	return keys, nil
}
