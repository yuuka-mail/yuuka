package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// The outbox side of §4.5: enqueueing a message (only through a
// PolicyTx, see policytx.go), claiming it for an attempt, the commit
// point, recording the result, lease recovery and the human resolution
// of unknown. Every transaction that writes a change
// takes contentLock first, like the content writers, so changes.seq stays
// in commit order and the lock order is content lock, then rows (the
// order with grants and drafts, and why it is safe, is in held.go).
// The two statements that run during an attempt (CommitAttempt and
// RenewLease) write no change and take no lock beyond their row.
//
// Times that the state machine compares (next_retry_at, lease_until) are
// the database's now(), never the caller's clock, so executors on several
// hosts agree on whose lease has run out.

// Outbox states (outbox.state).
const (
	OutboxHeld      = "held"
	OutboxQueued    = "queued"
	OutboxSending   = "sending"
	OutboxSent      = "sent"
	OutboxPartial   = "partial"
	OutboxUnknown   = "unknown"
	OutboxFailed    = "failed"
	OutboxDead      = "dead"
	OutboxWithdrawn = "withdrawn"
)

// Recipient kinds (outbox_recipients.kind).
const (
	RecipientTo  = "to"
	RecipientCc  = "cc"
	RecipientBcc = "bcc"
)

// Recipient states (outbox_recipients.state) the executor writes.
const (
	RecipientPending  = "pending"
	RecipientAccepted = "accepted"
	RecipientRejected = "rejected"
	RecipientDeferred = "deferred"
	RecipientUnknown  = "unknown"
)

// Attempt outcomes (send_attempts.outcome).
const (
	AttemptAccepted = "accepted"
	AttemptPartial  = "partial"
	AttemptRejected = "rejected"
	AttemptFailed   = "failed"
	AttemptUnknown  = "unknown"
	// AttemptAbandoned: ended before the commit point for a reason that is
	// not about the message (the executor was lost or shut down, the exit
	// was unusable, the sent copy could not be read). The remote provably
	// did not get the body, and it does not count toward the retry cap.
	AttemptAbandoned = "abandoned"
)

// Resolutions of an unknown outbox row (§4.5).
const (
	ResolveSent    = "sent"    // a human confirmed it was delivered
	ResolveResend  = "resend"  // accept a possible duplicate and send again
	ResolveAbandon = "abandon" // give up
)

var (
	// ErrLeaseLost: the attempt is no longer this executor's to commit,
	// because its lease ran out or recovery finished it.
	ErrLeaseLost = errors.New("send attempt lease lost")
	// ErrOutboxState: the outbox row is not in the state the operation
	// needs.
	ErrOutboxState = errors.New("outbox row is in another state")
)

// FoldAddr is outbox_recipients.addr_fold: Go's lowercasing, the folding
// ingest applies to prefixes (§4.6). Every lookup by folded address (first
// contact, new-recipient limits) must use it too.
func FoldAddr(addr string) string { return strings.ToLower(addr) }

// SentRaw is the raw_objects row of a sent copy (§4.6). Its bytes are
// already stored under Key, in R2 and locally.
type SentRaw struct {
	ID          string
	Key         string // sent/YYYY/MM/<ID>.eml, also the local path
	Size        int64
	SHA256      [32]byte
	ContentHash [32]byte
	At          time.Time
}

// OutboxRecipient is one envelope recipient as submitted.
type OutboxRecipient struct {
	Addr string // verbatim, as it goes into RCPT TO
	Kind string // RecipientTo, RecipientCc or RecipientBcc
}

// NewOutbox is what (*PolicyTx).Enqueue writes besides what the
// decision fixes. The identity, the From address, the submitter and the
// approver are not in it: they come from the PolicyTx, so a caller
// cannot queue mail as anyone the decision was not about.
type NewOutbox struct {
	ID      string
	DraftID string // "" for none
	// Raw is the sent copy, already stored under Raw.Key. It is recorded
	// here, unless it is the raw of DraftID (a held SMTP submission sent
	// as it was stored), which already has its row.
	Raw SentRaw
	// Message is the bytes stored under Raw.Key. Enqueue checks them
	// against Raw and against the decision (header From).
	Message []byte
	// Recipients in the order given. Spellings that fold to the same
	// address keep the first (§4.6). Their folded set must be the one
	// decided on.
	Recipients []OutboxRecipient
	// HeldDraft, for a held SMTP submission (no DraftID), is the draft
	// to create backed by Raw: where the row goes back to if withdrawn.
	// Enqueue sets its ID to the draft created.
	HeldDraft *Draft
}

// outboxRow is one outbox row with its sent copy and recipients.
type outboxRow struct {
	NewOutbox
	identityID  string
	fromAddr    string
	submittedBy string
	approvedBy  string
	// newRaw: Raw has no raw_objects row yet.
	newRaw bool
	// held: the review passed; the row waits holdSeconds in held with
	// its verdict.
	held        bool
	holdSeconds int
	review      []byte
}

