package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/mail"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/contenthash"
	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// The one way into the outbox (§8.9, AGENTS.md 外发按身份授权).
//
// An outbox row is created by (*PolicyTx).Enqueue and nothing else. A
// PolicyTx is made only by WithDecision, which gathers the facts and
// calls sendpolicy.Decide itself, inside the transaction that holds the
// grant's lock; the caller never hands in a decision, so there is none
// to forge or to reuse, and the counters it was made on are the ones
// locked until the row commits. Enqueue refuses unless that decision is
// Queue, takes the identity, From, submitter and approver from it, and
// checks that the bytes and recipients are the ones decided on. One
// PolicyTx records one outcome. TestOneWayToTheOutbox keeps it that way
// at the source level: no other statement inserts an outbox row, and
// nothing else builds a PolicyTx.

// ErrNotApproved: Enqueue on a PolicyTx whose decision is not Queue, or
// with something other than what was decided.
var ErrNotApproved = errors.New("not approved by the send policy")

// PolicyTx is one submission inside its decision's transaction. Its
// fields are unexported: what was decided cannot be changed by its user.
type PolicyTx struct {
	db       *DB
	tx       pgx.Tx
	v        *auth.Viewer
	sub      Submission
	facts    PolicyFacts
	decision sendpolicy.Decision
	done     bool
	after    []func(context.Context)
}

// WithDecision decides submission s by v and runs f with the result, in
// one transaction that holds the grant's lock (lockGrant). The facts are
// read once to learn whose grant it is, then again under the lock, and
// sendpolicy.Decide runs on the second reading; so two concurrent
// submissions of one grant cannot both pass a limit with one slot left.
// f records the outcome through the PolicyTx: Enqueue, CreateDraft or
// HoldDraft, and Audit. If f fails nothing is committed. Functions
// passed to AfterCommit run after the commit, in order.
//
// Writing to object storage inside f is fine (the sent copy goes there
// before its row): it holds the grant's lock, which only this grant's
// submissions wait for, and not the content lock, which Enqueue and the
// draft writes take last.
func (db *DB) WithDecision(ctx context.Context, v *auth.Viewer, s Submission, f func(ctx context.Context, p *PolicyTx) error) error {
	if err := needViewer(v, "decide"); err != nil {
		return err
	}
	var p *PolicyTx
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		facts, err := db.policyInput(ctx, tx, v, s)
		if err != nil {
			return err
		}
		if err := lockGrant(ctx, tx, facts.Identity.ID, facts.Input.Submitter.ID); err != nil {
			return err
		}
		if facts, err = db.policyInput(ctx, tx, v, s); err != nil {
			return err
		}
		p = &PolicyTx{db: db, tx: tx, v: v, sub: s, facts: facts, decision: sendpolicy.Decide(facts.Input)}
		return f(ctx, p)
	})
	if err != nil {
		return err
	}
	for _, fn := range p.after {
		fn(ctx)
	}
	return nil
}

// Facts returns what the decision was made on.
func (p *PolicyTx) Facts() PolicyFacts { return p.facts }

// Decision returns the decision. It is a copy: changing it changes
// nothing that Enqueue reads.
func (p *PolicyTx) Decision() sendpolicy.Decision { return p.decision }

// AfterCommit runs fn once the transaction has committed, such as waking
// the executor or indexing the sent copy. It does not run if the
// transaction fails.
func (p *PolicyTx) AfterCommit(fn func(context.Context)) { p.after = append(p.after, fn) }

// outcome claims the one outcome of p.
func (p *PolicyTx) outcome(op string) error {
	switch {
	case p == nil || p.tx == nil:
		return fmt.Errorf("%s: %w: not a PolicyTx from WithDecision", op, ErrNotApproved)
	case p.done:
		return fmt.Errorf("%s: %w: this decision already has its outcome", op, ErrNotApproved)
	}
	p.done = true
	return nil
}

