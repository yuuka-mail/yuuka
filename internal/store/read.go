package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/classify"
	"github.com/yuuka-mail/yuuka/internal/leak"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// The read paths of the WebUI and REST API: message list, message,
// thread and the prefix aggregation. Like Search, each takes the viewer it
// runs for and refuses a nil one (§13.8).

// errNoViewer is returned by every read called without a viewer.
var errNoViewer = errors.New("no viewer")

func needViewer(v *auth.Viewer, op string) error {
	if v == nil {
		return fmt.Errorf("%s: %w", op, errNoViewer)
	}
	return nil
}

// Visibility (§13.8) is decided here and nowhere else. Everything follows
// from one rule, which accounts a viewer may read:
//
//   - a delivery is visible when its account is readable;
//   - an outbox row, and with it its sent copy, when the account of the
//     identity it is sent as is readable (§4.6: sent mail has no
//     delivery, so the account comes from the identity);
//   - a message when it has a visible delivery or is the sent copy of a
//     visible outbox row.
//
// account_access is not enforced yet (M3.5): until then every account is
// readable to every viewer, so the conditions reduce to "has a delivery
// or a sent copy". The tests enforce it already (export_test.go), so the
// rule and every read path built on it are checked against restricted
// viewers before M3.5 switches it on.

// readableAccount is the condition "v may read the account in col".
func (db *DB) readableAccount(v *auth.Viewer, col string) string {
	if !db.enforceAccountAccess {
		return "true"
	}
	// The id is inlined, so only a well-formed one is: it is a ULID
	// (principals.id has that CHECK), and anything else reads nothing.
	if !ulid.Valid(v.PrincipalID) {
		return "false"
	}
	return `EXISTS (SELECT 1 FROM account_access aa WHERE aa.account_id = ` + col +
		` AND aa.principal_id = '` + v.PrincipalID + `' AND aa.can_read)`
}

// readsEveryAccount is the condition "v may read every account". It
// decides what is left of an object that is gone (a thread emptied by
// rethreading): nothing says any more which account it was in, so it is
// v's only if every account is.
func (db *DB) readsEveryAccount(v *auth.Viewer) string {
	return `NOT EXISTS (SELECT 1 FROM accounts ea WHERE NOT ` + db.readableAccount(v, "ea.id") + `)`
}

// visibleDelivery is the condition on a delivery aliased d.
func (db *DB) visibleDelivery(v *auth.Viewer, d string) string {
	return db.readableAccount(v, d+".account_id")
}

// visibleOutbox is the FROM and WHERE of the outbox rows (so) whose sent
// copy (sr) is the message aliased m and that v may see.
func (db *DB) visibleOutbox(v *auth.Viewer, m string) string {
	return `FROM outbox so JOIN raw_objects sr ON sr.id = so.raw_id JOIN identities si ON si.id = so.identity_id
		WHERE sr.content_hash = ` + m + `.content_hash AND ` + db.readableAccount(v, "si.account_id")
}

// visibleMessage is the condition on a message aliased m.
func (db *DB) visibleMessage(v *auth.Viewer, m string) string {
	return `(EXISTS (SELECT 1 FROM deliveries vd WHERE vd.message_id = ` + m + `.id AND ` + db.visibleDelivery(v, "vd") + `)
		OR EXISTS (SELECT 1 ` + db.visibleOutbox(v, m) + `))`
}

// outgoingJoin adds ob, the latest visible outbox row whose sent copy is
// message m, if any (several when an identical message was queued twice,
// or a recalled one queued again).
func (db *DB) outgoingJoin(v *auth.Viewer) string {
	return `
		LEFT JOIN LATERAL (SELECT so.id, so.state, so.from_addr ` + db.visibleOutbox(v, "m") + `
			ORDER BY so.created_at DESC, so.id DESC LIMIT 1) ob ON true`
}

// Outgoing marks a message as our own (§4.6): it is the sent copy of an
// outbox row. Messages that only have deliveries have none.
type Outgoing struct {
	OutboxID string
	// State is outbox.state (§4.5): queued, sending, sent, partial,
	// unknown, failed, dead, held or withdrawn. Whether it reached anyone
	// is this, not that the message exists: every outbox row's sent copy
	// is a message, whatever its fate.
	State string
	// From is outbox.from_addr: the address of the identity it was sent
	// as, with the prefix named for a *@zone identity.
	From string
}

