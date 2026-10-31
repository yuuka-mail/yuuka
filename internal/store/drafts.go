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

var (
	// ErrInFlight: the draft has gone to the outbox (or has outbox
	// history), so it can no longer be edited or deleted.
	ErrInFlight = errors.New("the draft is in the outbox")
	// ErrInvalidDraft: the content of a draft write cannot be stored: a
	// malformed address or cites, a reply to a message the writer cannot
	// see. The caller sent it, so it is theirs to fix.
	ErrInvalidDraft = errors.New("invalid draft")
	// ErrRawDraft: an SMTP submission held as a draft is MIME a service
	// sent, not Markdown; it is confirmed or deleted, never edited.
	ErrRawDraft = errors.New("an SMTP submission held as a draft cannot be edited")
)

// Draft is a row of drafts (§4, §8.9).
type Draft struct {
	ID string
	// PrincipalID wrote it; its grant applies when it is submitted.
	PrincipalID string
	// AuthorKind and AuthorName are the writer's; read only.
	AuthorKind, AuthorName string
	// IdentityID always follows from FromAddr (ResolveIdentity): the
	// writes resolve it, so a draft cannot pair an address with an
	// identity that does not cover it.
	IdentityID string
	// IdentityOwner owns that identity (ID, Kind and Name); read only.
	// Whoever sees the draft may see whose address it would go out as,
	// which decides who may confirm it (§8.9 owner cap).
	IdentityOwner Principal
	FromAddr      string
	InReplyToID   string // "" for a new thread
	To, Cc, Bcc   []string
	Subject       string
	// Exactly one body: BodyMD for composed mail, RawID for an SMTP
	// submission held as a draft (already MIME, stored under sent/).
	BodyMD string
	RawID  string
	// Cites is a JSON array of {uri, quote}; nil writes [].
	Cites      json.RawMessage
	HoldReason sendpolicy.HoldReason // "" for a draft never submitted
	HoldDetail string
	// Review is the verdict (sendpolicy.Verdict as JSON) of the review
	// that escalated it, nil unless HoldReason is review; read only, the
	// decision writes it.
	Review    json.RawMessage
	CreatedAt time.Time
	UpdatedAt time.Time

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
	if seesEveryDraft(v) {
		return "$1::text IS NOT NULL" // $1 must appear for its type
	}
	return "dr.principal_id = $1"
}

// seesEveryDraft: v sees all drafts. It alone sees what is left of a
// deleted one (its change, §8.6): who wrote it is gone with the row.
func seesEveryDraft(v *auth.Viewer) bool { return v.PrincipalKind == sendpolicy.KindHuman }

const draftCols = `dr.id, dr.principal_id, ap.kind, ap.name, dr.identity_id, dr.from_addr, coalesce(dr.in_reply_to_id, ''),
	dr.to_addrs, dr.cc_addrs, dr.bcc_addrs, dr.subject, coalesce(dr.body_md, ''), coalesce(dr.raw_id, ''),
	dr.cites, coalesce(dr.hold_reason, ''), coalesce(dr.hold_detail, ''), dr.review, dr.created_at, dr.updated_at,
	coalesce(o.id, ''), coalesce(o.state, ''), op.id, op.kind, op.name`

// draftFrom joins the identity's owner and the newest outbox row that
// was not withdrawn.
const draftFrom = ` FROM drafts dr
	JOIN principals ap ON ap.id = dr.principal_id
	JOIN identities di ON di.id = dr.identity_id
	JOIN principals op ON op.id = di.owner_id
	LEFT JOIN LATERAL (
		SELECT id, state FROM outbox WHERE draft_id = dr.id AND state <> 'withdrawn'
		ORDER BY created_at DESC, id DESC LIMIT 1) o ON true`

