package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/classify"
	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
)

// What sendpolicy.Decide needs from the database (§8.9): the grant, the
// recent activity of the grant, who is in the replied-to thread.

// contactedStates are the outbox states in which a message went, or is
// going, to its recipients: an address in one of them has been
// contacted. held (M3) is still under review; withdrawn and dead never
// reached anyone.
const contactedStates = `('queued', 'sending', 'sent', 'partial', 'unknown', 'failed')`

// ReplyFacts describe a message being replied to.
type ReplyFacts struct {
	// MessageID is the message, aliases resolved; ThreadID its thread.
	MessageID string
	ThreadID  string
	// Participants are the folded addresses already in the thread, as
	// the viewer sees it: the senders of its visible messages, and whom
	// our own mail in it went to. The To and Cc of inbound mail are not
	// stored (only from_addr is), so someone only ever copied is not a
	// participant until they write; recipient_scope thread then holds a
	// reply to them as a draft, the safe side.
	Participants []string
	// Machine: classified machine, by the classifier or by a human
	// override. internal/classify counts Auto-Submitted (not "no") and
	// Precedence bulk|list among its signals, so this covers the RFC 3834
	// rules of §8.9 without the headers. An override to human does not
	// clear it: agents may classify (§8.2), so an override must not open
	// the loop rule.
	Machine bool
	// The message's own threading headers as parsed (ids without angle
	// brackets), for the In-Reply-To and References of a reply.
	HeaderID   string
	InReplyTo  string
	References []string
}

// ReplyFacts returns the facts of message id as v sees it; ErrNotFound
// if v cannot see it.
func (db *DB) ReplyFacts(ctx context.Context, v *auth.Viewer, id string) (ReplyFacts, error) {
	if err := needViewer(v, "reply facts"); err != nil {
		return ReplyFacts{}, err
	}
	return db.replyFacts(ctx, db.pool, v, id)
}

func (db *DB) replyFacts(ctx context.Context, q dbtx, v *auth.Viewer, id string) (ReplyFacts, error) {
	var f ReplyFacts
	err := q.QueryRow(ctx, `
		SELECT m.id, m.thread_id, m.class = $2 OR coalesce(s.class_override = $2, false),
		       coalesce(m.message_id, ''), coalesce(m.in_reply_to, ''), m.refs
		FROM messages m LEFT JOIN message_state s ON s.message_id = m.id
		WHERE m.id = coalesce((SELECT new_id FROM message_aliases WHERE old_id = $1), $1)
		  AND `+db.visibleMessage(v, "m"), id, int16(classify.Machine)).
		Scan(&f.MessageID, &f.ThreadID, &f.Machine, &f.HeaderID, &f.InReplyTo, &f.References)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, fmt.Errorf("message %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return f, fmt.Errorf("message %s: %w", id, err)
	}
	// Our mail in the thread is found two ways: its sent copy folded into
	// a message of the thread (outbox.raw_id -> content hash -> message),
	// and, while that copy is not parsed yet, the draft it came from
	// replying to a message of the thread.
	rows, err := q.Query(ctx, `
		SELECT m.from_addr FROM messages m
		WHERE m.thread_id = $1 AND m.from_addr <> '' AND `+db.visibleMessage(v, "m")+`
		UNION
		SELECT r.addr_fold FROM messages m
		CROSS JOIN LATERAL (SELECT so.id, so.state `+db.visibleOutbox(v, "m")+`) o
		JOIN outbox_recipients r ON r.outbox_id = o.id
		WHERE m.thread_id = $1 AND o.state IN `+contactedStates+` AND `+db.visibleMessage(v, "m")+`
		UNION
		SELECT r.addr_fold FROM outbox o
		JOIN identities oi ON oi.id = o.identity_id
		JOIN drafts dr ON dr.id = o.draft_id
		JOIN messages m ON m.id = dr.in_reply_to_id
		JOIN outbox_recipients r ON r.outbox_id = o.id
		WHERE m.thread_id = $1 AND o.state IN `+contactedStates+`
		  AND `+db.readableAccount(v, "oi.account_id")+` AND `+db.visibleMessage(v, "m")+`
		ORDER BY 1`, f.ThreadID)
	if err != nil {
		return f, fmt.Errorf("thread participants: %w", err)
	}
	f.Participants, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return f, fmt.Errorf("thread participants: %w", err)
	}
	for i, p := range f.Participants {
		f.Participants[i] = FoldAddr(p)
	}
	return f, nil
}

