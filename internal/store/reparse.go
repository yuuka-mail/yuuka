package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
)

// ReparseTarget is one message to rebuild and its sources, each list
// earliest first: every delivery folded into it, each with its own raw
// and therefore its own auth facts, and its sent copies (§4.6), found
// through outbox.raw_id. The content is the same in all of them.
type ReparseTarget struct {
	MessageID  string
	Deliveries []RawRef
	Sent       []RawRef
}

// Canonical is the source whose raw the message is parsed from: the
// earliest, a delivery on a tie, as updateMessage picks canonical_raw_id.
// A target always has at least one source.
func (t ReparseTarget) Canonical() RawRef {
	if len(t.Deliveries) == 0 || (len(t.Sent) > 0 && t.Sent[0].ReceivedAt.Before(t.Deliveries[0].ReceivedAt)) {
		return t.Sent[0]
	}
	return t.Deliveries[0]
}

// ReparseTargets pages through the messages received at or after since,
// in id order after the id after ("" to start). A message with neither a
// delivery nor a sent copy has nothing to be rebuilt from and is skipped.
func (db *DB) ReparseTargets(ctx context.Context, since time.Time, after string, limit int) ([]ReparseTarget, error) {
	rows, err := db.pool.Query(ctx, `
		WITH m AS (SELECT id, content_hash FROM messages WHERE received_at >= $1 AND id > $2 ORDER BY id LIMIT $3)
		SELECT m.id, `+rawRefCols+`, d.id
		FROM m JOIN deliveries d ON d.message_id = m.id
		JOIN raw_objects r ON r.id = d.raw_id
		UNION ALL
		SELECT m.id, `+sentRawCols+`, sr.id
		FROM m JOIN raw_objects sr ON sr.content_hash = m.content_hash
		WHERE `+isSentCopy+`
		ORDER BY 1, 9, 10`, since, after, limit)
	if err != nil {
		return nil, fmt.Errorf("reparse targets: %w", err)
	}
	var out []ReparseTarget
	var mid, key string
	var ref RawRef
	_, err = pgx.ForEachRow(rows, []any{
		&mid, &ref.DeliveryID, &ref.RawID, &ref.R2Key, &ref.LocalPath,
		&ref.ContentHash, &ref.Zone, &ref.Prefix, &ref.ReceivedAt, &key,
	}, func() error {
		if len(out) == 0 || out[len(out)-1].MessageID != mid {
			out = append(out, ReparseTarget{MessageID: mid})
		}
		t := &out[len(out)-1]
		if ref.DeliveryID == "" {
			t.Sent = append(t.Sent, ref)
		} else {
			t.Deliveries = append(t.Deliveries, ref)
		}
		ref.ContentHash = nil // Scan reuses the slice otherwise
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reparse targets: %w", err)
	}
	return out, nil
}

// Rebuild rewrites the derived state of one message from a fresh parse of
// its content (t.Canonical) and the auth facts of each delivery (keyed by
// delivery id; sent copies have none): the messages columns except id,
// and deliveries.message_id/auth only (§4.2). Decisions and delivery facts are not touched; prefixes only get
// first_seen/last_seen widened. Threads are left to Rethread, which
// reparse runs once at the end. It reports whether anything changed.
func (db *DB) Rebuild(ctx context.Context, t ReparseTarget, msg mimeparse.Message, auth map[string]authres.Auth) (bool, error) {
	changed := false
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, contentLock); err != nil {
			return err
		}
		w := writer{tx: tx}
		for _, d := range t.Deliveries {
			a, ok := auth[d.DeliveryID]
			if !ok {
				return fmt.Errorf("no auth for delivery %s", d.DeliveryID)
			}
			js, err := json.Marshal(a)
			if err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `
				UPDATE deliveries SET auth = $2 WHERE id = $1 AND message_id = $3 AND auth IS DISTINCT FROM $2::jsonb`,
				d.DeliveryID, js, t.MessageID)
			if err != nil {
				return fmt.Errorf("update auth: %w", err)
			}
			if tag.RowsAffected() > 0 {
				w.change("delivery", d.DeliveryID)
			}
			if err := w.registerPrefix(ctx, d.Zone, d.Prefix, d.ReceivedAt); err != nil {
				return err
			}
		}
		if err := w.updateMessage(ctx, t.MessageID, msg); err != nil {
			return err
		}
		changed = len(w.changes) > 0
		return w.flush(ctx)
	})
	if err != nil {
		return false, fmt.Errorf("rebuild message %s: %w", t.MessageID, err)
	}
	return changed, nil
}