func scanDraft(row pgx.Row) (Draft, error) {
	var d Draft
	var reason string
	err := row.Scan(&d.ID, &d.PrincipalID, &d.AuthorKind, &d.AuthorName, &d.IdentityID, &d.FromAddr, &d.InReplyToID,
		&d.To, &d.Cc, &d.Bcc, &d.Subject, &d.BodyMD, &d.RawID,
		&d.Cites, &reason, &d.HoldDetail, &d.Review, &d.CreatedAt, &d.UpdatedAt, &d.OutboxID, &d.OutboxState,
		&d.IdentityOwner.ID, &d.IdentityOwner.Kind, &d.IdentityOwner.Name)
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

// DraftQuery selects drafts. Open, Held and InFlight pick drafts by
// where they stand; with none set it is Open and Held, the drafts still
// to be worked on.
type DraftQuery struct {
	// Open: never submitted, or edited since the last submission.
	Open bool
	// Held: the last submission stopped at the draft (HoldReason).
	Held bool
	// InFlight: went to the outbox (OutboxID).
	InFlight bool
	// Before returns drafts strictly after this position in the order
	// (updated_at, id), both descending; nil starts at the most recent.
	// An edit moves a draft to the top, past a cursor taken before it.
	Before *DraftCursor
	Limit  int // default 100, at most 500
}

// DraftCursor is a position in the draft list.
type DraftCursor struct {
	UpdatedAt time.Time
	ID        string
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
	if !q.Open && !q.Held && !q.InFlight {
		q.Open, q.Held = true, true
	}
	args := []any{v.PrincipalID, q.Open, q.Held, q.InFlight, limit}
	page := ""
	if q.Before != nil {
		args = append(args, q.Before.UpdatedAt, q.Before.ID)
		page = ` AND (dr.updated_at, dr.id) < ($6, $7)`
	}
	rows, err := db.pool.Query(ctx, `SELECT `+draftCols+draftFrom+`
		WHERE `+visibleDraft(v)+page+` AND CASE
			WHEN o.id IS NOT NULL THEN $4::bool
			WHEN dr.hold_reason IS NOT NULL THEN $3::bool
			ELSE $2::bool END
		ORDER BY dr.updated_at DESC, dr.id DESC LIMIT $5`, args...)
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
			return fmt.Errorf("%w: %w", ErrInvalidDraft, err)
		}
	}
	if d.Cites == nil {
		d.Cites = json.RawMessage("[]")
	}
	var cites []json.RawMessage
	if err := json.Unmarshal(d.Cites, &cites); err != nil {
		return fmt.Errorf("%w: cites: want a JSON array: %w", ErrInvalidDraft, err)
	}
	if _, _, err := splitSendAddress(d.FromAddr); err != nil {
		return fmt.Errorf("%w: from: %w", ErrInvalidDraft, err)
	}
	ident, from, err := resolveIdentity(ctx, tx, d.FromAddr)
	if err != nil {
		return err
	}
	if d.IdentityID != "" && d.IdentityID != ident.ID {
		return fmt.Errorf("%w: from %s goes out as identity %s (%s), not %s", ErrInvalidDraft, from, ident.ID, ident.Address(), d.IdentityID)
	}
	d.IdentityID, d.FromAddr = ident.ID, from
	d.IdentityOwner = Principal{ID: ident.OwnerID, Kind: ident.OwnerKind, Name: ident.OwnerName}
	if d.InReplyToID != "" {
		err := tx.QueryRow(ctx, `SELECT m.id FROM messages m
			WHERE m.id = coalesce((SELECT new_id FROM message_aliases WHERE old_id = $1), $1)
			  AND `+db.visibleMessage(v, "m"), d.InReplyToID).Scan(&d.InReplyToID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: in reply to %s: no such message", ErrInvalidDraft, d.InReplyToID)
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

// logDraftChange appends a 'draft' change; flush takes the content lock
// that keeps changes.seq in commit order.
func logDraftChange(ctx context.Context, tx pgx.Tx, id string) error {
	w := writer{tx: tx}
	w.change("draft", id)
	return w.flush(ctx)
}

// CreateDraft stores a new draft written by v at time at and returns it.
// d.ID, PrincipalID and the timestamps are set here; d.IdentityID
// follows from d.FromAddr (ErrNoIdentity if nothing covers it). It takes
// a Markdown body only: a raw body and a hold reason come from a
// submission that stopped at a draft, which (*PolicyTx).CreateDraft
// records with its decision. Recorded as draft.create.
func (db *DB) CreateDraft(ctx context.Context, v *auth.Viewer, d Draft, at time.Time) (Draft, error) {
	if err := needViewer(v, "create draft"); err != nil {
		return Draft{}, err
	}
	if d.RawID != "" || d.HoldReason != "" || d.HoldDetail != "" || d.Review != nil {
		return Draft{}, fmt.Errorf("create draft: %w: a raw body and a hold reason come from a submission's decision", ErrInvalidDraft)
	}
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		var err error
		d, err = db.createDraft(ctx, tx, v, d, at)
		return err
	})
	if err != nil {
		return Draft{}, fmt.Errorf("create draft: %w", err)
	}
	return d, nil
}

