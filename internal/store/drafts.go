package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// ErrInFlight: the draft has gone to the outbox (or has outbox history),
// so it can no longer be edited or deleted.
var ErrInFlight = errors.New("the draft is in the outbox")

// Draft is a row of drafts (§4, §8.9).
type Draft struct {
	ID string
	// PrincipalID wrote it; its grant applies when it is submitted.
	PrincipalID string
	// IdentityID always follows from FromAddr (ResolveIdentity): the
	// writes resolve it, so a draft cannot pair an address with an
	// identity that does not cover it.
	IdentityID  string
	FromAddr    string
	InReplyToID string // "" for a new thread
	To, Cc, Bcc []string
	Subject     string
	// Exactly one body: BodyMD for composed mail, RawID for an SMTP
	// submission held as a draft (already MIME, stored under sent/).
	BodyMD string
	RawID  string
	// Cites is a JSON array of {uri, quote}; nil writes [].
	Cites      json.RawMessage
	HoldReason sendpolicy.HoldReason // "" for a draft never submitted
	HoldDetail string
	CreatedAt  time.Time
	UpdatedAt  time.Time

	// Read only: the newest outbox row made from it that was not
	// withdrawn, "" when none, i.e. while it is still a draft.
	OutboxID    string
	OutboxState string
}

// Who sees which drafts. M2: humans see all of them, everyone else the
// drafts they wrote, like the audit log of §13.8 (an agent sees what it
// started). A draft is unsent text, often written on someone's behalf,
// so it is narrower than mail. §13.8 leaves the M3.5 rule (by the
// identity's account as well?) open; it goes here and nowhere else.
// Every draft query passes the viewer's principal id as $1.
func visibleDraft(v *auth.Viewer) string {
	if v.PrincipalKind == sendpolicy.KindHuman {
		return "$1::text IS NOT NULL" // $1 must appear for its type
	}
	return "dr.principal_id = $1"
}

const draftCols = `dr.id, dr.principal_id, dr.identity_id, dr.from_addr, coalesce(dr.in_reply_to_id, ''),
	dr.to_addrs, dr.cc_addrs, dr.bcc_addrs, dr.subject, coalesce(dr.body_md, ''), coalesce(dr.raw_id, ''),
	dr.cites, coalesce(dr.hold_reason, ''), coalesce(dr.hold_detail, ''), dr.created_at, dr.updated_at,
	coalesce(o.id, ''), coalesce(o.state, '')`

// draftFrom joins the newest outbox row that was not withdrawn.
const draftFrom = ` FROM drafts dr
	LEFT JOIN LATERAL (
		SELECT id, state FROM outbox WHERE draft_id = dr.id AND state <> 'withdrawn'
		ORDER BY created_at DESC, id DESC LIMIT 1) o ON true`

func scanDraft(row pgx.Row) (Draft, error) {
	var d Draft
	var reason string
	err := row.Scan(&d.ID, &d.PrincipalID, &d.IdentityID, &d.FromAddr, &d.InReplyToID,
		&d.To, &d.Cc, &d.Bcc, &d.Subject, &d.BodyMD, &d.RawID,
		&d.Cites, &reason, &d.HoldDetail, &d.CreatedAt, &d.UpdatedAt, &d.OutboxID, &d.OutboxState)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrNotFound
	}
	d.HoldReason = sendpolicy.HoldReason(reason)
	return d, err
}

// DraftByID returns draft id if v may see it, ErrNotFound otherwise.
func (db *DB) DraftByID(ctx context.Context, v *auth.Viewer, id string) (Draft, error) {
	if err := needViewer(v, "draft"); err != nil {
		return Draft{}, err
	}
	return draftByID(ctx, db.pool, v, id, false)
}

func draftByID(ctx context.Context, q querier, v *auth.Viewer, id string, forUpdate bool) (Draft, error) {
	sql := `SELECT ` + draftCols + draftFrom + ` WHERE dr.id = $2 AND ` + visibleDraft(v)
	if forUpdate {
		sql += ` FOR UPDATE OF dr`
	}
	d, err := scanDraft(q.QueryRow(ctx, sql, v.PrincipalID, id))
	if err != nil {
		return d, fmt.Errorf("draft %s: %w", id, err)
	}
	return d, nil
}

// DraftQuery selects drafts.
type DraftQuery struct {
	// InFlight also returns drafts that went to the outbox.
	InFlight bool
	Limit    int // default 100, at most 500
}

