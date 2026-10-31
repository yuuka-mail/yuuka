package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/classify"
)

// The read paths of the WebUI and REST API: message list, message,
// thread and the prefix aggregation. Like Search, each takes the viewer it
// runs for and refuses a nil one (§13.8). M1 has no accounts, so every
// viewer sees everything; when account_access lands, its filter goes into
// visibleMessage and visibleDelivery below and nowhere else.

// errNoViewer is returned by every read called without a viewer.
var errNoViewer = errors.New("no viewer")

func needViewer(v *auth.Viewer, op string) error {
	if v == nil {
		return fmt.Errorf("%s: %w", op, errNoViewer)
	}
	return nil
}

// visibleMessage and visibleDelivery are the one place where the viewer
// narrows what a query sees. M1: always true.
func visibleMessage(*auth.Viewer) string  { return "true" }
func visibleDelivery(*auth.Viewer) string { return "true" }

// Downgrade selects messages by the §4.3 minimal downgrade.
type Downgrade int

const (
	// HideDowngraded is the default of aggregate views.
	HideDowngraded Downgrade = iota
	// IncludeDowngraded returns both.
	IncludeDowngraded
	// OnlyDowngraded returns only downgraded mail, the "spam" view.
	OnlyDowngraded
)

// MessageFilter narrows the message list.
type MessageFilter struct {
	// Zone and Prefix select mail delivered to prefix@zone; set both or
	// neither.
	Zone, Prefix string
	// Class selects by effective class (a human override wins); nil means
	// any.
	Class      *classify.Class
	Downgraded Downgrade
}

// MessageCursor is a position in the list order (received_at, id), both
// descending.
type MessageCursor struct {
	ReceivedAt time.Time
	ID         string
}

// MessageQuery is one page of the message list.
type MessageQuery struct {
	MessageFilter
	// Before returns messages strictly after this position; nil starts at
	// the newest. Keyset paging keeps pages stable while mail arrives.
	Before *MessageCursor
	Limit  int // default 100, at most 500
}

// MessageSummary is a message as lists show it.
type MessageSummary struct {
	ID          string
	ThreadID    string
	ReceivedAt  time.Time
	Subject     string
	FromAddr    string
	FromName    string
	Snippet     string
	Class       classify.Class // effective
	Downgraded  bool
	Quarantined bool
	HasAttach   bool
	// Recipients are the prefix@zone of the visible deliveries, sorted.
	Recipients []string
}

// summaryCols and summaryFrom are shared by the list and the thread; the
// lateral subquery computes downgrade and recipients over the visible
// deliveries only.
const summaryCols = `m.id, m.thread_id, m.received_at, m.subject, m.from_addr, m.from_name, m.snippet,
	coalesce(s.class_override, m.class), g.downgraded, m.parse_error IS NOT NULL, m.has_attach, g.recipients`

func summaryFrom(v *auth.Viewer) string {
	return `
		FROM messages m
		LEFT JOIN message_state s ON s.message_id = m.id
		CROSS JOIN LATERAL (
			SELECT NOT coalesce(bool_or(` + notDowngraded + `), false) AS downgraded,
				coalesce(array_agg(DISTINCT d.prefix || '@' || d.zone ORDER BY d.prefix || '@' || d.zone), '{}') AS recipients
			FROM deliveries d JOIN prefixes p ON p.zone = d.zone AND p.prefix = d.prefix
			WHERE d.message_id = m.id AND ` + visibleDelivery(v) + `) g
		WHERE ` + visibleMessage(v)
}

func scanSummary(r pgx.CollectableRow) (MessageSummary, error) {
	var m MessageSummary
	err := r.Scan(&m.ID, &m.ThreadID, &m.ReceivedAt, &m.Subject, &m.FromAddr, &m.FromName, &m.Snippet,
		&m.Class, &m.Downgraded, &m.Quarantined, &m.HasAttach, &m.Recipients)
	return m, err
}

