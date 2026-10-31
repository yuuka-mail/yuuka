package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/classify"
)

// MessageView is what rendering one message needs (the agent view of
// §8.1): the message's derived facts and the viewer's deliveries of it.
type MessageView struct {
	// ID is the message id; an aliased id (§4.2) resolves to the
	// message it was merged into.
	ID         string
	ReceivedAt time.Time
	// Class is the effective class: a human override wins (§4.2).
	Class      classify.Class
	Downgraded bool
	Quarantine string // messages.parse_error; "" when parsed cleanly
	// Deliveries are the viewer's deliveries of the message, earliest
	// first. The first one's raw is the one to render.
	Deliveries []ViewDelivery
}

// ViewDelivery is one delivery of a MessageView.
type ViewDelivery struct {
	Raw        RawRef
	EnvelopeTo string
	// Auth is deliveries.auth; the zero value (untrusted) when the
	// delivery was never folded with authentication facts.
	Auth authres.Auth
}

// Message loads one message for v. It returns ErrNotFound when no such
// message exists or v may not see it (§13.8: a restricted viewer learns
// of existence elsewhere, never through a direct read).
func (db *DB) Message(ctx context.Context, v *auth.Viewer, id string) (MessageView, error) {
	if v == nil {
		return MessageView{}, errors.New("message: no viewer")
	}
	var mv MessageView
	err := pgx.BeginTxFunc(ctx, db.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		// M1 has no accounts yet, so every viewer sees every delivery;
		// the account_access filter lands here and in the deliveries
		// query below (M3.5).
		err := tx.QueryRow(ctx, `
			SELECT m.id, m.received_at, coalesce(s.class_override, m.class), g.downgraded, coalesce(m.parse_error, '')
			FROM messages m
			LEFT JOIN message_state s ON s.message_id = m.id
			CROSS JOIN LATERAL (SELECT NOT EXISTS (
				SELECT 1 FROM deliveries d JOIN prefixes p ON p.zone = d.zone AND p.prefix = d.prefix
				WHERE d.message_id = m.id AND `+notDowngraded+`) AS downgraded) g
			WHERE m.id = coalesce((SELECT new_id FROM message_aliases WHERE old_id = $1), $1)`, id).
			Scan(&mv.ID, &mv.ReceivedAt, &mv.Class, &mv.Downgraded, &mv.Quarantine)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT `+rawRefCols+`, d.envelope_to, d.auth
			FROM deliveries d JOIN raw_objects r ON r.id = d.raw_id
			WHERE d.message_id = $1
			ORDER BY d.received_at, d.id`, mv.ID)
		if err != nil {
			return err
		}
		mv.Deliveries, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (ViewDelivery, error) {
			var d ViewDelivery
			var authJSON []byte
			r := &d.Raw
			err := row.Scan(&r.DeliveryID, &r.RawID, &r.R2Key, &r.LocalPath, &r.ContentHash, &r.Zone, &r.Prefix,
				&r.ReceivedAt, &d.EnvelopeTo, &authJSON)
			if err == nil && authJSON != nil {
				err = json.Unmarshal(authJSON, &d.Auth)
			}
			return d, err
		})
		if err != nil {
			return err
		}
		if len(mv.Deliveries) == 0 {
			// Not visible through any delivery: the same answer as a
			// message that does not exist.
			return ErrNotFound
		}
		return nil
	})
	if errors.Is(err, ErrNotFound) {
		return MessageView{}, ErrNotFound
	}
	if err != nil {
		return MessageView{}, fmt.Errorf("message %s: %w", id, err)
	}
	return mv, nil
}

// PrefixQuery filters Prefixes.
type PrefixQuery struct {
	// LeakSuspect, when set, keeps only prefixes with that flag.
	LeakSuspect *bool
	Limit       int // default 200, at most 1000
}

// PrefixInfo is one row of the prefix registry (§3.3) with the number of
// messages the viewer can see at it.
type PrefixInfo struct {
	Zone            string
	Prefix          string
	Claimed         bool
	Label           string
	Note            string
	ExpectedDomains []string
	FirstSeen       time.Time
	LastSeen        time.Time
	LeakSuspect     bool
	Active          bool
	Messages        int
}

// PrefixList is a page of prefixes, most recently seen first, and the
// total number that matched.
type PrefixList struct {
	Total    int
	Prefixes []PrefixInfo
}

// Prefixes lists the prefix registry for v.
func (db *DB) Prefixes(ctx context.Context, v *auth.Viewer, q PrefixQuery) (PrefixList, error) {
	if v == nil {
		return PrefixList{}, errors.New("prefixes: no viewer")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}
	limit = min(limit, 1000)
	var res PrefixList
	err := pgx.BeginTxFunc(ctx, db.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		// The leak filter is a nullable parameter so both statements
		// share one WHERE. Counts aggregate the viewer's deliveries only
		// (§13.8); in M1 that is all of them.
		const where = `WHERE ($1::boolean IS NULL OR p.leak_suspect = $1)`
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM prefixes p `+where, q.LeakSuspect).Scan(&res.Total); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT p.zone, p.prefix, p.state = $3, coalesce(p.label, ''), coalesce(p.note, ''), p.expected_domains,
				p.first_seen, p.last_seen, p.leak_suspect, p.active,
				(SELECT count(DISTINCT d.message_id) FROM deliveries d
				 WHERE d.zone = p.zone AND d.prefix = p.prefix AND d.message_id IS NOT NULL)
			FROM prefixes p `+where+`
			ORDER BY p.last_seen DESC, p.zone, p.prefix
			LIMIT $2`, q.LeakSuspect, limit, PrefixClaimed)
		if err != nil {
			return err
		}
		res.Prefixes, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (PrefixInfo, error) {
			var p PrefixInfo
			err := r.Scan(&p.Zone, &p.Prefix, &p.Claimed, &p.Label, &p.Note, &p.ExpectedDomains,
				&p.FirstSeen, &p.LastSeen, &p.LeakSuspect, &p.Active, &p.Messages)
			return p, err
		})
		return err
	})
	if err != nil {
		return PrefixList{}, fmt.Errorf("prefixes: %w", err)
	}
	return res, nil
}