func (db *DB) createDraft(ctx context.Context, tx pgx.Tx, v *auth.Viewer, d Draft, at time.Time) (Draft, error) {
	if d.RawID != "" && d.BodyMD != "" {
		return Draft{}, errors.New("a body or a raw, not both")
	}
	d.ID, d.PrincipalID, d.CreatedAt, d.UpdatedAt = ulid.New(at), v.PrincipalID, at, at
	d.AuthorKind, d.AuthorName = v.PrincipalKind, v.PrincipalName
	if err := db.prepareDraft(ctx, tx, v, &d); err != nil {
		return Draft{}, err
	}
	var body any = d.BodyMD
	if d.RawID != "" {
		body = nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO drafts (id, principal_id, identity_id, from_addr, in_reply_to_id, to_addrs, cc_addrs, bcc_addrs,
		                    subject, body_md, raw_id, cites, hold_reason, hold_detail, review, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $16)`,
		d.ID, d.PrincipalID, d.IdentityID, d.FromAddr, nullable(d.InReplyToID), d.To, d.Cc, d.Bcc,
		d.Subject, body, nullable(d.RawID), d.Cites, nullable(string(d.HoldReason)), nullable(d.HoldDetail), d.Review, at)
	if err != nil {
		return Draft{}, fmt.Errorf("insert draft: %w", err)
	}
	if err := logDraftChange(ctx, tx, d.ID); err != nil {
		return Draft{}, err
	}
	return d, insertAction(ctx, tx, draftAction(v, "draft.create", d, at))
}

// UpdateDraft replaces the content of draft d.ID (From, reply target,
// recipients, subject, body, cites) as v at time at. Only Markdown drafts
// are edited; an SMTP submission's raw is what its service sent. Editing
// clears the hold reason and the review verdict: they described a
// submission of other content, and submitting again decides afresh. ErrInFlight once the draft has
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
			return ErrRawDraft
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
				review = NULL, updated_at = $11
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

// holdDraft records why a submission of draft id stopped at the draft,
// and the review verdict when one escalated it (nil clears it), as v at
// time at; (*PolicyTx).HoldDraft passes its decision. Recorded as
// draft.hold.
func holdDraft(ctx context.Context, tx pgx.Tx, v *auth.Viewer, id string, reason sendpolicy.HoldReason, detail string, review []byte, at time.Time) error {
	if reason == "" {
		return errors.New("hold draft: no reason")
	}
	d, err := draftByID(ctx, tx, v, id, true)
	if err != nil {
		return err
	}
	if err := notInFlight(ctx, tx, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE drafts SET hold_reason = $2, hold_detail = $3, review = $4, updated_at = $5 WHERE id = $1`,
		id, string(reason), nullable(detail), review, at); err != nil {
		return err
	}
	if err := logDraftChange(ctx, tx, id); err != nil {
		return err
	}
	d.HoldReason = reason
	return insertAction(ctx, tx, draftAction(v, "draft.hold", d, at))
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
