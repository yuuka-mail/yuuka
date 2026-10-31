package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/classify"
)

// SearchQuery is a substring search over subject, sender and body.
type SearchQuery struct {
	// Text is split on whitespace; every term must occur (case-insensitive
	// substring, which also works for Chinese without segmentation).
	// Empty matches everything.
	Text  string
	Limit int // default 50, at most 500
	// Before returns hits strictly after this position in the order
	// (received_at, id), both descending; nil starts at the newest. As in
	// the message list, keyset paging keeps pages stable while mail
	// arrives, which an offset would not. Total ignores it.
	Before *MessageCursor
	// IncludeDowngraded also returns mail the §4.3 minimal downgrade
	// hides from aggregate views (discovered prefixes, dmarc=fail).
	IncludeDowngraded bool
	// Prefix and Zone, when set, keep messages with a delivery to that
	// local part (and domain).
	Prefix string
	Zone   string
	// Class, when set, keeps messages of that effective class.
	Class *classify.Class
	// Since, when set, keeps messages received at or after it.
	Since time.Time
	// Label and Archived narrow as in MessageFilter.
	Label    string
	Archived *bool
}

// SearchHit is one message in a search result.
type SearchHit struct {
	ID         string
	ThreadID   string
	ReceivedAt time.Time
	Subject    string
	FromAddr   string
	FromName   string
	Snippet    string
	// Class is the effective class: a human override wins (§4.2).
	Class       classify.Class
	Downgraded  bool
	Quarantined bool
	// Outgoing is set for our own mail (§4.6), as in MessageSummary.
	Outgoing *Outgoing
	// Extracted, SenderVerified, Labels and Archived are as in
	// MessageSummary.
	Extracted      []Extraction
	SenderVerified *bool
	Labels         []string
	Archived       bool
}

// SearchResult is a page of hits, newest first, and the total number of
// matches.
type SearchResult struct {
	Total int
	Hits  []SearchHit
}

// searchDoc must stay identical to the expression of the messages_search
// trigram index, or the index is not used.
const searchDoc = `(m.subject || ' ' || m.from_name || ' ' || m.from_addr || ' ' || m.body_text)`

// Search runs q for v. Total and hits are computed over the same set in
// one snapshot, so they agree.
//
// Every read function of the store takes the viewer it runs for (§13.8):
// the hits, the total and the downgrade are computed over what v may see
// (visibleMessage). A nil viewer is refused rather than treated as
// "everyone", so a forgotten viewer fails loudly.
func (db *DB) Search(ctx context.Context, v *auth.Viewer, q SearchQuery) (SearchResult, error) {
	if v == nil {
		return SearchResult{}, errors.New("search: no viewer")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 500)

	var where []string
	args := []any{}
	for term := range strings.FieldsSeq(q.Text) {
		args = append(args, "%"+likeEscape(term)+"%")
		where = append(where, fmt.Sprintf("%s ILIKE $%d", searchDoc, len(args)))
	}
	if q.Prefix != "" || q.Zone != "" {
		args = append(args, q.Prefix, q.Zone)
		where = append(where, fmt.Sprintf(`EXISTS (SELECT 1 FROM deliveries d WHERE d.message_id = m.id
			AND ($%d = '' OR d.prefix = $%[1]d) AND ($%d = '' OR d.zone = $%[2]d) AND %s)`,
			len(args)-1, len(args), db.visibleDelivery(v, "d")))
	}
	if q.Class != nil {
		args = append(args, *q.Class)
		where = append(where, fmt.Sprintf(`coalesce(s.class_override, m.class) = $%d`, len(args)))
	}
	if !q.Since.IsZero() {
		args = append(args, q.Since)
		where = append(where, fmt.Sprintf(`m.received_at >= $%d`, len(args)))
	}
	where = append(where, annotationFilter(q.Label, q.Archived, func(a any) string {
		args = append(args, a)
		return fmt.Sprintf("$%d", len(args))
	})...)
	if !q.IncludeDowngraded {
		where = append(where, `NOT downgraded`)
	}
	where = append([]string{db.visibleMessage(v, "m")}, where...)
	// downgraded is a lateral column so the filter can use it by name.
	// As in the list, our own mail is never downgraded.
	joins := `
		FROM messages m
		LEFT JOIN message_state s ON s.message_id = m.id` + db.outgoingJoin(v) + `
		CROSS JOIN LATERAL (SELECT ob.id IS NULL AND NOT EXISTS (
			SELECT 1 FROM deliveries d JOIN prefixes p ON p.zone = d.zone AND p.prefix = d.prefix
			WHERE d.message_id = m.id AND ` + db.visibleDelivery(v, "d") + ` AND ` + notDowngraded + `) AS downgraded) g`
	filter := `
		WHERE ` + strings.Join(where, " AND ")

	var res SearchResult
	err := pgx.BeginTxFunc(ctx, db.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) `+joins+filter, args...).Scan(&res.Total); err != nil {
			return err
		}
		// Only the page needs the extractions, not the count.
		page, pageArgs := joins+hitExtras(db, v)+filter, slices.Clone(args)
		if q.Before != nil {
			pageArgs = append(pageArgs, q.Before.ReceivedAt, q.Before.ID)
			page += fmt.Sprintf(` AND (m.received_at, m.id) < ($%d, $%d)`, len(pageArgs)-1, len(pageArgs))
		}
		pageArgs = append(pageArgs, limit)
		rows, err := tx.Query(ctx, fmt.Sprintf(`
			SELECT m.id, m.thread_id, m.received_at, m.subject, m.from_addr, m.from_name, m.snippet,
				coalesce(s.class_override, m.class), downgraded, m.parse_error IS NOT NULL, ob.id, ob.state, ob.from_addr,
				hx.items, hx.auths, coalesce(s.labels, '{}'), coalesce(s.archived, false)
			%s
			ORDER BY m.received_at DESC, m.id DESC
			LIMIT $%d`, page, len(pageArgs)), pageArgs...)
		if err != nil {
			return err
		}
		res.Hits, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (SearchHit, error) {
			var h SearchHit
			var obID, obState, obFrom *string
			var auths []*authres.Auth
			err := r.Scan(&h.ID, &h.ThreadID, &h.ReceivedAt, &h.Subject, &h.FromAddr, &h.FromName,
				&h.Snippet, &h.Class, &h.Downgraded, &h.Quarantined, &obID, &obState, &obFrom,
				&h.Extracted, &auths, &h.Labels, &h.Archived)
			h.Outgoing = outgoing(obID, obState, obFrom)
			h.SenderVerified = senderVerified(auths, h.FromAddr)
			return h, err
		})
		return err
	})
	if err != nil {
		return SearchResult{}, fmt.Errorf("search: %w", err)
	}
	return res, nil
}

// hitExtras joins hx for a page of hits: the best extractions and the
// auth facts of the visible deliveries, which decide SenderVerified as
// in summaries.
func hitExtras(db *DB, v *auth.Viewer) string {
	return bestExtractions + `
		CROSS JOIN LATERAL (SELECT ex.items, (SELECT jsonb_agg(d.auth ORDER BY d.received_at, d.id) FROM deliveries d
			WHERE d.message_id = m.id AND ` + db.visibleDelivery(v, "d") + `) AS auths) hx`
}

// likeEscape makes a search term literal inside an ILIKE pattern, whose
// default escape character is the backslash.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
