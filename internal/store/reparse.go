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

// ReparseTarget is one message to rebuild and every delivery folded into
// it, earliest first. Each delivery has its own raw and therefore its own
// auth facts; the content is the same in all of them.
type ReparseTarget struct {
	MessageID  string
	Deliveries []RawRef
}

// ReparseTargets pages through the messages received at or after since,
// in id order after the id after ("" to start).
func (db *DB) ReparseTargets(ctx context.Context, since time.Time, after string, limit int) ([]ReparseTarget, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT d.message_id, `+rawRefCols+`
		FROM (SELECT id FROM messages WHERE received_at >= $1 AND id > $2 ORDER BY id LIMIT $3) m
		JOIN deliveries d ON d.message_id = m.id
		JOIN raw_objects r ON r.id = d.raw_id
		ORDER BY m.id, d.received_at, d.id`, since, after, limit)
	if err != nil {
		return nil, fmt.Errorf("reparse targets: %w", err)
	}
	var out []ReparseTarget
	var mid string
	var ref RawRef
	_, err = pgx.ForEachRow(rows, []any{
		&mid, &ref.DeliveryID, &ref.RawID, &ref.R2Key, &ref.LocalPath,
		&ref.ContentHash, &ref.Zone, &ref.Prefix, &ref.ReceivedAt,
	}, func() error {
		if len(out) == 0 || out[len(out)-1].MessageID != mid {
			out = append(out, ReparseTarget{MessageID: mid})
		}
		t := &out[len(out)-1]
		t.Deliveries = append(t.Deliveries, ref)
		ref.ContentHash = nil // Scan reuses the slice otherwise
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reparse targets: %w", err)
	}
	return out, nil
}

// Rebuild rewrites the derived state of one message from a fresh parse of
// its content and the auth facts of each delivery (keyed by delivery id):
// the messages columns except id, and deliveries.message_id/auth only
// (§4.2). Decisions and delivery facts are not touched; prefixes only get
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