// enqueueOutbox records a sent copy and the outbox row that will send it,
// in state queued and due now, or held until the end of its window by
// the database's clock, in tx (§4.6). The bytes must already be stored:
// if the transaction fails they are an orphan under sent/, which is not
// lost mail, since nothing was sent.
//
// It is the only statement that creates an outbox row, and
// (*PolicyTx).Enqueue is its only caller (TestOneWayToTheOutbox): an
// outbox row exists only where sendpolicy.Decide said Queue.
func enqueueOutbox(ctx context.Context, tx pgx.Tx, o outboxRow) error {
	addrs, folds, kinds := dedupeRecipients(o.Recipients)
	if len(addrs) == 0 {
		return fmt.Errorf("enqueue %s: no recipients", o.ID)
	}
	for _, k := range kinds {
		if k != RecipientTo && k != RecipientCc && k != RecipientBcc {
			return fmt.Errorf("enqueue %s: recipient kind %q", o.ID, k)
		}
	}
	if err := lockContent(ctx, tx); err != nil {
		return err
	}
	if o.newRaw {
		_, err := tx.Exec(ctx, `
			INSERT INTO raw_objects (id, r2_key, local_path, size, sha256, received_at, completeness, content_hash)
			VALUES ($1, $2, $2, $3, $4, $5, 1, $6)`,
			o.Raw.ID, o.Raw.Key, o.Raw.Size, o.Raw.SHA256[:], o.Raw.At, o.Raw.ContentHash[:])
		if err != nil {
			return fmt.Errorf("enqueue %s: insert raw_objects: %w", o.ID, err)
		}
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO outbox (id, state, next_retry_at, hold_until, review,
		                    identity_id, from_addr, submitted_by, approved_by, draft_id, raw_id)
		VALUES ($1, CASE WHEN $8 THEN 'held' ELSE 'queued' END,
		        CASE WHEN $8 THEN NULL ELSE now() END,
		        CASE WHEN $8 THEN now() + $9 * interval '1 second' END, $10,
		        $2, $3, $4, $5, $6, $7)`,
		o.ID, o.identityID, o.fromAddr, o.submittedBy, nullable(o.approvedBy), nullable(o.DraftID), o.Raw.ID,
		o.held, o.holdSeconds, o.review)
	if err != nil {
		return fmt.Errorf("enqueue %s: insert outbox: %w", o.ID, err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO outbox_recipients (outbox_id, addr, addr_fold, kind)
		SELECT $1, a, f, k FROM unnest($2::text[], $3::text[], $4::text[]) AS u(a, f, k)`,
		o.ID, addrs, folds, kinds)
	if err != nil {
		return fmt.Errorf("enqueue %s: insert outbox_recipients: %w", o.ID, err)
	}
	if err := outboxChanged(ctx, tx, o.ID); err != nil {
		return fmt.Errorf("enqueue %s: %w", o.ID, err)
	}
	return nil
}

func dedupeRecipients(rs []OutboxRecipient) (addrs, folds, kinds []string) {
	seen := map[string]bool{}
	for _, r := range rs {
		f := FoldAddr(r.Addr)
		if seen[f] {
			continue
		}
		seen[f] = true
		addrs, folds, kinds = append(addrs, r.Addr), append(folds, f), append(kinds, r.Kind)
	}
	return addrs, folds, kinds
}

// outboxChanged logs one outbox change (§4: one change covers the row, its
// attempts and recipients). The caller holds contentLock.
func outboxChanged(ctx context.Context, tx pgx.Tx, id string) error {
	w := writer{tx: tx}
	w.change("outbox", id)
	return w.flush(ctx)
}

func lockContent(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, contentLock)
	return err
}

// Claimed is an attempt an executor now owns.
type Claimed struct {
	OutboxID  string
	AttemptID string
	FromAddr  string
	Transport string
	// Recipients of this attempt, verbatim: those still pending or
	// deferred, so a retry after partial goes to the deferred ones only.
	Recipients []string
	RawID      string
	R2Key      string
	LocalPath  string
	Size       int64
	SHA256     []byte
}