// ListMessages returns one page of messages for v, newest first.
func (db *DB) ListMessages(ctx context.Context, v *auth.Viewer, q MessageQuery) ([]MessageSummary, error) {
	if err := needViewer(v, "list messages"); err != nil {
		return nil, err
	}
	if (q.Zone == "") != (q.Prefix == "") {
		return nil, errors.New("list messages: set zone and prefix together")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 500)

	var where []string
	var args []any
	arg := func(a any) string {
		args = append(args, a)
		return fmt.Sprintf("$%d", len(args))
	}
	if q.Zone != "" {
		where = append(where, `EXISTS (SELECT 1 FROM deliveries d WHERE d.message_id = m.id AND d.zone = `+
			arg(q.Zone)+` AND d.prefix = `+arg(q.Prefix)+` AND `+visibleDelivery(v)+`)`)
	}
	if q.Class != nil {
		where = append(where, `coalesce(s.class_override, m.class) = `+arg(int16(*q.Class)))
	}
	switch q.Downgraded {
	case HideDowngraded:
		where = append(where, `NOT g.downgraded`)
	case OnlyDowngraded:
		where = append(where, `g.downgraded`)
	}
	if q.Before != nil {
		where = append(where, `(m.received_at, m.id) < (`+arg(q.Before.ReceivedAt)+`, `+arg(q.Before.ID)+`)`)
	}
	sql := `SELECT ` + summaryCols + summaryFrom(v)
	for _, w := range where {
		sql += "\n\t\tAND " + w
	}
	sql += "\n\t\tORDER BY m.received_at DESC, m.id DESC LIMIT " + arg(limit)
	rows, err := db.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanSummary)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	return out, nil
}

// DeliveryFact is one visible delivery of a message.
type DeliveryFact struct {
	ID           string
	Zone         string
	Prefix       string
	PrefixState  int // PrefixClaimed or PrefixDiscovered
	EnvelopeFrom string
	EnvelopeTo   string
	ReceivedAt   time.Time
	// Auth is nil when it was never extracted (M0 deliveries).
	Auth *authres.Auth
}

// MessageDetail is one message with its parsed fields and deliveries.
type MessageDetail struct {
	MessageSummary
	MessageID  string // Message-ID header without brackets; "" if none
	InReplyTo  string
	References []string
	Date       *time.Time // Date header; nil if missing or unparseable
	BodyText   string
	ParseError string // why it is quarantined; "" if parsed cleanly
	Deliveries []DeliveryFact
}

// MessageByID returns message id as v sees it. An id merged away by a
// content-hash change resolves to its survivor (§4.2), whose id the
// result carries. ErrNotFound if it does not exist or is not visible.
func (db *DB) MessageByID(ctx context.Context, v *auth.Viewer, id string) (MessageDetail, error) {
	if err := needViewer(v, "message"); err != nil {
		return MessageDetail{}, err
	}
	var d MessageDetail
	err := pgx.BeginTxFunc(ctx, db.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+summaryCols+`,
				coalesce(m.message_id, ''), coalesce(m.in_reply_to, ''), m.refs, m.date_header, m.body_text, coalesce(m.parse_error, '')
			`+summaryFrom(v)+`
			AND m.id = coalesce((SELECT new_id FROM message_aliases WHERE old_id = $1), $1)`, id)
		if err != nil {
			return err
		}
		d, err = pgx.CollectExactlyOneRow(rows, func(r pgx.CollectableRow) (MessageDetail, error) {
			var d MessageDetail
			m := &d.MessageSummary
			err := r.Scan(&m.ID, &m.ThreadID, &m.ReceivedAt, &m.Subject, &m.FromAddr, &m.FromName, &m.Snippet,
				&m.Class, &m.Downgraded, &m.Quarantined, &m.HasAttach, &m.Recipients,
				&d.MessageID, &d.InReplyTo, &d.References, &d.Date, &d.BodyText, &d.ParseError)
			return d, err
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `
			SELECT d.id, d.zone, d.prefix, p.state, d.envelope_from, d.envelope_to, d.received_at, d.auth
			FROM deliveries d JOIN prefixes p ON p.zone = d.zone AND p.prefix = d.prefix
			WHERE d.message_id = $1 AND `+visibleDelivery(v)+`
			ORDER BY d.received_at, d.id`, d.ID)
		if err != nil {
			return err
		}
		d.Deliveries, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (DeliveryFact, error) {
			var f DeliveryFact
			err := r.Scan(&f.ID, &f.Zone, &f.Prefix, &f.PrefixState, &f.EnvelopeFrom, &f.EnvelopeTo, &f.ReceivedAt, &f.Auth)
			return f, err
		})
		return err
	})
	if err != nil {
		return MessageDetail{}, fmt.Errorf("message %s: %w", id, err)
	}
	return d, nil
}