// Enqueue records o as a queued outbox row (§4.5, §4.6), approved by the
// decision. It refuses (ErrNotApproved) unless the decision is Queue,
// and unless o is what was decided on: the same folded set of
// recipients, a header From that is the decided From address, bytes that
// are the sent copy described. Identity, From, submitted_by and
// approved_by come from the decision. With o.DraftID the draft is locked
// and must not be in the outbox already (ErrInFlight), so a draft is
// queued once however often send is pressed.
func (p *PolicyTx) Enqueue(ctx context.Context, o NewOutbox) error {
	if err := p.outcome("enqueue"); err != nil {
		return err
	}
	d, f := p.decision, p.facts
	if !d.Queue {
		return fmt.Errorf("enqueue: %w: the decision is a draft (%s: %s)", ErrNotApproved, d.HoldReason, d.Detail)
	}
	if err := sameRecipients(p.sub.Recipients, o.Recipients); err != nil {
		return fmt.Errorf("enqueue: %w", err)
	}
	if err := checkFrom(o.Message, f.FromAddr); err != nil {
		return fmt.Errorf("enqueue: %w", err)
	}
	row := outboxRow{
		NewOutbox: o, identityID: f.Identity.ID, fromAddr: f.FromAddr,
		submittedBy: f.Input.Submitter.ID, approvedBy: d.ApprovedBy, newRaw: true,
	}
	if o.DraftID != "" {
		dr, err := draftByID(ctx, p.tx, p.v, o.DraftID, true)
		if err != nil {
			return fmt.Errorf("enqueue: %w", err)
		}
		if err := notInFlight(ctx, p.tx, o.DraftID); err != nil {
			return fmt.Errorf("enqueue: draft %s: %w", o.DraftID, err)
		}
		if dr.IdentityID != f.Identity.ID || dr.FromAddr != f.FromAddr || dr.PrincipalID != f.Input.Submitter.ID {
			return fmt.Errorf("enqueue: %w: draft %s is not the submission decided on", ErrNotApproved, o.DraftID)
		}
		row.newRaw = dr.RawID == "" || dr.RawID != o.Raw.ID
	}
	if row.newRaw {
		if err := o.Raw.describes(o.Message); err != nil {
			return fmt.Errorf("enqueue: %w", err)
		}
	} else if err := storedRawIs(ctx, p.tx, o.Raw.ID, o.Message); err != nil {
		return fmt.Errorf("enqueue: %w", err)
	}
	return enqueueOutbox(ctx, p.tx, row)
}

// CreateDraft records a submission that stopped at a draft as a new
// draft by the submitter, with the decision's hold reason; raw, when not
// nil, is its body as normalized bytes already stored under sent/ (an
// SMTP submission, §13.6), recorded here. It refuses when the decision is
// Queue, when the submission confirms a draft (that one is held with
// HoldDraft), and when d is not what was decided on.
func (p *PolicyTx) CreateDraft(ctx context.Context, d Draft, raw *SentRaw, message []byte, at time.Time) (Draft, error) {
	if err := p.outcome("create draft"); err != nil {
		return Draft{}, err
	}
	dec, f := p.decision, p.facts
	switch {
	case dec.Queue:
		return Draft{}, errors.New("create draft: the decision is to queue")
	case p.sub.DraftAuthorID != "":
		return Draft{}, errors.New("create draft: a confirmation holds its draft")
	case FoldAddr(d.FromAddr) != f.FromAddr || d.InReplyToID != p.sub.InReplyToID:
		return Draft{}, fmt.Errorf("create draft: from %s in reply to %q is not what was decided", d.FromAddr, d.InReplyToID)
	}
	if err := sameRecipients(p.sub.Recipients, DraftRecipients(d)); err != nil {
		return Draft{}, fmt.Errorf("create draft: %w", err)
	}
	d.HoldReason, d.HoldDetail = dec.HoldReason, dec.Detail
	if raw != nil {
		if err := raw.describes(message); err != nil {
			return Draft{}, fmt.Errorf("create draft: %w", err)
		}
		if _, err := p.tx.Exec(ctx, `
			INSERT INTO raw_objects (id, r2_key, local_path, size, sha256, received_at, completeness, content_hash)
			VALUES ($1, $2, $2, $3, $4, $5, 1, $6)`,
			raw.ID, raw.Key, raw.Size, raw.SHA256[:], raw.At, raw.ContentHash[:]); err != nil {
			return Draft{}, fmt.Errorf("create draft: insert raw_objects: %w", err)
		}
		d.RawID, d.BodyMD = raw.ID, ""
	}
	out, err := p.db.createDraft(ctx, p.tx, p.v, d, at)
	if err != nil {
		return Draft{}, fmt.Errorf("create draft: %w", err)
	}
	if out.IdentityID != f.Identity.ID {
		return Draft{}, fmt.Errorf("create draft: %s now resolves to another identity", d.FromAddr)
	}
	return out, nil
}

// HoldDraft records the decision's hold reason on draft id, the draft
// submitted. It refuses when the decision is Queue.
func (p *PolicyTx) HoldDraft(ctx context.Context, id string, at time.Time) error {
	if err := p.outcome("hold draft"); err != nil {
		return err
	}
	if p.decision.Queue {
		return errors.New("hold draft: the decision is to queue")
	}
	if err := holdDraft(ctx, p.tx, p.v, id, p.decision.HoldReason, p.decision.Detail, at); err != nil {
		return fmt.Errorf("hold draft %s: %w", id, err)
	}
	return nil
}