// Drafts lists the drafts v may see, most recently changed first.
func (db *DB) Drafts(ctx context.Context, v *auth.Viewer, q DraftQuery) ([]Draft, error) {
	if err := needViewer(v, "drafts"); err != nil {
		return nil, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 500)
	rows, err := db.pool.Query(ctx, `SELECT `+draftCols+draftFrom+`
		WHERE `+visibleDraft(v)+` AND ($2 OR o.id IS NULL)
		ORDER BY dr.updated_at DESC, dr.id DESC LIMIT $3`, v.PrincipalID, q.InFlight, limit)
	if err != nil {
		return nil, fmt.Errorf("list drafts: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Draft, error) { return scanDraft(r) })
	if err != nil {
		return nil, fmt.Errorf("list drafts: %w", err)
	}
	return out, nil
}

// checkAddrs refuses what cannot be an envelope recipient: an address
// without @, or with whitespace or control characters (header and
// command injection).
func checkAddrs(role string, addrs []string) error {
	for _, a := range addrs {
		if !strings.Contains(a, "@") || strings.IndexFunc(a, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
			return fmt.Errorf("%s address %q: want a bare address", role, a)
		}
	}
	return nil
}

// prepareDraft validates d's content and resolves its identity and its
// replied-to message as v sees them.
func (db *DB) prepareDraft(ctx context.Context, tx pgx.Tx, v *auth.Viewer, d *Draft) error {
	for _, l := range []struct {
		role  string
		addrs *[]string
	}{{"to", &d.To}, {"cc", &d.Cc}, {"bcc", &d.Bcc}} {
		if *l.addrs == nil {
			*l.addrs = []string{}
		}
		if err := checkAddrs(l.role, *l.addrs); err != nil {
			return err
		}
	}
	if d.Cites == nil {
		d.Cites = json.RawMessage("[]")
	}
	var cites []json.RawMessage
	if err := json.Unmarshal(d.Cites, &cites); err != nil {
		return fmt.Errorf("cites: want a JSON array: %w", err)
	}
	ident, from, err := resolveIdentity(ctx, tx, d.FromAddr)
	if err != nil {
		return err
	}
	if d.IdentityID != "" && d.IdentityID != ident.ID {
		return fmt.Errorf("from %s goes out as identity %s (%s), not %s", from, ident.ID, ident.Address(), d.IdentityID)
	}
	d.IdentityID, d.FromAddr = ident.ID, from
	if d.InReplyToID != "" {
		err := tx.QueryRow(ctx, `SELECT m.id FROM messages m
			WHERE m.id = coalesce((SELECT new_id FROM message_aliases WHERE old_id = $1), $1)
			  AND `+db.visibleMessage(v, "m"), d.InReplyToID).Scan(&d.InReplyToID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("in reply to %s: %w", d.InReplyToID, ErrNotFound)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// draftAction is the audit entry of a draft change by v. It names the
// draft and its addressing, never the body.
func draftAction(v *auth.Viewer, tool string, d Draft, at time.Time) Action {
	args := map[string]any{"draft_id": d.ID}
	if d.IdentityID != "" {
		args["identity_id"], args["from"] = d.IdentityID, d.FromAddr
	}
	if d.InReplyToID != "" {
		args["in_reply_to_id"] = d.InReplyToID
	}
	if d.HoldReason != "" {
		args["hold_reason"] = d.HoldReason
	}
	return Action{ID: ulid.New(at), Actor: v.PrincipalID, CredentialID: v.CredentialID, Tool: tool, At: at, Args: args}
}

// logDraftChange appends a 'draft' change. It takes the content lock so
// that changes.seq stays in commit order with the content writers.
func logDraftChange(ctx context.Context, tx pgx.Tx, id string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, contentLock); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO changes (kind, target_id) VALUES ('draft', $1)`, id); err != nil {
		return fmt.Errorf("log change: %w", err)
	}
	return nil
}

// CreateDraft stores a new draft written by v at time at and returns it.
// d.ID, PrincipalID and the timestamps are set here; d.IdentityID
// follows from d.FromAddr (ErrNoIdentity if nothing covers it). A
// HoldReason records a submission that stopped here, such as an SMTP
// submission held with its RawID. Recorded as draft.create.
func (db *DB) CreateDraft(ctx context.Context, v *auth.Viewer, d Draft, at time.Time) (Draft, error) {
	if err := needViewer(v, "create draft"); err != nil {
		return Draft{}, err
	}
	if d.RawID != "" && d.BodyMD != "" {
		return Draft{}, errors.New("create draft: a body or a raw, not both")
	}
	d.ID, d.PrincipalID, d.CreatedAt, d.UpdatedAt = ulid.New(at), v.PrincipalID, at, at
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if err := db.prepareDraft(ctx, tx, v, &d); err != nil {
			return err
		}
		var body any = d.BodyMD
		if d.RawID != "" {
			body = nil
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO drafts (id, principal_id, identity_id, from_addr, in_reply_to_id, to_addrs, cc_addrs, bcc_addrs,
			                    subject, body_md, raw_id, cites, hold_reason, hold_detail, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $15)`,
			d.ID, d.PrincipalID, d.IdentityID, d.FromAddr, nullable(d.InReplyToID), d.To, d.Cc, d.Bcc,
			d.Subject, body, nullable(d.RawID), d.Cites, nullable(string(d.HoldReason)), nullable(d.HoldDetail), at)
		if err != nil {
			return fmt.Errorf("insert draft: %w", err)
		}
		if err := logDraftChange(ctx, tx, d.ID); err != nil {
			return err
		}
		return insertAction(ctx, tx, draftAction(v, "draft.create", d, at))
	})
	if err != nil {
		return Draft{}, fmt.Errorf("create draft: %w", err)
	}
	return d, nil
}

// UpdateDraft replaces the content of draft d.ID (From, reply target,
// recipients, subject, body, cites) as v at time at. Only Markdown drafts
// are edited; an SMTP submission's raw is what its service sent. Editing
// clears the hold reason: it described a submission of other content,
// and submitting again decides afresh. ErrInFlight once the draft has
// gone to the outbox; ErrNotFound if v cannot see it. Recorded as
// draft.update.
func (db *DB) UpdateDraft(ctx context.Context, v *auth.Viewer, d Draft, at time.Time) (Draft, error) {
	if err := needViewer(v, "update draft"); err != nil {
		return Draft{}, err
	}
	var out Draft
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		cur, err := draftByID(ctx, tx, v, d.ID, true)
		if err != nil {
			return err
		}
		if cur.RawID != "" {
			return errors.New("an SMTP submission held as a draft cannot be edited")
		}
		if err := notInFlight(ctx, tx, d.ID); err != nil {
			return err
		}
		d.IdentityID = "" // follows from FromAddr
		if err := db.prepareDraft(ctx, tx, v, &d); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE drafts SET identity_id = $2, from_addr = $3, in_reply_to_id = $4, to_addrs = $5, cc_addrs = $6,
				bcc_addrs = $7, subject = $8, body_md = $9, cites = $10, hold_reason = NULL, hold_detail = NULL,
				updated_at = $11
			WHERE id = $1`,
			d.ID, d.IdentityID, d.FromAddr, nullable(d.InReplyToID), d.To, d.Cc, d.Bcc, d.Subject, d.BodyMD, d.Cites, at)
		if err != nil {
			return fmt.Errorf("update draft: %w", err)
		}
		if err := logDraftChange(ctx, tx, d.ID); err != nil {
			return err
		}
		if err := insertAction(ctx, tx, draftAction(v, "draft.update", d, at)); err != nil {
			return err
		}
		out, err = draftByID(ctx, tx, v, d.ID, false)
		return err
	})
	if err != nil {
		return Draft{}, fmt.Errorf("update draft %s: %w", d.ID, err)
	}
	return out, nil
}

// HoldDraft records why a submission of draft id stopped at the draft
// (sendpolicy.Decision), as v at time at. Recorded as draft.hold.
func (db *DB) HoldDraft(ctx context.Context, v *auth.Viewer, id string, reason sendpolicy.HoldReason, detail string, at time.Time) error {
	if err := needViewer(v, "hold draft"); err != nil {
		return err
	}
	if reason == "" {
		return errors.New("hold draft: no reason")
	}
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		d, err := draftByID(ctx, tx, v, id, true)
		if err != nil {
			return err
		}
		if err := notInFlight(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE drafts SET hold_reason = $2, hold_detail = $3, updated_at = $4 WHERE id = $1`,
			id, string(reason), nullable(detail), at); err != nil {
			return err
		}
		if err := logDraftChange(ctx, tx, id); err != nil {
			return err
		}
		d.HoldReason = reason
		return insertAction(ctx, tx, draftAction(v, "draft.hold", d, at))
	})
	if err != nil {
		return fmt.Errorf("hold draft %s: %w", id, err)
	}
	return nil
}

