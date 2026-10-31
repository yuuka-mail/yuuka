package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/classify"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// contentLock serializes every writer of the content layer (fold, reparse,
// rethread). Threading reads other messages to decide where a new one
// goes; two concurrent folds of related messages would otherwise each
// start a thread. Personal mail volume makes one lock affordable, and it
// also makes changes.seq follow commit order.
const contentLock = 0x7975756b61_02

// Prefix states (prefixes.state).
const (
	PrefixClaimed    = 1
	PrefixDiscovered = 2
)

// notDowngraded is true for a delivery d (joined with its prefix row p)
// that escapes the M1 minimal spam downgrade of §4.3: the prefix is
// claimed and the trusted receiver did not report dmarc=fail. A message is
// downgraded when none of its deliveries escapes. It is evaluated live, so
// claiming a prefix takes effect without a reparse.
const notDowngraded = `(p.state = 1 AND coalesce(d.auth->>'dmarc', '') <> 'fail')`

// RawRef locates one delivery and its raw, or a sent copy (§4.6), which
// has no delivery: then DeliveryID, Zone and Prefix are empty.
type RawRef struct {
	DeliveryID  string
	RawID       string
	R2Key       string
	LocalPath   string
	ContentHash []byte // nil for header_only raw
	Zone        string
	Prefix      string
	ReceivedAt  time.Time
}

const rawRefCols = `d.id, d.raw_id, r.r2_key, r.local_path, r.content_hash, d.zone, d.prefix, d.received_at`

func scanRawRef(row pgx.Row) (RawRef, error) {
	var r RawRef
	err := row.Scan(&r.DeliveryID, &r.RawID, &r.R2Key, &r.LocalPath, &r.ContentHash, &r.Zone, &r.Prefix, &r.ReceivedAt)
	return r, err
}

// Unprocessed returns up to limit deliveries without a message, oldest
// first: committed by M0 before parsing existed, or left behind when the
// content step failed after the ingest commit point.
func (db *DB) Unprocessed(ctx context.Context, limit int) ([]RawRef, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT `+rawRefCols+`
		FROM deliveries d JOIN raw_objects r ON r.id = d.raw_id
		WHERE d.message_id IS NULL
		ORDER BY d.received_at, d.id
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("unprocessed deliveries: %w", err)
	}
	refs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (RawRef, error) { return scanRawRef(r) })
	if err != nil {
		return nil, fmt.Errorf("unprocessed deliveries: %w", err)
	}
	return refs, nil
}

// Delivery returns the raw reference of one delivery.
func (db *DB) Delivery(ctx context.Context, id string) (RawRef, error) {
	ref, err := scanRawRef(db.pool.QueryRow(ctx, `
		SELECT `+rawRefCols+`
		FROM deliveries d JOIN raw_objects r ON r.id = d.raw_id
		WHERE d.id = $1`, id))
	if err != nil {
		return RawRef{}, fmt.Errorf("delivery %s: %w", id, err)
	}
	return ref, nil
}

// Folded says what Fold did.
type Folded struct {
	MessageID  string
	NewMessage bool
	// Notify is true when this delivery escapes the spam downgrade and no
	// earlier delivery of the same message did: the message is new to the
	// operator's attention. Callers that replay old mail ignore it.
	Notify bool
}

