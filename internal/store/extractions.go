package store

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/extract"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
)

// The extractions of a message (migration 0008) are derived like its
// other content columns: the writer rebuilds them whenever it rebuilds
// the message, from the same parse, and a backlog pass rebuilds those an
// older extractor wrote (UnextractedMessages, Reextract). Readers get
// them through the message they belong to, so whoever may read the
// message may read them and nobody else (§13.8): they are body data.

// Extraction is one row of extractions: a value the extractor found in
// a message. Kind is otp, link, expiry or amount; Source subject, body
// or html; ExpiresAt is set for expiry only. The JSON names are the
// columns', which the summary query aggregates with to_jsonb.
type Extraction struct {
	Kind       string     `json:"kind"`
	Value      string     `json:"value"`
	Confidence int        `json:"confidence"`
	Source     string     `json:"source"`
	ExpiresAt  *time.Time `json:"expires_at"`
}

const extractionCols = `kind, value, confidence, source, expires_at`

func extractionsOf(ctx context.Context, q pgx.Tx, id string) ([]Extraction, error) {
	rows, err := q.Query(ctx, `SELECT `+extractionCols+` FROM extractions WHERE message_id = $1 ORDER BY ord`, id)
	if err != nil {
		return nil, fmt.Errorf("extractions: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Extraction])
	if err != nil {
		return nil, fmt.Errorf("extractions: %w", err)
	}
	return out, nil
}

// extract rewrites the extractions of message id from msg, its current
// parse, and records the extractor version. It runs after the message's
// received_at is final in this transaction, since expiry counts from it.
// A change is logged only when the rows differ: a version bump that
// finds the same values is no news to anyone.
func (w *writer) extract(ctx context.Context, id string, msg mimeparse.Message) error {
	var receivedAt time.Time
	var version int16
	if err := w.tx.QueryRow(ctx, `SELECT received_at, extract_version FROM messages WHERE id = $1`, id).
		Scan(&receivedAt, &version); err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	var want []Extraction
	for _, x := range extract.Message(msg, receivedAt) {
		e := Extraction{Kind: string(x.Kind), Value: x.Value, Confidence: x.Confidence, Source: string(x.Source)}
		if !x.Expires.IsZero() {
			// The column keeps microseconds; compare what it will hold.
			t := x.Expires.Truncate(time.Microsecond).UTC()
			e.ExpiresAt = &t
		}
		want = append(want, e)
	}
	have, err := extractionsOf(ctx, w.tx, id)
	if err != nil {
		return err
	}
	if !slices.EqualFunc(have, want, sameExtraction) {
		if _, err := w.tx.Exec(ctx, `DELETE FROM extractions WHERE message_id = $1`, id); err != nil {
			return fmt.Errorf("extract: %w", err)
		}
		if len(want) > 0 {
			kinds, values, sources := make([]string, len(want)), make([]string, len(want)), make([]string, len(want))
			confidence := make([]int16, len(want))
			expires := make([]*time.Time, len(want))
			for i, e := range want {
				kinds[i], values[i], sources[i] = e.Kind, e.Value, e.Source
				confidence[i], expires[i] = int16(e.Confidence), e.ExpiresAt
			}
			if _, err := w.tx.Exec(ctx, `
				INSERT INTO extractions (message_id, ord, kind, value, confidence, source, expires_at)
				SELECT $1, n - 1, k, v, c, s, x
				FROM unnest($2::text[], $3::text[], $4::smallint[], $5::text[], $6::timestamptz[])
					WITH ORDINALITY AS e(k, v, c, s, x, n)`,
				id, kinds, values, confidence, sources, expires); err != nil {
				return fmt.Errorf("extract: %w", err)
			}
		}
		w.change("message", id)
	}
	if version != extract.Version {
		if _, err := w.tx.Exec(ctx, `UPDATE messages SET extract_version = $2 WHERE id = $1`, id, extract.Version); err != nil {
			return fmt.Errorf("extract: %w", err)
		}
	}
	return nil
}

func sameExtraction(a, b Extraction) bool {
	if (a.ExpiresAt == nil) != (b.ExpiresAt == nil) || a.ExpiresAt != nil && !a.ExpiresAt.Equal(*b.ExpiresAt) {
		return false
	}
	return a.Kind == b.Kind && a.Value == b.Value && a.Confidence == b.Confidence && a.Source == b.Source
}

// UnextractedMessages returns up to limit messages, in id order, whose
// extractions an older extractor wrote, or none did: messages stored
// before migration 0008, and all of them after extract.Version moves.
// The backlog pass re-extracts them (Reextract), so neither an upgrade
// nor a better extractor needs a reparse. Messages with nothing to
// rebuild from are left out, as by UnreadRecipients.
func (db *DB) UnextractedMessages(ctx context.Context, limit int) ([]ReparseTarget, error) {
	return db.reparseTargets(ctx, `SELECT mm.id, mm.content_hash FROM messages mm
		WHERE mm.extract_version < $2
		  AND (EXISTS (SELECT 1 FROM deliveries d WHERE d.message_id = mm.id)
		    OR EXISTS (SELECT 1 FROM raw_objects sr WHERE sr.content_hash = mm.content_hash AND `+isSentCopy+`))
		ORDER BY mm.id LIMIT $1`, limit, extract.Version)
}

// Reextract rewrites the extractions of message id from msg, a fresh
// parse of its canonical raw, and nothing else: the other content
// columns stay as the last fold or reparse left them. It reports whether
// the extractions changed.
func (db *DB) Reextract(ctx context.Context, id string, msg mimeparse.Message) (bool, error) {
	changed := false
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, contentLock); err != nil {
			return err
		}
		w := writer{tx: tx}
		if err := w.extract(ctx, id, msg); err != nil {
			return err
		}
		changed = len(w.changes) > 0
		return w.flush(ctx)
	})
	if err != nil {
		return false, fmt.Errorf("re-extract message %s: %w", id, err)
	}
	return changed, nil
}