// ClaimOutbox takes the oldest due outbox row whose identity's primary
// transport is one of transports (by name, as the process configures
// them; the chain's fallbacks are not used yet), moves it to sending and
// opens an attempt owned by owner with a lease of lease, all in one
// transaction. It returns nil when nothing is due. Concurrent claimers
// skip each other's rows (SKIP LOCKED), and the unique index on open
// attempts would refuse a second one anyway.
func (db *DB) ClaimOutbox(ctx context.Context, owner string, transports []string, lease time.Duration) (*Claimed, error) {
	if len(transports) == 0 {
		return nil, nil
	}
	var c *Claimed
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if err := lockContent(ctx, tx); err != nil {
			return err
		}
		var cl Claimed
		err := tx.QueryRow(ctx, `
			SELECT o.id, o.from_addr, t.name, r.id, r.r2_key, r.local_path, r.size, r.sha256
			FROM outbox o
			JOIN identity_transports it ON it.identity_id = o.identity_id AND it.position = 0
			JOIN transports t ON t.id = it.transport_id
			JOIN raw_objects r ON r.id = o.raw_id
			WHERE o.next_retry_at <= now() AND t.name = ANY($1)
			ORDER BY o.next_retry_at, o.id
			LIMIT 1
			FOR UPDATE OF o SKIP LOCKED`, transports).
			Scan(&cl.OutboxID, &cl.FromAddr, &cl.Transport, &cl.RawID, &cl.R2Key, &cl.LocalPath, &cl.Size, &cl.SHA256)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("select due: %w", err)
		}
		rows, err := tx.Query(ctx, `
			SELECT addr FROM outbox_recipients
			WHERE outbox_id = $1 AND state IN ('pending', 'deferred')
			ORDER BY array_position(ARRAY['to', 'cc', 'bcc'], kind), addr_fold`, cl.OutboxID)
		if err != nil {
			return fmt.Errorf("recipients: %w", err)
		}
		if cl.Recipients, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return fmt.Errorf("recipients: %w", err)
		}
		if len(cl.Recipients) == 0 {
			// Due with nobody left to send to cannot be reached through
			// this package; settle it from its recipients rather than
			// claim it forever.
			if _, err := advance(ctx, tx, cl.OutboxID, RetryPolicy{}, ""); err != nil {
				return err
			}
			return outboxChanged(ctx, tx, cl.OutboxID)
		}
		cl.AttemptID = ulid.New(time.Now())
		if _, err := tx.Exec(ctx, `UPDATE outbox SET state = 'sending', next_retry_at = NULL WHERE id = $1`, cl.OutboxID); err != nil {
			return fmt.Errorf("mark sending: %w", err)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO send_attempts (id, outbox_id, recipients, transport, owner, lease_until)
			VALUES ($1, $2, $3, $4, $5, now() + $6 * interval '1 millisecond')`,
			cl.AttemptID, cl.OutboxID, cl.Recipients, cl.Transport, owner, lease.Milliseconds())
		if err != nil {
			return fmt.Errorf("insert send_attempts: %w", err)
		}
		c = &cl
		return outboxChanged(ctx, tx, cl.OutboxID)
	})
	if err != nil {
		return nil, fmt.Errorf("claim outbox: %w", err)
	}
	return c, nil
}

// CommitAttempt is the commit point of §4.5: it records committed_at and
// commits before returning, so the record exists before the transport
// crosses the point of no return. It fails with ErrLeaseLost unless the
// attempt is still open, owned by owner and within its lease; recovery
// requires an expired lease, so of the two at most one wins, and a stuck
// executor that comes back late cannot send.
func (db *DB) CommitAttempt(ctx context.Context, attemptID, owner string) error {
	tag, err := db.pool.Exec(ctx, `
		UPDATE send_attempts SET committed_at = now()
		WHERE id = $1 AND owner = $2 AND finished_at IS NULL AND committed_at IS NULL AND lease_until > now()`,
		attemptID, owner)
	if err != nil {
		return fmt.Errorf("commit attempt %s: %w", attemptID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("commit attempt %s: %w", attemptID, ErrLeaseLost)
	}
	return nil
}

// RenewLease extends an open attempt's lease to now + lease. False means
// the attempt is no longer open: recovery finished it.
func (db *DB) RenewLease(ctx context.Context, attemptID, owner string, lease time.Duration) (bool, error) {
	tag, err := db.pool.Exec(ctx, `
		UPDATE send_attempts SET lease_until = now() + $3 * interval '1 millisecond'
		WHERE id = $1 AND owner = $2 AND finished_at IS NULL`, attemptID, owner, lease.Milliseconds())
	if err != nil {
		return false, fmt.Errorf("renew lease %s: %w", attemptID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// RetryPolicy decides what happens to recipients left to retry.
type RetryPolicy struct {
	// MaxFailures is how many failed or partial attempts a message gets
	// before the executor gives up on the recipients still deferred.
	// Abandoned and unknown attempts do not count.
	MaxFailures int
	// Delay is the wait after the n-th failed or partial attempt (n >= 1).
	Delay func(n int) time.Duration
}

// RecipientResult is what one attempt learned about one recipient.
type RecipientResult struct {
	Addr   string // verbatim, as in Claimed.Recipients
	State  string // RecipientAccepted, RecipientDeferred, RecipientRejected or RecipientUnknown
	Code   int    // the SMTP reply code; 0 when none decided it
	Detail string
}

// AttemptResult is what an attempt reports when it ends with an answer.
type AttemptResult struct {
	Outcome    string // AttemptAccepted, AttemptPartial, AttemptFailed, AttemptRejected or AttemptUnknown
	Recipients []RecipientResult
	ProviderID string
	MessageID  string
	Error      string
}

// Finished says what FinishAttempt did.
type Finished struct {
	// State is the outbox state afterwards.
	State string
	// Late: recovery had already finished the attempt, and this result
	// came after its lease. A result for a recovered unknown is recorded
	// in the attempt row as reconciliation evidence; Applied says whether
	// it also settled the outbox row, which it does only while the row is
	// still unknown from this very attempt. Anything else late is ignored.
	Late    bool
	Applied bool
}

// FinishAttempt records the result of attempt attemptID and advances its
// outbox row per §4.5, in one transaction.
func (db *DB) FinishAttempt(ctx context.Context, attemptID string, res AttemptResult, p RetryPolicy) (Finished, error) {
	var f Finished
	switch res.Outcome {
	case AttemptAccepted, AttemptPartial, AttemptFailed, AttemptRejected, AttemptUnknown:
	default:
		return f, fmt.Errorf("finish attempt %s: outcome %q", attemptID, res.Outcome)
	}
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if err := lockContent(ctx, tx); err != nil {
			return err
		}
		var outboxID string
		var finished bool
		var outcome *string
		err := tx.QueryRow(ctx, `
			SELECT outbox_id, finished_at IS NOT NULL, outcome FROM send_attempts WHERE id = $1 FOR UPDATE`,
			attemptID).Scan(&outboxID, &finished, &outcome)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var state string
		if err := tx.QueryRow(ctx, `SELECT state FROM outbox WHERE id = $1 FOR UPDATE`, outboxID).Scan(&state); err != nil {
			return fmt.Errorf("lock outbox: %w", err)
		}
		f.State = state
		if finished {
			// Recovery got here first. Only a recovered unknown can learn
			// something from a late answer; an abandoned attempt never
			// committed, so its executor's answer says nothing new.
			f.Late = true
			if outcome == nil || *outcome != AttemptUnknown || res.Outcome == AttemptUnknown {
				return nil
			}
			if err := recordAttempt(ctx, tx, attemptID, res); err != nil {
				return err
			}
			var latest string
			if err := tx.QueryRow(ctx, `
				SELECT id FROM send_attempts WHERE outbox_id = $1 ORDER BY started_at DESC, id DESC LIMIT 1`,
				outboxID).Scan(&latest); err != nil {
				return fmt.Errorf("latest attempt: %w", err)
			}
			if state != OutboxUnknown || latest != attemptID {
				return nil
			}
			f.Applied = true
		} else if err := recordAttempt(ctx, tx, attemptID, res); err != nil {
			return err
		}
		if state != OutboxSending && !f.Applied {
			// Nothing else moves a row out of sending while it has an
			// open attempt; leave anything else alone.
			return nil
		}
		if err := applyRecipients(ctx, tx, outboxID, attemptID, res.Recipients); err != nil {
			return err
		}
		if f.State, err = advance(ctx, tx, outboxID, p, res.Error); err != nil {
			return err
		}
		return outboxChanged(ctx, tx, outboxID)
	})
	if err != nil {
		return f, fmt.Errorf("finish attempt %s: %w", attemptID, err)
	}
	return f, nil
}

func recordAttempt(ctx context.Context, tx pgx.Tx, id string, res AttemptResult) error {
	_, err := tx.Exec(ctx, `
		UPDATE send_attempts SET finished_at = now(), outcome = $2, provider_id = $3, message_id = $4, error = $5
		WHERE id = $1`, id, res.Outcome, nullable(res.ProviderID), nullable(res.MessageID), nullable(res.Error))
	if err != nil {
		return fmt.Errorf("record attempt: %w", err)
	}
	return nil
}

func applyRecipients(ctx context.Context, tx pgx.Tx, outboxID, attemptID string, rs []RecipientResult) error {
	if len(rs) == 0 {
		return nil
	}
	addrs := make([]string, len(rs))
	states := make([]string, len(rs))
	codes := make([]int32, len(rs))
	details := make([]string, len(rs))
	for i, r := range rs {
		addrs[i], states[i], codes[i], details[i] = r.Addr, r.State, int32(r.Code), r.Detail
	}
	_, err := tx.Exec(ctx, `
		UPDATE outbox_recipients o SET state = u.state,
			smtp_code = CASE WHEN u.code BETWEEN 200 AND 599 THEN u.code::smallint END,
			detail = nullif(u.detail, ''), last_attempt_id = $2, updated_at = now()
		FROM unnest($3::text[], $4::text[], $5::int[], $6::text[]) AS u(addr, state, code, detail)
		WHERE o.outbox_id = $1 AND o.addr = u.addr`,
		outboxID, attemptID, addrs, states, codes, details)
	if err != nil {
		return fmt.Errorf("record recipients: %w", err)
	}
	return nil
}

// advance sets the state of an outbox row that has no open attempt from
// its recipients (§4.5) and returns it:
//
//   - any recipient unknown: unknown, never retried by itself
//   - recipients left to send (pending or deferred), within the retry
//     cap: failed, or partial if someone already accepted it, due after
//     p.Delay
//   - the same beyond the cap: dead, or partial if someone accepted it;
//     the deferred recipients stay deferred
//   - nothing left: sent if everyone accepted it, partial if some did,
//     dead if nobody did
//
// errText, when not empty, becomes outbox.error; sent clears it.
func advance(ctx context.Context, tx pgx.Tx, outboxID string, p RetryPolicy, errText string) (string, error) {
	var accepted, retry, unknown, total, failures int
	err := tx.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE state IN ('accepted', 'bounced', 'complained')),
		       count(*) FILTER (WHERE state IN ('pending', 'deferred')),
		       count(*) FILTER (WHERE state = 'unknown'),
		       count(*),
		       (SELECT count(*) FROM send_attempts WHERE outbox_id = $1 AND outcome IN ('failed', 'partial'))
		FROM outbox_recipients WHERE outbox_id = $1`, outboxID).
		Scan(&accepted, &retry, &unknown, &total, &failures)
	if err != nil {
		return "", fmt.Errorf("count recipients: %w", err)
	}
	var state string
	var delay *int64 // milliseconds until next_retry_at; nil clears it
	switch {
	case unknown > 0:
		state = OutboxUnknown
	case retry > 0 && failures < p.MaxFailures:
		state = OutboxFailed
		if accepted > 0 {
			state = OutboxPartial
		}
		d := p.Delay(max(failures, 1)).Milliseconds()
		delay = &d
	case retry > 0:
		state = OutboxDead
		if accepted > 0 {
			state = OutboxPartial
		}
		errText = strings.TrimSuffix(fmt.Sprintf("gave up after %d failed attempts: %s", failures, errText), ": ")
	case accepted == total:
		state = OutboxSent
	case accepted > 0:
		state = OutboxPartial
	default:
		state = OutboxDead
	}
	_, err = tx.Exec(ctx, `
		UPDATE outbox SET state = $2,
			next_retry_at = CASE WHEN $3::bigint IS NULL THEN NULL ELSE now() + $3 * interval '1 millisecond' END,
			error = CASE WHEN $2 = 'sent' THEN NULL ELSE coalesce(nullif($4, ''), error) END
		WHERE id = $1`, outboxID, state, delay, errText)
	if err != nil {
		return "", fmt.Errorf("advance outbox: %w", err)
	}
	return state, nil
}