// SendCounters returns sendpolicy.Counters for a submission by principal
// as identity to recipients, replying into threadID ("" for a new
// thread), at now. Windows are rolling (the last hour, the last 24
// hours). They count outbox rows in every state: a submission spent its
// slot whatever became of it.
func (db *DB) SendCounters(ctx context.Context, identityID, principalID string, recipients []string, threadID string, now time.Time) (sendpolicy.Counters, error) {
	return sendCounters(ctx, db.pool, identityID, principalID, recipients, threadID, now)
}

func sendCounters(ctx context.Context, q dbtx, identityID, principalID string, recipients []string, threadID string, now time.Time) (sendpolicy.Counters, error) {
	var c sendpolicy.Counters
	folded := make([]string, 0, len(recipients))
	for _, r := range recipients {
		folded = append(folded, FoldAddr(r))
	}
	hour, day := now.Add(-time.Hour), now.Add(-24*time.Hour)

	err := q.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE created_at > $3), count(*)
		FROM outbox WHERE identity_id = $1 AND submitted_by = $2 AND created_at > $4`,
		identityID, principalID, hour, day).Scan(&c.SentLastHour, &c.SentLastDay)
	if err != nil {
		return c, fmt.Errorf("send counters: %w", err)
	}

	// Addresses this grant reached first: no earlier contacted row, by
	// anyone, had them.
	err = q.QueryRow(ctx, `
		SELECT count(DISTINCT r.addr_fold)
		FROM outbox o JOIN outbox_recipients r ON r.outbox_id = o.id
		WHERE o.identity_id = $1 AND o.submitted_by = $2 AND o.created_at > $3 AND o.state <> 'withdrawn'
		  AND NOT EXISTS (
			SELECT 1 FROM outbox_recipients r2 JOIN outbox o2 ON o2.id = r2.outbox_id
			WHERE r2.addr_fold = r.addr_fold AND o2.state IN `+contactedStates+`
			  AND (o2.created_at, o2.id) < (o.created_at, o.id))`,
		identityID, principalID, day).Scan(&c.NewRecipientsLastDay)
	if err != nil {
		return c, fmt.Errorf("new recipients: %w", err)
	}

	rows, err := q.Query(ctx, `
		SELECT DISTINCT f FROM unnest($1::text[]) f
		WHERE NOT EXISTS (
			SELECT 1 FROM outbox_recipients r JOIN outbox o ON o.id = r.outbox_id
			WHERE r.addr_fold = f AND o.state IN `+contactedStates+`)
		ORDER BY f`, folded)
	if err != nil {
		return c, fmt.Errorf("first contact: %w", err)
	}
	if c.FirstContact, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return c, fmt.Errorf("first contact: %w", err)
	}

	if threadID == "" {
		return c, nil
	}
	// Automatic mail (approved by system) of the last day in this thread,
	// per recipient, whoever sent it. Starting from the few recent outbox
	// rows keeps this on primary keys and the unique content_hash index.
	rows, err = q.Query(ctx, `
		SELECT r.addr_fold, count(DISTINCT o.id)::int
		FROM outbox o JOIN outbox_recipients r ON r.outbox_id = o.id
		WHERE o.approved_by = $1 AND o.created_at > $2 AND r.addr_fold = ANY($3)
		  AND (EXISTS (SELECT 1 FROM raw_objects ro JOIN messages m ON m.content_hash = ro.content_hash
		               WHERE ro.id = o.raw_id AND m.thread_id = $4)
		    OR EXISTS (SELECT 1 FROM drafts dr JOIN messages m ON m.id = dr.in_reply_to_id
		               WHERE dr.id = o.draft_id AND m.thread_id = $4))
		GROUP BY r.addr_fold`, SystemPrincipalID, day, folded, threadID)
	if err != nil {
		return c, fmt.Errorf("automatic mail in thread: %w", err)
	}
	c.AutoInThread = map[string]int{}
	var addr string
	var n int
	if _, err := pgx.ForEachRow(rows, []any{&addr, &n}, func() error {
		c.AutoInThread[addr] = n
		return nil
	}); err != nil {
		return c, fmt.Errorf("automatic mail in thread: %w", err)
	}
	return c, nil
}

// Submission is one submission to decide on.
type Submission struct {
	// FromAddr is the From address. The identity follows from it
	// (ResolveIdentity: exact beats *@zone, disabled skipped).
	FromAddr string
	// IdentityID, when set (a draft's), must still be what FromAddr
	// resolves to; if it is disabled or another identity now covers the
	// address, the input says the identity is disabled, so the mail stays
	// a draft instead of going out as an identity nobody chose.
	IdentityID string
	// Recipients are the envelope recipients of all roles.
	Recipients []string
	// InReplyToID is the message replied to, "" for a new thread.
	InReplyToID string
	// DraftAuthorID is set when the viewer confirms a held draft: the
	// draft's author is then the submitter (its grant is the one counted)
	// and the viewer the confirmer.
	DraftAuthorID string
	Now           time.Time
}

// PolicyFacts are sendpolicy's input for a submission, with what was
// resolved on the way.
type PolicyFacts struct {
	Input    sendpolicy.Input
	Identity Identity
	FromAddr string // as stored: lowercase
	Reply    *ReplyFacts
}

// PolicyInput gathers sendpolicy.Input for submission s by v. Everything
// sendpolicy.Decide reads comes from here, so no submission path builds
// it by hand. It is a read for previews and for the SMTP port's Sender
// check; what is queued is decided by WithDecision, which gathers the
// same input again under the grant's lock.
//
// It fails with ErrNoIdentity when nothing covers s.FromAddr and no
// IdentityID is given; the SMTP port rejects such mail (§13.6), the other
// paths only accept From addresses of identities.
func (db *DB) PolicyInput(ctx context.Context, v *auth.Viewer, s Submission) (PolicyFacts, error) {
	if err := needViewer(v, "policy input"); err != nil {
		return PolicyFacts{}, err
	}
	return db.policyInput(ctx, db.pool, v, s)
}

func (db *DB) policyInput(ctx context.Context, q dbtx, v *auth.Viewer, s Submission) (PolicyFacts, error) {
	var f PolicyFacts
	ident, from, err := resolveIdentity(ctx, q, s.FromAddr)
	disabled := false
	switch {
	case s.IdentityID == "" && err != nil:
		return f, err
	case s.IdentityID != "" && (err != nil || ident.ID != s.IdentityID):
		if !errors.Is(err, ErrNoIdentity) && err != nil {
			return f, err
		}
		if ident, err = identityByID(ctx, q, s.IdentityID); err != nil {
			return f, err
		}
		from, disabled = strings.ToLower(s.FromAddr), true
	}
	f.Identity, f.FromAddr = ident, from

	in := &f.Input
	in.Identity = sendpolicy.Identity{
		ID: ident.ID, OwnerID: ident.OwnerID, OwnerKind: ident.OwnerKind,
		Disabled: disabled || ident.DisabledAt != nil,
	}
	in.CanSubmit = v.Has(auth.SubmissionSend)
	in.Submitter = sendpolicy.Principal{ID: v.PrincipalID, Kind: v.PrincipalKind}
	if s.DraftAuthorID != "" {
		author, err := scanPrincipal(q.QueryRow(ctx, `SELECT `+principalCols+` FROM principals WHERE id = $1`, s.DraftAuthorID))
		if err != nil {
			return f, fmt.Errorf("draft author %s: %w", s.DraftAuthorID, err)
		}
		in.ConfirmedBy = &sendpolicy.Principal{ID: v.PrincipalID, Kind: v.PrincipalKind}
		in.Submitter = sendpolicy.Principal{ID: author.ID, Kind: author.Kind}
	}

	g, err := grantFor(ctx, q, ident.ID, in.Submitter.ID)
	switch {
	case err == nil:
		pg := g.ForPolicy()
		in.Grant = &pg
	case !errors.Is(err, ErrNotFound):
		return f, err
	}

	in.Recipients = s.Recipients
	threadID := ""
	if s.InReplyToID != "" {
		r, err := db.replyFacts(ctx, q, v, s.InReplyToID)
		if err != nil {
			return f, err
		}
		f.Reply, threadID = &r, r.ThreadID
		in.Reply = &sendpolicy.Reply{Participants: r.Participants, Machine: r.Machine}
	}
	in.Counters, err = sendCounters(ctx, q, ident.ID, in.Submitter.ID, s.Recipients, threadID, s.Now)
	if err != nil {
		return f, err
	}
	return f, nil
}

// MessageByHeaderID returns the newest message v can see whose
// Message-ID header is headerID (without angle brackets): the message a
// raw submission's In-Reply-To names. ErrNotFound if there is none.
func (db *DB) MessageByHeaderID(ctx context.Context, v *auth.Viewer, headerID string) (string, error) {
	if err := needViewer(v, "message by header id"); err != nil {
		return "", err
	}
	var id string
	err := db.pool.QueryRow(ctx, `SELECT m.id FROM messages m
		WHERE m.message_id = $1 AND `+db.visibleMessage(v, "m")+`
		ORDER BY m.received_at DESC, m.id DESC LIMIT 1`, headerID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("message-id %s: %w", headerID, ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("message-id %s: %w", headerID, err)
	}
	return id, nil
}

// Standing is where submissions by a viewer as one identity stand before
// any particular message: the grant that applies, the tier after the
// owner cap, and the hold that every submission meets whatever its
// recipients. It is what a composer shows next to the From choice ("will
// be sent" or "will be held"); the decision itself stays Decide's, on the
// submission, with its recipients, counters and thread.
type Standing struct {
	Identity Identity
	// Grant is the one that applies (sendpolicy.Effective): the row, or
	// the default when Granted is false, with NULL limits filled in.
	Grant   sendpolicy.Grant
	Granted bool
	// Tier is the effective tier: Grant.Policy lowered by the owner cap.
	Tier   sendpolicy.Tier
	Capped bool
	// Hold is the reason every submission as this identity stops at a
	// draft, "" when one can go out within the grant's recipient scope
	// and limits. HoldDetail says the rest for people.
	Hold       sendpolicy.HoldReason
	HoldDetail string
}

// Standings returns every enabled identity, by address, with where v's
// submissions as it stand; those v may not use have Tier none. v's
// credential counts: without submission:send everything is held (§8.9).
func (db *DB) Standings(ctx context.Context, v *auth.Viewer) ([]Standing, error) {
	if err := needViewer(v, "standings"); err != nil {
		return nil, err
	}
	ids, err := db.Identities(ctx, false)
	if err != nil {
		return nil, err
	}
	grants, err := db.Grants(ctx, "", v.PrincipalID)
	if err != nil {
		return nil, err
	}
	byIdentity := map[string]sendpolicy.Grant{}
	for _, g := range grants {
		byIdentity[g.IdentityID] = g.ForPolicy()
	}
	out := []Standing{}
	for _, i := range ids {
		in := sendpolicy.Input{
			Submitter: sendpolicy.Principal{ID: v.PrincipalID, Kind: v.PrincipalKind},
			CanSubmit: v.Has(auth.SubmissionSend),
			Identity:  sendpolicy.Identity{ID: i.ID, OwnerID: i.OwnerID, OwnerKind: i.OwnerKind},
		}
		s := Standing{Identity: i}
		if g, ok := byIdentity[i.ID]; ok {
			in.Grant, s.Granted = &g, true
		}
		s.Grant = sendpolicy.Effective(in)
		// Without recipients Decide runs its rules about who submits as
		// what (1–6) and stops at the recipients rule (7) if none of them
		// held it. Of the rules after that, only the review tier holds
		// every submission alike.
		d := sendpolicy.Decide(in)
		s.Tier, s.Capped = d.Tier, sendpolicy.OwnerCapped(v.PrincipalID, in.Identity)
		switch {
		case d.HoldReason != sendpolicy.HoldRecipients:
			s.Hold, s.HoldDetail = d.HoldReason, d.Detail
		case s.Tier == sendpolicy.Review:
			s.Hold, s.HoldDetail = sendpolicy.HoldReview,
				"the grant's tier is review, and the automatic review is not available yet; a human confirms instead"
		}
		out = append(out, s)
	}
	return out, nil
}
