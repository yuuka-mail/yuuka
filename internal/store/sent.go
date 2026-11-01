package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Our own outgoing mail is content like inbound mail (§4.6): the sent copy
// an outbox row points at (outbox.raw_id, stored under sent/) is parsed
// into messages, whatever the row's state, so it threads and is found by
// search. It has no delivery: no envelope, prefix or auth facts. Message
// and outbox are linked by the fold key only; the sent copies of message m
// are the raws some outbox row points at whose content_hash is m's. So
// everything here is derived from outbox.raw_id, which never changes, and
// from send facts, never from outbox.state.

// sentRawCols are the RawRef columns of a sent copy raw sr. A sent copy
// has no delivery, zone, prefix or account; ReceivedAt is raw_objects.received_at,
// the time the bytes were stored before the outbox row was created.
const sentRawCols = `'', sr.id, sr.r2_key, sr.local_path, sr.content_hash, '', '', sr.received_at, ''`

// isSentCopy is true for a raw sr that an outbox row points at. A raw that
// only a draft points at (an SMTP submission held before the outbox) is
// not one: it becomes a message once it is queued (§4.6).
const isSentCopy = `EXISTS (SELECT 1 FROM outbox o WHERE o.raw_id = sr.id)`

// SentCopy returns the raw reference of a sent copy. It fails when no
// outbox row points at the raw.
func (db *DB) SentCopy(ctx context.Context, rawID string) (RawRef, error) {
	ref, err := scanRawRef(db.pool.QueryRow(ctx, `
		SELECT `+sentRawCols+` FROM raw_objects sr WHERE sr.id = $1 AND `+isSentCopy, rawID))
	if errors.Is(err, pgx.ErrNoRows) {
		return RawRef{}, fmt.Errorf("sent copy %s: %w: no outbox row points at it", rawID, ErrNotFound)
	}
	if err != nil {
		return RawRef{}, fmt.Errorf("sent copy %s: %w", rawID, err)
	}
	return ref, nil
}

// UnindexedSent returns up to limit sent copies that have no message yet,
// oldest first: the indexing hook after enqueue failed, or the process
// stopped between the enqueue commit and the hook. A sent copy without a
// content hash is included so the backlog pass reports it; FoldSent
// refuses it.
func (db *DB) UnindexedSent(ctx context.Context, limit int) ([]RawRef, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT `+sentRawCols+`
		FROM raw_objects sr
		WHERE `+isSentCopy+`
			AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.content_hash = sr.content_hash)
		ORDER BY sr.received_at, sr.id
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("unindexed sent copies: %w", err)
	}
	refs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (RawRef, error) { return scanRawRef(r) })
	if err != nil {
		return nil, fmt.Errorf("unindexed sent copies: %w", err)
	}
	return refs, nil
}

// FoldSent runs the content step for a sent copy: fold it into the message
// with the same content_hash or create one and thread it, and log the
// changes. Unlike Fold there is no delivery to attach, no prefix to
// register and no auth facts: the bytes are our own. It is idempotent,
// and never notifies.
func (db *DB) FoldSent(ctx context.Context, ref RawRef, msg mimeparse.Message) (Folded, error) {
	var res Folded
	if ref.ContentHash == nil {
		// Without it the message could never be found from the outbox
		// row, nor the row from the message.
		return res, fmt.Errorf("fold sent copy %s: no content hash", ref.RawID)
	}
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, contentLock); err != nil {
			return err
		}
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT `+isSentCopy+` FROM raw_objects sr WHERE sr.id = $1`, ref.RawID).Scan(&ok); err != nil {
			return fmt.Errorf("load raw: %w", err)
		}
		if !ok {
			return fmt.Errorf("%w: no outbox row points at it", ErrNotFound)
		}
		w := writer{tx: tx}
		var id string
		err := tx.QueryRow(ctx, `SELECT id FROM messages WHERE content_hash = $1`, ref.ContentHash).Scan(&id)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("find fold target: %w", err)
		}
		if id == "" {
			id = ulid.New(ref.ReceivedAt)
			res.NewMessage = true
			if err := w.insertMessage(ctx, id, ref, msg); err != nil {
				return err
			}
		}
		// For an existing message this is what a second call, or an
		// inbound copy that arrived first, needs: the sent copy may now be
		// the earliest source.
		if err := w.updateMessage(ctx, id, msg); err != nil {
			return err
		}
		if err := w.refreshThreadOf(ctx, id); err != nil {
			return err
		}
		res.MessageID = id
		return w.flush(ctx)
	})
	if err != nil {
		return Folded{}, fmt.Errorf("fold sent copy %s: %w", ref.RawID, err)
	}
	return res, nil
}

// sentIDs returns the ids recipients may know the sent copies with
// content hash h by: each attempt's message_id and provider_id
// (§10.4.1). A platform that assigns Message-IDs gives every attempt a new
// one, and the provider id is only very likely the delivered Message-ID,
// so all of them count. They are send facts, so threading on them keeps
// reparse deterministic.
func (w *writer) sentIDs(ctx context.Context, h []byte) ([]string, error) {
	if h == nil {
		return nil, nil
	}
	rows, err := w.tx.Query(ctx, `
		SELECT DISTINCT x
		FROM raw_objects sr JOIN outbox o ON o.raw_id = sr.id
		JOIN send_attempts a ON a.outbox_id = o.id,
		unnest(ARRAY[a.message_id, a.provider_id]) x
		WHERE sr.content_hash = $1 AND x IS NOT NULL AND x <> ''
		ORDER BY x`, h)
	if err != nil {
		return nil, fmt.Errorf("sent ids: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("sent ids: %w", err)
	}
	return ids, nil
}

// SentRawByID returns the raw_objects row of sent copy id: an outbox
// row's, or a held SMTP submission's (drafts.raw_id).
func (db *DB) SentRawByID(ctx context.Context, id string) (SentRaw, error) {
	var r SentRaw
	var sum, ch []byte
	err := db.pool.QueryRow(ctx, `SELECT id, r2_key, size, sha256, content_hash, received_at FROM raw_objects WHERE id = $1`, id).
		Scan(&r.ID, &r.Key, &r.Size, &sum, &ch, &r.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, fmt.Errorf("raw %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return r, fmt.Errorf("raw %s: %w", id, err)
	}
	if len(sum) != len(r.SHA256) || len(ch) != len(r.ContentHash) {
		return r, fmt.Errorf("raw %s: not a sent copy (no sha256 or content hash)", id)
	}
	r.SHA256, r.ContentHash = [32]byte(sum), [32]byte(ch)
	return r, nil
}