func outgoing(id, state, from *string) *Outgoing {
	if id == nil {
		return nil
	}
	return &Outgoing{OutboxID: *id, State: *state, From: *from}
}

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
	// Classes selects by effective class (a human override wins); empty
	// means any. The WebUI's conversation view is human and unknown
	// together (§12.9).
	Classes    []classify.Class
	Downgraded Downgrade
	// Label, when set, keeps messages carrying it (exact,
	// case-sensitive). Archived, when set, keeps archived (true) or
	// unarchived (false) messages; nil means both.
	Label    string
	Archived *bool
	// Answer, when set, keeps messages whose latest score leans to an
	// option (§15): an observation to look through, not a verdict.
	Answer *AnswerFilter
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
	// Recipients are the prefix@zone of the visible deliveries, sorted;
	// empty for mail we only sent.
	Recipients []string
	// Outgoing is set for our own mail, from the latest visible outbox
	// row that sent it.
	Outgoing *Outgoing
	// Extracted is the best extraction of each kind (§3 principle 4),
	// in extractor order: what the notification zone shows instead of
	// the subject. The values are the sender's (§8.3).
	Extracted []Extraction
	// SenderVerified says whether a visible delivery proves the From
	// the reader sees: the trusted receiver reported dmarc=pass for that
	// From's organization (§10.1.1, leak.SenderOf). It is the rules'
	// from.verified, so the list never calls verified what such a rule
	// would not match. nil for mail with no visible delivery, which
	// nobody judged. It is a delivery fact: callers withhold it from
	// viewers without delivery:read.
	SenderVerified *bool
	// Labels and Archived are the annotations (§8.5): who set them is
	// in MessageAnnotations.
	Labels   []string
	Archived bool
}

// summaryCols and summaryFrom are shared by the list and the thread; the
// lateral subqueries compute the outgoing state, downgrade and recipients
// over what the viewer may see only. Our own mail is never downgraded:
// the §4.3 downgrade judges how mail arrived, and a sent copy did not.
const summaryCols = `m.id, m.thread_id, m.received_at, m.subject, m.from_addr, m.from_name, m.snippet,
	coalesce(s.class_override, m.class), g.downgraded, m.parse_error IS NOT NULL, m.has_attach, g.recipients,
	ob.id, ob.state, ob.from_addr, ex.items, g.auths, coalesce(s.labels, '{}'), coalesce(s.archived, false)`

func (db *DB) summaryFrom(v *auth.Viewer) string {
	return `
		FROM messages m
		LEFT JOIN message_state s ON s.message_id = m.id` + db.outgoingJoin(v) + `
		CROSS JOIN LATERAL (
			SELECT ob.id IS NULL AND NOT coalesce(bool_or(` + notDowngraded + `), false) AS downgraded,
				coalesce(array_agg(DISTINCT d.prefix || '@' || d.zone ORDER BY d.prefix || '@' || d.zone), '{}') AS recipients,
				jsonb_agg(d.auth ORDER BY d.received_at, d.id) AS auths
			FROM deliveries d JOIN prefixes p ON p.zone = d.zone AND p.prefix = d.prefix
			WHERE d.message_id = m.id AND ` + db.visibleDelivery(v, "d") + `) g` + bestExtractions + `
		WHERE ` + db.visibleMessage(v, "m")
}

// senderVerified is SenderVerified from the auth facts of the visible
// deliveries (nil: there are none) of a message whose From address is
// from. It is decided in Go, by the function the rules and the leak
// check use: whether a pass proves this From needs the organizational
// domain, which SQL does not know.
func senderVerified(auths []*authres.Auth, from string) *bool {
	if auths == nil {
		return nil
	}
	ok := slices.ContainsFunc(auths, func(a *authres.Auth) bool { return leak.SenderOf(a, from).Verified != "" })
	return &ok
}

// bestExtractions joins ex.items, the best extraction of each kind of
// message m as a JSON array in extractor order. The rows belong to m, so
// they are visible exactly when m is.
const bestExtractions = `
		CROSS JOIN LATERAL (
			SELECT coalesce(jsonb_agg(to_jsonb(e) ORDER BY e.ord), '[]') AS items
			FROM (SELECT DISTINCT ON (kind) ord, ` + extractionCols + ` FROM extractions
			      WHERE message_id = m.id ORDER BY kind, ord) e) ex`