// AbandonAttempt ends an open attempt that did not reach the commit point
// as abandoned and puts its outbox row back to queued, due after retry
// (§4.5: nothing was delivered, and the reason was not the message). It
// returns false when the attempt was no longer open or had committed.
func (db *DB) AbandonAttempt(ctx context.Context, attemptID, errText string, retry time.Duration) (bool, error) {
	var ok bool
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if err := lockContent(ctx, tx); err != nil {
			return err
		}
		var outboxID string
		err := tx.QueryRow(ctx, `
			UPDATE send_attempts SET finished_at = now(), outcome = 'abandoned', error = $2
			WHERE id = $1 AND finished_at IS NULL AND committed_at IS NULL
			RETURNING outbox_id`, attemptID, nullable(errText)).Scan(&outboxID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		ok = true
		return requeue(ctx, tx, outboxID, errText, retry)
	})
	if err != nil {
		return false, fmt.Errorf("abandon attempt %s: %w", attemptID, err)
	}
	return ok, nil
}

func requeue(ctx context.Context, tx pgx.Tx, outboxID, errText string, retry time.Duration) error {
	_, err := tx.Exec(ctx, `
		UPDATE outbox SET state = 'queued', next_retry_at = now() + $2 * interval '1 millisecond',
			error = coalesce(nullif($3, ''), error)
		WHERE id = $1 AND state = 'sending'`, outboxID, retry.Milliseconds(), errText)
	if err != nil {
		return fmt.Errorf("requeue: %w", err)
	}
	return outboxChanged(ctx, tx, outboxID)
}