// LockDraft returns draft id as the viewer sees it, locked until the
// transaction ends.
func (p *PolicyTx) LockDraft(ctx context.Context, id string) (Draft, error) {
	if p == nil || p.tx == nil {
		return Draft{}, errors.New("lock draft: not a PolicyTx from WithDecision")
	}
	return draftByID(ctx, p.tx, p.v, id, true)
}

// Audit appends the submission's entry to the audit log as the viewer:
// args as given (addressing, never a body), the result with the decision
// added under "decision".
func (p *PolicyTx) Audit(ctx context.Context, tool string, args, result map[string]any, at time.Time) error {
	if p == nil || p.tx == nil {
		return errors.New("audit: not a PolicyTx from WithDecision")
	}
	if result == nil {
		result = map[string]any{}
	}
	result["decision"] = DecisionRecord(p.decision)
	return insertAction(ctx, p.tx, Action{
		ID: ulid.New(at), Actor: p.v.PrincipalID, CredentialID: p.v.CredentialID,
		Tool: tool, Args: args, Result: result, At: at,
	})
}

// DecisionRecord is how a decision appears in the audit log.
func DecisionRecord(d sendpolicy.Decision) map[string]any {
	r := map[string]any{"queue": d.Queue, "tier": d.Tier}
	if d.Queue {
		r["approved_by"] = d.ApprovedBy
	} else {
		r["hold_reason"], r["detail"] = d.HoldReason, d.Detail
	}
	if d.AutoSubmitted != "" {
		r["auto_submitted"] = d.AutoSubmitted
	}
	return r
}

// NewSentRaw describes message as a sent copy stored under key at time
// at, with id.
func NewSentRaw(id, key string, message []byte, at time.Time) SentRaw {
	return SentRaw{
		ID: id, Key: key, Size: int64(len(message)),
		SHA256: sha256.Sum256(message), ContentHash: contenthash.V1(message), At: at,
	}
}

// describes checks that r is the row of message.
func (r SentRaw) describes(message []byte) error {
	if want := NewSentRaw(r.ID, r.Key, message, r.At); r != want {
		return fmt.Errorf("%w: sent copy %s does not describe the message", ErrNotApproved, r.ID)
	}
	return nil
}

// storedRawIs checks that raw id holds message.
func storedRawIs(ctx context.Context, q querier, id string, message []byte) error {
	var size int64
	var sum []byte
	if err := q.QueryRow(ctx, `SELECT size, sha256 FROM raw_objects WHERE id = $1`, id).Scan(&size, &sum); err != nil {
		return fmt.Errorf("raw %s: %w", id, err)
	}
	if got := sha256.Sum256(message); size != int64(len(message)) || !bytes.Equal(sum, got[:]) {
		return fmt.Errorf("%w: raw %s does not hold the message", ErrNotApproved, id)
	}
	return nil
}

// checkFrom checks that message has exactly one header From, of addr.
func checkFrom(message []byte, addr string) error {
	m, err := mail.ReadMessage(bytes.NewReader(message))
	if err != nil {
		return fmt.Errorf("%w: header: %w", ErrNotApproved, err)
	}
	if len(m.Header["From"]) != 1 {
		return fmt.Errorf("%w: %d From fields", ErrNotApproved, len(m.Header["From"]))
	}
	list, err := m.Header.AddressList("From")
	if err != nil || len(list) != 1 || FoldAddr(list[0].Address) != addr {
		return fmt.Errorf("%w: header From is not %s", ErrNotApproved, addr)
	}
	return nil
}

// sameRecipients checks that got are the decided recipients, compared
// as folded sets.
func sameRecipients(decided []string, got []OutboxRecipient) error {
	fold := func(addrs []string) []string {
		out := make([]string, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, FoldAddr(a))
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	addrs := make([]string, 0, len(got))
	for _, r := range got {
		addrs = append(addrs, r.Addr)
	}
	if !slices.Equal(fold(decided), fold(addrs)) {
		return fmt.Errorf("%w: the recipients are not the ones decided on", ErrNotApproved)
	}
	return nil
}

// DraftRecipients returns the envelope recipients of d by kind.
func DraftRecipients(d Draft) []OutboxRecipient {
	var out []OutboxRecipient
	for _, l := range []struct {
		kind  string
		addrs []string
	}{{RecipientTo, d.To}, {RecipientCc, d.Cc}, {RecipientBcc, d.Bcc}} {
		for _, a := range l.addrs {
			out = append(out, OutboxRecipient{Addr: a, Kind: l.kind})
		}
	}
	return out
}
