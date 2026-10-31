package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/auth"
)

// Counts are what a sidebar shows beside its sections, each counted
// over the rows the list it opens would show v (§13.8): outbox rows as
// OutboxList, proposals as Proposals, drafts as Drafts.
type Counts struct {
	// OutboxHeld waits out a recall window (§8.9).
	OutboxHeld int
	// OutboxUnknown is mail whose delivery is unknown: only a person
	// resolves it (§4.5).
	OutboxUnknown int
	// OutboxAttention is unknown, failed or partial: something went
	// wrong that a person may want to look at.
	OutboxAttention int
	// Proposals are pending and not expired.
	Proposals int
	// DraftsOpen are being written; DraftsHeld stopped at the draft
	// (a hold reason, no outbox row).
	DraftsOpen int
	DraftsHeld int
}

// Counts counts for v, in one statement, at now (which expires
// proposals as Proposals does).
func (db *DB) Counts(ctx context.Context, v *auth.Viewer, now time.Time) (Counts, error) {
	if err := needViewer(v, "counts"); err != nil {
		return Counts{}, err
	}
	var c Counts
	err := db.pool.QueryRow(ctx, `
		SELECT ob.held, ob.unknown, ob.attention, pp.pending, dd.open, dd.held
		FROM (SELECT count(*) FILTER (WHERE o.state = 'held') AS held,
		             count(*) FILTER (WHERE o.state = 'unknown') AS unknown,
		             count(*) FILTER (WHERE o.state IN ('unknown', 'failed', 'partial')) AS attention
		      FROM outbox o JOIN identities i ON i.id = o.identity_id
		      WHERE o.state IN ('held', 'unknown', 'failed', 'partial') AND `+db.readableAccount(v, "i.account_id")+`) ob,
		     (SELECT count(*) AS pending FROM proposals pr
		      WHERE pr.state = 'pending' AND pr.expires_at > $2 AND `+db.proposalVisible(v)+`) pp,
		     (SELECT count(*) FILTER (WHERE dr.hold_reason IS NULL) AS open,
		             count(*) FILTER (WHERE dr.hold_reason IS NOT NULL) AS held
		      FROM drafts dr
		      WHERE `+visibleDraft(v)+`
		        AND NOT EXISTS (SELECT 1 FROM outbox WHERE draft_id = dr.id AND state <> 'withdrawn')) dd`,
		v.PrincipalID, now).
		Scan(&c.OutboxHeld, &c.OutboxUnknown, &c.OutboxAttention, &c.Proposals, &c.DraftsOpen, &c.DraftsHeld)
	if err != nil {
		return Counts{}, fmt.Errorf("counts: %w", err)
	}
	return c, nil
}

// Subjects returns, by id, the subjects of the messages of ids that v
// may see (an alias resolves to the message it was merged into). One
// v cannot see, or without a subject, is left out: a restricted viewer
// may know hidden mail exists, never its headers (§13.8).
func (db *DB) Subjects(ctx context.Context, v *auth.Viewer, ids []string) (map[string]string, error) {
	if err := needViewer(v, "subjects"); err != nil {
		return nil, err
	}
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := db.pool.Query(ctx, `
		SELECT q.id, m.subject FROM unnest($1::text[]) q(id)
		JOIN messages m ON m.id = coalesce((SELECT new_id FROM message_aliases WHERE old_id = q.id), q.id)
		WHERE m.subject <> '' AND `+db.visibleMessage(v, "m"), ids)
	if err != nil {
		return nil, fmt.Errorf("subjects: %w", err)
	}
	var id, subject string
	if _, err := pgx.ForEachRow(rows, []any{&id, &subject}, func() error {
		out[id] = subject
		return nil
	}); err != nil {
		return nil, fmt.Errorf("subjects: %w", err)
	}
	return out, nil
}