// Recovered is one attempt the recovery pass finished.
type Recovered struct {
	AttemptID string
	OutboxID  string
	Outcome   string // AttemptAbandoned or AttemptUnknown
}

// Recovery texts, also what people read in outbox.error.
const (
	lostBeforeCommit = "executor lost before the commit point; nothing was sent"
	lostAfterCommit  = "executor lost after the commit point; the message may have been delivered"
)

// RecoverAttempts finishes every open attempt whose lease has run out
// (§4.5). Without committed_at the remote provably did not get the body:
// the attempt is abandoned and the row goes back to queued, due after
// retry, its recipients untouched. With committed_at nothing can be proved:
// the attempt and the recipients it was sending to become unknown, and so
// does the row, which is never retried by itself.
func (db *DB) RecoverAttempts(ctx context.Context, retry time.Duration) ([]Recovered, error) {
	var out []Recovered
	for {
		var r *Recovered
		err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
			if err := lockContent(ctx, tx); err != nil {
				return err
			}
			var rec Recovered
			var committed bool
			err := tx.QueryRow(ctx, `
				SELECT id, outbox_id, committed_at IS NOT NULL FROM send_attempts
				WHERE finished_at IS NULL AND lease_until < now()
				ORDER BY lease_until LIMIT 1
				FOR UPDATE SKIP LOCKED`).Scan(&rec.AttemptID, &rec.OutboxID, &committed)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			r = &rec
			if !committed {
				rec.Outcome = AttemptAbandoned
				if _, err := tx.Exec(ctx, `
					UPDATE send_attempts SET finished_at = now(), outcome = 'abandoned', error = $2 WHERE id = $1`,
					rec.AttemptID, lostBeforeCommit); err != nil {
					return err
				}
				return requeue(ctx, tx, rec.OutboxID, lostBeforeCommit, retry)
			}
			rec.Outcome = AttemptUnknown
			if _, err := tx.Exec(ctx, `
				UPDATE send_attempts SET finished_at = now(), outcome = 'unknown', error = $2 WHERE id = $1`,
				rec.AttemptID, lostAfterCommit); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE outbox_recipients o SET state = 'unknown', detail = $3, last_attempt_id = a.id, updated_at = now()
				FROM send_attempts a
				WHERE a.id = $2 AND o.outbox_id = $1 AND o.addr = ANY(a.recipients) AND o.state IN ('pending', 'deferred')`,
				rec.OutboxID, rec.AttemptID, lostAfterCommit); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE outbox SET state = 'unknown', next_retry_at = NULL, error = $2
				WHERE id = $1 AND state = 'sending'`, rec.OutboxID, lostAfterCommit); err != nil {
				return err
			}
			return outboxChanged(ctx, tx, rec.OutboxID)
		})
		if err != nil {
			return out, fmt.Errorf("recover attempts: %w", err)
		}
		if r == nil {
			return out, nil
		}
		out = append(out, *r)
	}
}