// Fold runs the content step for one committed delivery (§4.1): register
// its prefix, fold it into the message with the same content_hash or
// create one and thread it, record its auth facts, and log the changes.
// It is idempotent: a delivery that already has a message is left alone.
// The caller decides whether to act on Folded.Notify.
func (db *DB) Fold(ctx context.Context, ref RawRef, msg mimeparse.Message, auth authres.Auth) (Folded, error) {
	var res Folded
	authJSON, err := json.Marshal(auth)
	if err != nil {
		return res, err
	}
	err = pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, contentLock); err != nil {
			return err
		}
		var existing *string
		if err := tx.QueryRow(ctx, `SELECT message_id FROM deliveries WHERE id = $1`, ref.DeliveryID).Scan(&existing); err != nil {
			return fmt.Errorf("load delivery: %w", err)
		}
		if existing != nil {
			// Already folded, e.g. by the backlog pass racing the live
			// path. The notification decision does not depend on who
			// folded, so the live caller still gets it.
			res.MessageID = *existing
			return notifyFor(ctx, tx, *existing, ref.DeliveryID, &res.Notify)
		}
		w := writer{tx: tx}
		if err := w.registerPrefix(ctx, ref.Zone, ref.Prefix, ref.ReceivedAt); err != nil {
			return err
		}

		var id string
		if ref.ContentHash != nil {
			err := tx.QueryRow(ctx, `SELECT id FROM messages WHERE content_hash = $1`, ref.ContentHash).Scan(&id)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("find fold target: %w", err)
			}
		}
		if id == "" {
			id = ulid.New(ref.ReceivedAt)
			res.NewMessage = true
			if err := w.insertMessage(ctx, id, ref, msg); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET message_id = $2, auth = $3 WHERE id = $1`,
			ref.DeliveryID, id, authJSON); err != nil {
			return fmt.Errorf("attach delivery: %w", err)
		}
		w.change("delivery", ref.DeliveryID)
		// Same for a new and a folded message: canonical raw, received_at
		// and class follow from all deliveries, as on reparse.
		if err := w.updateMessage(ctx, id, msg); err != nil {
			return err
		}
		if err := w.refreshThreadOf(ctx, id); err != nil {
			return err
		}
		res.MessageID = id
		if err := notifyFor(ctx, tx, id, ref.DeliveryID, &res.Notify); err != nil {
			return err
		}
		return w.flush(ctx)
	})
	if err != nil {
		return Folded{}, fmt.Errorf("fold delivery %s: %w", ref.DeliveryID, err)
	}
	return res, nil
}

// notifyFor decides Folded.Notify: delivery escapes the downgrade and no
// earlier delivery of message did.
func notifyFor(ctx context.Context, tx pgx.Tx, message, delivery string, out *bool) error {
	err := tx.QueryRow(ctx, `
		WITH this AS (SELECT received_at, id FROM deliveries WHERE id = $2)
		SELECT
			EXISTS (SELECT 1 FROM deliveries d JOIN prefixes p ON p.zone = d.zone AND p.prefix = d.prefix
			        WHERE d.id = $2 AND `+notDowngraded+`)
			AND NOT EXISTS (SELECT 1 FROM deliveries d JOIN prefixes p ON p.zone = d.zone AND p.prefix = d.prefix, this
			        WHERE d.message_id = $1 AND (d.received_at, d.id) < (this.received_at, this.id) AND `+notDowngraded+`)`,
		message, delivery).Scan(out)
	if err != nil {
		return fmt.Errorf("downgrade: %w", err)
	}
	return nil
}

// writer bundles the statements shared by fold and reparse and collects
// the change log entries of one transaction.
type writer struct {
	tx      pgx.Tx
	changes [][2]string
}

// change records that kind/id changed; once per transaction is enough.
func (w *writer) change(kind, id string) {
	c := [2]string{kind, id}
	if !slices.Contains(w.changes, c) {
		w.changes = append(w.changes, c)
	}
}

func (w *writer) flush(ctx context.Context) error {
	if len(w.changes) == 0 {
		return nil
	}
	kinds := make([]string, len(w.changes))
	ids := make([]string, len(w.changes))
	for i, c := range w.changes {
		kinds[i], ids[i] = c[0], c[1]
	}
	_, err := w.tx.Exec(ctx, `
		INSERT INTO changes (kind, target_id)
		SELECT k, t FROM unnest($1::text[], $2::text[]) WITH ORDINALITY AS c(k, t, n) ORDER BY n`, kinds, ids)
	if err != nil {
		return fmt.Errorf("log changes: %w", err)
	}
	w.changes = nil
	return nil
}

// registerPrefix records (zone, prefix) as discovered on first sight
// (§3.3) and widens first_seen/last_seen otherwise. It never touches the
// decision columns.
func (w *writer) registerPrefix(ctx context.Context, zone, prefix string, at time.Time) error {
	var inserted bool
	err := w.tx.QueryRow(ctx, `
		INSERT INTO prefixes (zone, prefix, first_seen, last_seen) VALUES ($1, $2, $3, $3)
		ON CONFLICT (zone, prefix) DO UPDATE SET
			first_seen = least(prefixes.first_seen, excluded.first_seen),
			last_seen = greatest(prefixes.last_seen, excluded.last_seen)
		RETURNING xmax = 0`, zone, prefix, at).Scan(&inserted)
	if err != nil {
		return fmt.Errorf("register prefix: %w", err)
	}
	if inserted {
		w.change("prefix", prefix+"@"+zone)
	}
	return nil
}

// insertMessage creates a message for ref (a delivery or a sent copy) and
// puts it in a thread. The columns that depend on all its sources are
// filled by updateMessage.
func (w *writer) insertMessage(ctx context.Context, id string, ref RawRef, msg mimeparse.Message) error {
	var prefixes []string
	if ref.Prefix != "" {
		prefixes = []string{ref.Prefix}
	}
	threadID, err := w.placeThread(ctx, id, ref.ReceivedAt, ref.ContentHash, msg, classify.Of(msg, prefixes))
	if err != nil {
		return err
	}
	_, err = w.tx.Exec(ctx, `
		INSERT INTO messages (id, canonical_raw_id, content_hash, thread_id, received_at,
			subject, snippet, from_addr, from_name, class, has_attach, body_text, parser_version)
		VALUES ($1, $2, $3, $4, $5, '', '', '', '', 0, false, '', 0)`,
		id, ref.RawID, ref.ContentHash, threadID, ref.ReceivedAt)
	if err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	return nil
}

// updateMessage writes the derived columns of message id from msg and
// its sources, logging a change only if something differs. It returns
// nothing about threads; callers re-thread as needed.
//
// The canonical raw and received_at come from the earliest source: its
// deliveries and its sent copies (§4.6), whose time is when the bytes
// were stored, i.e. submission. On a tie a delivery wins, then the lower
// id; ReparseTarget.Canonical picks the same one.
func (w *writer) updateMessage(ctx context.Context, id string, msg mimeparse.Message) error {
	var prefixes []string
	rows, err := w.tx.Query(ctx, `SELECT DISTINCT prefix FROM deliveries WHERE message_id = $1 ORDER BY prefix`, id)
	if err != nil {
		return fmt.Errorf("message prefixes: %w", err)
	}
	if prefixes, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return fmt.Errorf("message prefixes: %w", err)
	}
	var date *time.Time
	if !msg.Date.IsZero() {
		date = &msg.Date
	}
	var problem *string
	if msg.Problem != "" {
		problem = &msg.Problem
	}
	tag, err := w.tx.Exec(ctx, `
		UPDATE messages m SET
			canonical_raw_id = first.raw_id, received_at = first.received_at,
			message_id = $2, in_reply_to = $3, refs = $4, date_header = $5,
			subject = $6, snippet = $7, from_addr = $8, from_name = $9, class = $10,
			has_attach = $11, body_text = $12, parser_version = $13, parse_error = $14
		FROM (SELECT raw_id, received_at FROM (
		          SELECT raw_id, received_at, 0 AS src, id FROM deliveries WHERE message_id = $1
		          UNION ALL
		          SELECT sr.id, sr.received_at, 1, sr.id
		          FROM messages mm JOIN raw_objects sr ON sr.content_hash = mm.content_hash
		          WHERE mm.id = $1 AND `+isSentCopy+`
		      ) src ORDER BY received_at, src, id LIMIT 1) first
		WHERE m.id = $1 AND (m.canonical_raw_id, m.received_at, m.message_id, m.in_reply_to, m.refs,
			m.date_header, m.subject, m.snippet, m.from_addr, m.from_name, m.class, m.has_attach,
			m.body_text, m.parser_version, m.parse_error)
		IS DISTINCT FROM (first.raw_id, first.received_at, $2, $3, $4, $5, $6, $7, $8, $9, $10,
			$11, $12, $13, $14)`,
		id, nullIfEmpty(msg.MessageID), nullIfEmpty(msg.InReplyTo), msg.References, date,
		msg.Subject, msg.Snippet, msg.FromAddr, msg.FromName, int16(classify.Of(msg, prefixes)),
		msg.HasAttach, msg.BodyText, mimeparse.Version, problem)
	if err != nil {
		return fmt.Errorf("update message: %w", err)
	}
	if tag.RowsAffected() > 0 {
		w.change("message", id)
	}
	return nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