// DeleteDraft deletes draft id as v at time at. A draft any outbox row
// came from is history and stays (ErrInFlight). Recorded as draft.delete.
func (db *DB) DeleteDraft(ctx context.Context, v *auth.Viewer, id string, at time.Time) error {
	if err := needViewer(v, "delete draft"); err != nil {
		return err
	}
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		d, err := draftByID(ctx, tx, v, id, true)
		if err != nil {
			return err
		}
		var used bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM outbox WHERE draft_id = $1)`, id).Scan(&used); err != nil {
			return err
		}
		if used {
			return ErrInFlight
		}
		if _, err := tx.Exec(ctx, `DELETE FROM drafts WHERE id = $1`, id); err != nil {
			return err
		}
		if err := logDraftChange(ctx, tx, id); err != nil {
			return err
		}
		return insertAction(ctx, tx, draftAction(v, "draft.delete", d, at))
	})
	if err != nil {
		return fmt.Errorf("delete draft %s: %w", id, err)
	}
	return nil
}

// notInFlight fails with ErrInFlight if an outbox row that was not
// withdrawn came from draft id.
func notInFlight(ctx context.Context, q querier, id string) error {
	var busy bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM outbox WHERE draft_id = $1 AND state <> 'withdrawn')`, id).Scan(&busy)
	if err != nil {
		return err
	}
	if busy {
		return ErrInFlight
	}
	return nil
}