// Parked is a due outbox row whose transport is not configured.
type Parked struct {
	OutboxID  string
	Transport string
}

// ParkUnconfigured pushes every due outbox row whose identity's primary
// transport is outside configured (or which has none) back by retry, and
// says so in outbox.error.
// A missing transport is a configuration error, not a verdict on the
// message: the row keeps its state and is not attempted, so it neither
// opens an attempt nor counts toward the retry cap.
func (db *DB) ParkUnconfigured(ctx context.Context, configured []string, retry time.Duration) ([]Parked, error) {
	if configured == nil {
		configured = []string{} // NULL would match nothing
	}
	var out []Parked
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if err := lockContent(ctx, tx); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			WITH due AS (
				SELECT o.id, coalesce(t.name, '') AS transport, o.error AS old
				FROM outbox o
				LEFT JOIN identity_transports it ON it.identity_id = o.identity_id AND it.position = 0
				LEFT JOIN transports t ON t.id = it.transport_id
				WHERE o.next_retry_at <= now() AND (t.name IS NULL OR NOT (t.name = ANY($1)))
				FOR UPDATE OF o SKIP LOCKED
			)
			UPDATE outbox o SET next_retry_at = now() + $2 * interval '1 millisecond',
				error = format('transport %s is not configured', quote_literal(due.transport))
			FROM due WHERE o.id = due.id
			RETURNING o.id, due.transport, due.old IS DISTINCT FROM o.error`, configured, retry.Milliseconds())
		if err != nil {
			return err
		}
		w := writer{tx: tx}
		var p Parked
		var changed bool
		_, err = pgx.ForEachRow(rows, []any{&p.OutboxID, &p.Transport, &changed}, func() error {
			out = append(out, p)
			if changed {
				w.change("outbox", p.OutboxID)
			}
			return nil
		})
		if err != nil {
			return err
		}
		return w.flush(ctx)
	})
	if err != nil {
		return nil, fmt.Errorf("park unconfigured: %w", err)
	}
	return out, nil
}

// ResolveOutbox is the human decision on an unknown row (§4.5):
// ResolveSent takes its unknown recipients as accepted and settles the row
// from its recipients (sent, or partial with any deferred ones still to
// retry); ResolveResend makes them pending again and queues the row now,
// accepting that someone may get it twice; ResolveAbandon makes it dead.
// The decision is recorded as a. It returns the new state; a row that is
// not unknown is ErrOutboxState.
func (db *DB) ResolveOutbox(ctx context.Context, id, resolution string, a Action, p RetryPolicy) (string, error) {
	var state string
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if err := lockContent(ctx, tx); err != nil {
			return err
		}
		var from string
		err := tx.QueryRow(ctx, `SELECT state FROM outbox WHERE id = $1 FOR UPDATE`, id).Scan(&from)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if from != OutboxUnknown {
			return fmt.Errorf("%w: it is %s, and only unknown can be resolved", ErrOutboxState, from)
		}
		switch resolution {
		case ResolveSent:
			if _, err := tx.Exec(ctx, `
				UPDATE outbox_recipients SET state = 'accepted', updated_at = now()
				WHERE outbox_id = $1 AND state = 'unknown'`, id); err != nil {
				return err
			}
			if state, err = advance(ctx, tx, id, p, ""); err != nil {
				return err
			}
		case ResolveResend:
			if _, err := tx.Exec(ctx, `
				UPDATE outbox_recipients SET state = 'pending', updated_at = now()
				WHERE outbox_id = $1 AND state = 'unknown'`, id); err != nil {
				return err
			}
			state = OutboxQueued
			if _, err := tx.Exec(ctx, `UPDATE outbox SET state = 'queued', next_retry_at = now() WHERE id = $1 AND state = 'unknown'`, id); err != nil {
				return err
			}
		case ResolveAbandon:
			state = OutboxDead
			if _, err := tx.Exec(ctx, `UPDATE outbox SET state = 'dead' WHERE id = $1`, id); err != nil {
				return err
			}
		default:
			return fmt.Errorf("resolution %q", resolution)
		}
		args := map[string]any{"outbox_id": id, "resolution": resolution, "from": from, "to": state}
		if extra, ok := a.Args.(map[string]any); ok {
			for k, v := range extra {
				args[k] = v
			}
		}
		a.Args = args
		if err := insertAction(ctx, tx, a); err != nil {
			return err
		}
		return outboxChanged(ctx, tx, id)
	})
	if err != nil {
		return "", fmt.Errorf("resolve outbox %s: %w", id, err)
	}
	return state, nil
}

// OutboxQuery selects rows for OutboxList.
type OutboxQuery struct {
	State string // "" for any
	// States, when not empty, also selects these states (with State).
	States []string
	// Before returns rows strictly older than this outbox id, which is a
	// ULID of the enqueue time: the cursor of the list.
	Before string
	Limit  int // <= 0 means 50; at most 500
}

// OutboxEntry is one row of OutboxList.
type OutboxEntry struct {
	ID string
	// Identity is the identity's address, *@zone for a pattern.
	Identity    string
	IdentityID  string
	State       string
	FromAddr    string
	CreatedAt   time.Time
	NextRetryAt *time.Time
	Error       string
	// DraftID is the draft it came from, "" for an SMTP submission sent
	// directly. MessageID is its sent copy as a message, "" until that is
	// indexed (§4.6).
	DraftID   string
	MessageID string
	// Subject is that message's subject, "" until it is indexed. The
	// outbox row has none of its own; a row v may see has a sent copy v
	// may read, by the same account rule (§13.8).
	Subject string
	// SubmittedBy started it; ApprovedBy let it go (Principal.ID "" while
	// nobody has).
	SubmittedBy Principal
	ApprovedBy  Principal
	// Recipients in RCPT order with their states.
	Recipients []OutboxRecipientState
	Attempts   int
	// LastOutcome is the newest attempt's outcome; "" while it runs or
	// when there is none.
	LastOutcome string
	// HoldUntil ends the recall window of a row the review tier passed
	// (set from held on); Review is that review's verdict
	// (sendpolicy.Verdict as JSON), nil for mail that was not reviewed.
	HoldUntil *time.Time
	Review    json.RawMessage
	// WithdrawnBy recalled it from held (Principal.ID "" unless withdrawn).
	WithdrawnBy Principal
}

// OutboxRecipientState is a recipient and its state.
type OutboxRecipientState struct {
	Addr  string
	Kind  string
	State string
}

const outboxEntryCols = `o.id, coalesce(i.prefix, '*') || '@' || i.zone, i.id, o.state, o.from_addr, o.created_at,
	o.next_retry_at, coalesce(o.error, ''), coalesce(o.draft_id, ''),
	coalesce(sm.id, ''), coalesce(sm.subject, ''),
	sp.id, sp.kind, sp.name, coalesce(ap.id, ''), coalesce(ap.kind, ''), coalesce(ap.name, ''),
	(SELECT count(*) FROM send_attempts a WHERE a.outbox_id = o.id),
	coalesce((SELECT a.outcome FROM send_attempts a WHERE a.outbox_id = o.id
		ORDER BY a.started_at DESC, a.id DESC LIMIT 1), ''),
	r.addrs, r.kinds, r.states, o.hold_until, o.review,
	coalesce(wp.id, ''), coalesce(wp.kind, ''), coalesce(wp.name, '')`

// outboxEntryFrom selects the outbox rows v may see: those whose
// identity's account v may read (§13.8, the rule of visibleOutbox).
func (db *DB) outboxEntryFrom(v *auth.Viewer) string {
	return `
		FROM outbox o
		JOIN identities i ON i.id = o.identity_id
		JOIN principals sp ON sp.id = o.submitted_by
		LEFT JOIN principals ap ON ap.id = o.approved_by
		LEFT JOIN principals wp ON wp.id = o.withdrawn_by
		LEFT JOIN LATERAL (
			SELECT m.id, m.subject FROM raw_objects sr JOIN messages m ON m.content_hash = sr.content_hash
			WHERE sr.id = o.raw_id ORDER BY m.id LIMIT 1
		) sm ON true
		CROSS JOIN LATERAL (
			SELECT array_agg(addr ORDER BY array_position(ARRAY['to', 'cc', 'bcc'], kind), addr_fold) AS addrs,
			       array_agg(kind ORDER BY array_position(ARRAY['to', 'cc', 'bcc'], kind), addr_fold) AS kinds,
			       array_agg(state ORDER BY array_position(ARRAY['to', 'cc', 'bcc'], kind), addr_fold) AS states
			FROM outbox_recipients WHERE outbox_id = o.id
		) r
		WHERE ` + db.readableAccount(v, "i.account_id")
}

func scanOutboxEntries(rows pgx.Rows) ([]OutboxEntry, error) {
	out := []OutboxEntry{}
	var e OutboxEntry
	var addrs, kinds, states []string
	s, a, w := &e.SubmittedBy, &e.ApprovedBy, &e.WithdrawnBy
	_, err := pgx.ForEachRow(rows, []any{
		&e.ID, &e.Identity, &e.IdentityID, &e.State, &e.FromAddr, &e.CreatedAt, &e.NextRetryAt, &e.Error,
		&e.DraftID, &e.MessageID, &e.Subject, &s.ID, &s.Kind, &s.Name, &a.ID, &a.Kind, &a.Name,
		&e.Attempts, &e.LastOutcome, &addrs, &kinds, &states, &e.HoldUntil, &e.Review, &w.ID, &w.Kind, &w.Name,
	}, func() error {
		e.Recipients = make([]OutboxRecipientState, len(addrs))
		for i := range addrs {
			e.Recipients[i] = OutboxRecipientState{Addr: addrs[i], Kind: kinds[i], State: states[i]}
		}
		out = append(out, e)
		// The next row's JSON must not land in this one's array.
		e.Review = nil
		return nil
	})
	return out, err
}

// OutboxList returns the outbox rows v may see, newest first. Like every
// read it needs a viewer (§13.8).
func (db *DB) OutboxList(ctx context.Context, v *auth.Viewer, q OutboxQuery) ([]OutboxEntry, error) {
	if err := needViewer(v, "outbox list"); err != nil {
		return nil, err
	}
	if q.Limit <= 0 {
		q.Limit = 50
	}
	q.Limit = min(q.Limit, 500)
	states := q.States
	if q.State != "" {
		states = append(slices.Clone(states), q.State)
	}
	rows, err := db.pool.Query(ctx, `SELECT `+outboxEntryCols+db.outboxEntryFrom(v)+`
		AND (coalesce(cardinality($1::text[]), 0) = 0 OR o.state = ANY($1))
		AND ($2 = '' OR o.id < $2)
		ORDER BY o.id DESC
		LIMIT $3`, states, q.Before, q.Limit)
	if err != nil {
		return nil, fmt.Errorf("outbox list: %w", err)
	}
	out, err := scanOutboxEntries(rows)
	if err != nil {
		return nil, fmt.Errorf("outbox list: %w", err)
	}
	return out, nil
}

// OutboxByID returns outbox row id if v may see it, ErrNotFound
// otherwise.
func (db *DB) OutboxByID(ctx context.Context, v *auth.Viewer, id string) (OutboxEntry, error) {
	if err := needViewer(v, "outbox"); err != nil {
		return OutboxEntry{}, err
	}
	rows, err := db.pool.Query(ctx, `SELECT `+outboxEntryCols+db.outboxEntryFrom(v)+` AND o.id = $1`, id)
	if err != nil {
		return OutboxEntry{}, fmt.Errorf("outbox %s: %w", id, err)
	}
	out, err := scanOutboxEntries(rows)
	if err != nil {
		return OutboxEntry{}, fmt.Errorf("outbox %s: %w", id, err)
	}
	if len(out) == 0 {
		return OutboxEntry{}, fmt.Errorf("outbox %s: %w", id, ErrNotFound)
	}
	return out[0], nil
}