// ThreadView is a thread as v sees it. The aggregates are computed from
// the visible messages, never read from the threads table, whose stored
// values only serve unrestricted views (§13.8).
type ThreadView struct {
	ID           string
	Subject      string // of the earliest visible message
	Participants []string
	FirstAt      time.Time
	LastAt       time.Time
	Messages     []MessageSummary // oldest first
}

// ThreadByID returns thread id. ErrNotFound if it has no visible message.
func (db *DB) ThreadByID(ctx context.Context, v *auth.Viewer, id string) (ThreadView, error) {
	if err := needViewer(v, "thread"); err != nil {
		return ThreadView{}, err
	}
	rows, err := db.pool.Query(ctx, `SELECT `+summaryCols+summaryFrom(v)+`
		AND m.thread_id = $1
		ORDER BY m.received_at, m.id`, id)
	if err != nil {
		return ThreadView{}, fmt.Errorf("thread %s: %w", id, err)
	}
	ms, err := pgx.CollectRows(rows, scanSummary)
	if err != nil {
		return ThreadView{}, fmt.Errorf("thread %s: %w", id, err)
	}
	if len(ms) == 0 {
		return ThreadView{}, fmt.Errorf("thread %s: %w", id, ErrNotFound)
	}
	t := ThreadView{
		ID: id, Subject: ms[0].Subject, Participants: []string{},
		FirstAt: ms[0].ReceivedAt, LastAt: ms[len(ms)-1].ReceivedAt, Messages: ms,
	}
	seen := map[string]bool{}
	for _, m := range ms {
		if m.FromAddr != "" && !seen[m.FromAddr] {
			seen[m.FromAddr] = true
			t.Participants = append(t.Participants, m.FromAddr)
		}
	}
	return t, nil
}

// PrefixStat is one (zone, prefix) of the registry with what was
// delivered to it, the aggregation view of §3.3.
type PrefixStat struct {
	Zone      string
	Prefix    string
	State     int // PrefixClaimed or PrefixDiscovered
	Label     string
	FirstSeen time.Time
	LastSeen  time.Time
	// Messages counts the distinct visible messages delivered here;
	// Downgraded how many of them the §4.3 downgrade hides from the
	// default views (judged over all their visible deliveries).
	Messages   int
	Downgraded int
}

// PrefixSummary is the aggregation view: the prefixes and the distinct
// totals over all of them (a message sent to two prefixes counts once).
type PrefixSummary struct {
	Messages   int
	Downgraded int
	Prefixes   []PrefixStat
}

// PrefixStats returns every prefix with its counts, most recently used
// first, and the totals.
func (db *DB) PrefixStats(ctx context.Context, v *auth.Viewer) (PrefixSummary, error) {
	if err := needViewer(v, "prefixes"); err != nil {
		return PrefixSummary{}, err
	}
	// md: each visible message once, with its downgrade over all its
	// visible deliveries.
	md := `WITH md AS (
			SELECT d.message_id, NOT bool_or(` + notDowngraded + `) AS downgraded
			FROM deliveries d JOIN prefixes p ON p.zone = d.zone AND p.prefix = d.prefix
			WHERE d.message_id IS NOT NULL AND ` + visibleDelivery(v) + `
			GROUP BY d.message_id
		)`
	var out PrefixSummary
	err := pgx.BeginTxFunc(ctx, db.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, md+` SELECT count(*), count(*) FILTER (WHERE downgraded) FROM md`).Scan(&out.Messages, &out.Downgraded)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, md+`
			SELECT p.zone, p.prefix, p.state, coalesce(p.label, ''), p.first_seen, p.last_seen,
				count(DISTINCT md.message_id), count(DISTINCT md.message_id) FILTER (WHERE md.downgraded)
			FROM prefixes p
			LEFT JOIN deliveries d ON d.zone = p.zone AND d.prefix = p.prefix AND `+visibleDelivery(v)+`
			LEFT JOIN md ON md.message_id = d.message_id
			GROUP BY p.zone, p.prefix
			ORDER BY p.last_seen DESC, p.zone, p.prefix`)
		if err != nil {
			return err
		}
		out.Prefixes, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (PrefixStat, error) {
			var p PrefixStat
			err := r.Scan(&p.Zone, &p.Prefix, &p.State, &p.Label, &p.FirstSeen, &p.LastSeen, &p.Messages, &p.Downgraded)
			return p, err
		})
		return err
	})
	if err != nil {
		return PrefixSummary{}, fmt.Errorf("prefixes: %w", err)
	}
	return out, nil
}