func scanSummary(r pgx.CollectableRow) (MessageSummary, error) {
	var m MessageSummary
	var obID, obState, obFrom *string
	var auths []*authres.Auth
	err := r.Scan(&m.ID, &m.ThreadID, &m.ReceivedAt, &m.Subject, &m.FromAddr, &m.FromName, &m.Snippet,
		&m.Class, &m.Downgraded, &m.Quarantined, &m.HasAttach, &m.Recipients, &obID, &obState, &obFrom,
		&m.Extracted, &auths, &m.Labels, &m.Archived)
	m.Outgoing = outgoing(obID, obState, obFrom)
	m.SenderVerified = senderVerified(auths, m.FromAddr)
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
			arg(q.Zone)+` AND d.prefix = `+arg(q.Prefix)+` AND `+db.visibleDelivery(v, "d")+`)`)
	}
	if len(q.Classes) > 0 {
		cs := make([]int16, len(q.Classes))
		for i, c := range q.Classes {
			cs[i] = int16(c)
		}
		where = append(where, `coalesce(s.class_override, m.class) = ANY(`+arg(cs)+`)`)
	}
	where = append(where, annotationFilter(q.Label, q.Archived, arg)...)
	if q.Answer != nil {
		if err := q.Answer.valid(); err != nil {
			return nil, fmt.Errorf("list messages: %w", err)
		}
		where = append(where, q.Answer.condition(arg))
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
	sql := `SELECT ` + summaryCols + db.summaryFrom(v)
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
	// Leak is the leak check of this delivery (§3 principle 3), judged
	// now against the prefix's expected domains; no verdict for a
	// discovered prefix, a prefix that expects nothing yet, or a DMARC
	// failure that does not claim an expected sender (leak.Judge).
	Leak leak.Judgement
	// Raw locates the raw message this delivery carried. Rendering reads
	// the raw of a visible delivery only (§13.8: raw and auth come from
	// the readable path).
	Raw RawRef
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
	// Extractions are all the extractor found, in its order; the
	// summary's Extracted is the best of each kind.
	Extractions []Extraction
	Deliveries  []DeliveryFact
	// ReplyToCounts: its Reply-To counts as v sees it, as it does for
	// the thread correspondents of §8.9 (replyToCounts). A reply goes to
	// the Reply-To only then.
	ReplyToCounts bool
	// Raw is the raw to render the message from: that of the earliest
	// visible delivery, or for mail we only sent, the earliest visible
	// sent copy (DeliveryID empty). The content is the same in all of
	// them (§4.1); readers must not assume a delivery exists.
	Raw RawRef
}

