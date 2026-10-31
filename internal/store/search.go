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
	"github.com/yuuka-mail/yuuka/internal/search"
	"github.com/yuuka-mail/yuuka/internal/triage/score"
)

// SearchQuery is one page of a search (§16).
type SearchQuery struct {
	// Filter is the parsed query (search.Parse); nil matches everything.
	Filter *search.Filter
	Limit  int // default 50, at most 500
	// Before returns hits strictly after this position in the order
	// (received_at, id), both descending; nil starts at the newest. As in
	// the message list, keyset paging keeps pages stable while mail
	// arrives, which an offset would not. Total ignores it.
	Before *MessageCursor
	// IncludeDowngraded also returns mail the §4.3 minimal downgrade
	// hides from aggregate views (discovered prefixes, dmarc=fail). A
	// filter that names is:downgraded anywhere includes it as well:
	// hiding it would make that condition find nothing.
	IncludeDowngraded bool
	// Triage is the active scorer's profile, which triage: conditions
	// read (§15); nil when none is configured.
	Triage *score.Profile
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

	c := &compiler{db: db, v: v, triage: q.Triage}
	where := []string{db.visibleMessage(v, "m")}
	if q.Filter != nil {
		w, err := c.filter(q.Filter)
		if err != nil {
			return SearchResult{}, fmt.Errorf("search: %w", err)
		}
		where = append(where, w)
	}
	if !q.IncludeDowngraded && !q.Filter.Uses(search.Downgraded) {
		where = append(where, `NOT g.downgraded`)
	}
	args := c.args
	// downgraded is a lateral column so the filter can use it by name.
	// As in the list, our own mail is never downgraded.
	joins := `
		FROM messages m
		LEFT JOIN message_state s ON s.message_id = m.id` + db.outgoingJoin(v) + `
		CROSS JOIN LATERAL (SELECT ob.id IS NULL AND NOT EXISTS (
			SELECT 1 FROM deliveries d JOIN prefixes p ON p.zone = d.zone AND p.prefix = d.prefix
			WHERE d.message_id = m.id AND ` + db.visibleDelivery(v, "d") + ` AND ` + notDowngraded + `) AS downgraded) g`
	filter := `
		WHERE ` + strings.Join(where, "\n\t\tAND ")

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
				coalesce(s.class_override, m.class), g.downgraded, m.parse_error IS NOT NULL, ob.id, ob.state, ob.from_addr,
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