// MessageByID returns message id as v sees it. An id merged away by a
// content-hash change resolves to its survivor (§4.2), whose id the
// result carries. ErrNotFound if it does not exist or is not visible: a
// message is readable through a visible delivery or a visible sent copy,
// or not at all (§13.8). Deliveries come earliest first.
func (db *DB) MessageByID(ctx context.Context, v *auth.Viewer, id string) (MessageDetail, error) {
	if err := needViewer(v, "message"); err != nil {
		return MessageDetail{}, err
	}
	var d MessageDetail
	err := pgx.BeginTxFunc(ctx, db.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+summaryCols+`,
				coalesce(m.message_id, ''), coalesce(m.in_reply_to, ''), m.refs, m.date_header, m.body_text, coalesce(m.parse_error, ''),
				`+db.replyToCounts(v, "m")+`
			`+db.summaryFrom(v)+`
			AND m.id = coalesce((SELECT new_id FROM message_aliases WHERE old_id = $1), $1)`, id)
		if err != nil {
			return err
		}
		d, err = pgx.CollectExactlyOneRow(rows, func(r pgx.CollectableRow) (MessageDetail, error) {
			var d MessageDetail
			m := &d.MessageSummary
			var obID, obState, obFrom *string
			var auths []*authres.Auth
			err := r.Scan(&m.ID, &m.ThreadID, &m.ReceivedAt, &m.Subject, &m.FromAddr, &m.FromName, &m.Snippet,
				&m.Class, &m.Downgraded, &m.Quarantined, &m.HasAttach, &m.Recipients, &obID, &obState, &obFrom,
				&m.Extracted, &auths, &m.Labels, &m.Archived, &d.MessageID, &d.InReplyTo, &d.References, &d.Date, &d.BodyText, &d.ParseError, &d.ReplyToCounts)
			m.Outgoing = outgoing(obID, obState, obFrom)
			m.SenderVerified = senderVerified(auths, m.FromAddr)
			return d, err
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `
			SELECT d.id, d.zone, d.prefix, p.state, d.envelope_from, d.envelope_to, d.received_at, d.auth,
				`+judged+`, p.expected_domains, `+rawRefCols+`
			FROM deliveries d JOIN prefixes p ON p.zone = d.zone AND p.prefix = d.prefix
			JOIN raw_objects r ON r.id = d.raw_id
			WHERE d.message_id = $1 AND `+db.visibleDelivery(v, "d")+`
			ORDER BY d.received_at, d.id`, d.ID)
		if err != nil {
			return err
		}
		d.Deliveries, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (DeliveryFact, error) {
			var f DeliveryFact
			var isJudged bool
			var expected []string
			raw := &f.Raw
			err := r.Scan(&f.ID, &f.Zone, &f.Prefix, &f.PrefixState, &f.EnvelopeFrom, &f.EnvelopeTo, &f.ReceivedAt, &f.Auth,
				&isJudged, &expected,
				&raw.DeliveryID, &raw.RawID, &raw.R2Key, &raw.LocalPath, &raw.ContentHash, &raw.Zone, &raw.Prefix, &raw.ReceivedAt)
			if isJudged {
				f.Leak = leak.Judge(expected, leak.SenderOf(f.Auth, d.FromAddr))
			}
			return f, err
		})
		if err != nil {
			return err
		}
		// The message is visible to v: its derived rows are too.
		if d.Extractions, err = extractionsOf(ctx, tx, d.ID); err != nil {
			return err
		}
		if len(d.Deliveries) > 0 {
			d.Raw = d.Deliveries[0].Raw
			return nil
		}
		d.Raw, err = scanRawRef(tx.QueryRow(ctx, `
			SELECT `+sentRawCols+`
			FROM messages m CROSS JOIN LATERAL (SELECT sr.* `+db.visibleOutbox(v, "m")+`) sr
			WHERE m.id = $1
			ORDER BY sr.received_at, sr.id LIMIT 1`, d.ID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
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
	rows, err := db.pool.Query(ctx, `SELECT `+summaryCols+db.summaryFrom(v)+`
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
	Zone   string
	Prefix string
	State  int // PrefixClaimed or PrefixDiscovered
	// Label, Note and ExpectedDomains are the operator's.
	Label           string
	Note            string
	ExpectedDomains []string
	// FirstSeen and LastSeen span the deliveries; both are nil for a
	// prefix that was claimed before any mail reached it.
	FirstSeen *time.Time
	LastSeen  *time.Time
	Active    bool
	// Deliveries counts the visible deliveries to this prefix; Messages
	// the distinct visible messages among them; Downgraded how many of
	// those the §4.3 downgrade hides from the default views (judged over
	// all their visible deliveries).
	Deliveries int
	Messages   int
	Downgraded int
	// LeakStats is the leak check of a claimed prefix (§3 principle 3)
	// over the same visible mail.
	LeakStats
}

// PrefixQuery narrows PrefixStats.
type PrefixQuery struct {
	// LeakSuspect, when set, keeps only prefixes whose leak check found
	// (true) or did not find (false) a leak or a spoof.
	LeakSuspect *bool
	// State, when non-zero, keeps only PrefixClaimed or PrefixDiscovered.
	State int
	// Zone, when set, keeps only that zone (lowercase, as ingest stores
	// it); Prefix, when also set, only that prefix of it.
	Zone, Prefix string
	// Limit caps the prefixes returned, most recently seen first; 0
	// returns all of them. Offset skips that many first: the unclaimed
	// list runs to thousands under a dictionary attack (§12.9).
	Limit, Offset int
}

// PrefixSummary is the aggregation view: the prefixes and the distinct
// totals over all visible mail (a message sent to two prefixes counts
// once). The totals do not depend on the query; Total does.
type PrefixSummary struct {
	Messages   int
	Downgraded int
	// Total is how many prefixes matched before Limit.
	Total    int
	Prefixes []PrefixStat
}

// PrefixStats returns the prefixes q selects with their counts, most
// recently used first (never-used claimed prefixes last), and the totals.
func (db *DB) PrefixStats(ctx context.Context, v *auth.Viewer, q PrefixQuery) (PrefixSummary, error) {
	if err := needViewer(v, "prefixes"); err != nil {
		return PrefixSummary{}, err
	}
	var out PrefixSummary
	err := pgx.BeginTxFunc(ctx, db.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		out, err = db.prefixStats(ctx, tx, v, q)
		return err
	})
	if err != nil {
		return PrefixSummary{}, fmt.Errorf("prefixes: %w", err)
	}
	return out, nil
}

func (db *DB) prefixStats(ctx context.Context, tx pgx.Tx, v *auth.Viewer, q PrefixQuery) (PrefixSummary, error) {
	if q.Limit < 0 || q.Offset < 0 {
		return PrefixSummary{}, errors.New("negative limit or offset")
	}
	if q.State != 0 && q.State != PrefixClaimed && q.State != PrefixDiscovered {
		return PrefixSummary{}, fmt.Errorf("state %d", q.State)
	}
	if q.Prefix != "" && q.Zone == "" {
		return PrefixSummary{}, errors.New("a prefix needs its zone")
	}
	// md: each visible message once, with its downgrade over all its
	// visible deliveries.
	md := `WITH md AS (
			SELECT d.message_id, NOT bool_or(` + notDowngraded + `) AS downgraded
			FROM deliveries d JOIN prefixes p ON p.zone = d.zone AND p.prefix = d.prefix
			WHERE d.message_id IS NOT NULL AND ` + db.visibleDelivery(v, "d") + `
			GROUP BY d.message_id
		)`
	// The filters are nullable parameters so that both statements share
	// them; a NULL limit is no limit.
	const where = `WHERE ($1::smallint IS NULL OR p.state = $1) AND ($2::text IS NULL OR p.zone = $2)
		AND ($3::text IS NULL OR p.prefix = $3)`
	var state *int16
	if q.State != 0 {
		s := int16(q.State)
		state = &s
	}
	filter := []any{state, nullable(q.Zone), nullable(q.Prefix)}
	// The leak check is judged in Go, so filtering by it pages in Go.
	var limit *int
	offset := q.Offset
	if q.LeakSuspect != nil {
		offset = 0
	} else if q.Limit > 0 {
		limit = &q.Limit
	}
	var out PrefixSummary
	err := tx.QueryRow(ctx, md+` SELECT count(*), count(*) FILTER (WHERE downgraded),
			(SELECT count(*) FROM prefixes p `+where+`) FROM md`, filter...).
		Scan(&out.Messages, &out.Downgraded, &out.Total)
	if err != nil {
		return PrefixSummary{}, err
	}
	rows, err := tx.Query(ctx, md+`
		SELECT p.zone, p.prefix, p.state, coalesce(p.label, ''), coalesce(p.note, ''), p.expected_domains,
			p.first_seen, p.last_seen, p.active, count(d.id),
			count(DISTINCT md.message_id), count(DISTINCT md.message_id) FILTER (WHERE md.downgraded)
		FROM prefixes p
		LEFT JOIN deliveries d ON d.zone = p.zone AND d.prefix = p.prefix AND `+db.visibleDelivery(v, "d")+`
		LEFT JOIN md ON md.message_id = d.message_id
		`+where+`
		GROUP BY p.zone, p.prefix
		ORDER BY p.last_seen DESC NULLS LAST, p.zone, p.prefix
		LIMIT $4 OFFSET $5`, append(filter, limit, offset)...)
	if err != nil {
		return PrefixSummary{}, err
	}
	out.Prefixes, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (PrefixStat, error) {
		var p PrefixStat
		err := r.Scan(&p.Zone, &p.Prefix, &p.State, &p.Label, &p.Note, &p.ExpectedDomains,
			&p.FirstSeen, &p.LastSeen, &p.Active, &p.Deliveries, &p.Messages, &p.Downgraded)
		return p, err
	})
	if err != nil {
		return PrefixSummary{}, err
	}
	if err := db.judgePrefixes(ctx, tx, v, out.Prefixes); err != nil {
		return PrefixSummary{}, err
	}
	if q.LeakSuspect != nil {
		out.Prefixes = slices.DeleteFunc(out.Prefixes, func(p PrefixStat) bool { return p.Suspect() != *q.LeakSuspect })
		out.Total = len(out.Prefixes)
		out.Prefixes = out.Prefixes[min(q.Offset, len(out.Prefixes)):]
		if q.Limit > 0 {
			out.Prefixes = out.Prefixes[:min(q.Limit, len(out.Prefixes))]
		}
	}
	return out, nil
}

// annotationFilter is the label and archived conditions on message_state
// s shared by the list and search. labels <> '{}' is implied by the
// containment; it is spelled out so the partial GIN index applies.
func annotationFilter(label string, archived *bool, arg func(any) string) []string {
	var where []string
	if label != "" {
		where = append(where, `s.labels <> '{}' AND s.labels @> ARRAY[`+arg(label)+`]::text[]`)
	}
	if archived != nil {
		where = append(where, `coalesce(s.archived, false) = `+arg(*archived))
	}
	return where
}
