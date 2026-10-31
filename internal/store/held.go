package store

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// The recall window of the review tier (§4.5, §8.9): a held row leaves
// held one of two ways. PromoteHeld releases it to queued once its window
// has ended; it is the only statement that does, and TestOneWayToTheOutbox
// keeps it so. WithdrawOutbox sends it back to its draft before that.
// Both take the content lock and then the row, so they serialize: a
// release that came first leaves the recall a clean "already released",
// a recall that came first leaves the release nothing to match. Neither
// ever touches hold_until or the verdict, so a window is never shortened.
//
// Lock order of everything that sends (TestHeldLockOrder runs the paths
// against each other):
//
//	grant lock → draft row → content lock → outbox rows → drafts of held rows
//
//   - WithDecision takes the grant's advisory lock, then the submitted
//     draft (LockDraft), and the content lock last, in Enqueue or the
//     draft writes; the outbox row it inserts is new, and its foreign
//     key's share lock is on a draft it already holds or just created.
//   - Draft writers (UpdateDraft, DeleteDraft, holdDraft) lock their
//     draft, then the content lock when they log the change.
//   - ClaimOutbox, FinishAttempt and recovery take the content lock,
//     then outbox rows; they touch no draft.
//   - WithdrawOutbox takes the grant lock, the content lock, the outbox
//     row, then its draft; PromoteHeld the content lock, the rows, then
//     the drafts of the rows it withdraws.
//
// The last two lock a draft after the content lock, the reverse of the
// draft writers. That cannot close a cycle: they only lock the draft of
// a held row, and such a draft is in flight. Every draft writer checks
// that (notInFlight, or the outbox history for a delete) right after
// locking the draft and gives up before it asks for the content lock;
// and a draft only becomes in flight in the transaction that holds its
// row lock (WithDecision). Nothing that holds a draft row ever waits for
// an outbox row.

// ErrNotYours: an agent or a service may withdraw only mail it
// submitted; a human may withdraw any mail they can see (§8.9).
var ErrNotYours = errors.New("only a human may withdraw mail someone else submitted")

// promoteBatch bounds the rows one PromoteHeld transaction releases; the
// rest wait for the next pass, a moment later.
const promoteBatch = 100

// Released is a held row PromoteHeld dealt with.
type Released struct {
	OutboxID string
	DraftID  string
	// State is queued, or withdrawn when the grant was suspended or the
	// identity or the submitter disabled during the window: then Reason
	// is suspended or policy.
	State  string
	Reason sendpolicy.HoldReason
}

// PromoteHeld releases the held rows whose window has ended, in one
// transaction under the content lock: each becomes queued and due now,
// approved by system (by its submitter when a human,
// sendpolicy.ReleasedBy), unless its grant is suspended or its identity
// or submitter disabled, which withdraws it to its draft. Promotion does
// not count against the limits again: the submission spent its slot.
//
// It is idempotent and safe to run anywhere at any time; a server that
// is down keeps mail held, and releases it when it is back. next is how
// long until the earliest window still open ends, or a negative duration
// when nothing is held.
func (db *DB) PromoteHeld(ctx context.Context) (out []Released, next time.Duration, err error) {
	err = pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		var due bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM outbox WHERE state = 'held' AND hold_until <= now())`).
			Scan(&due); err != nil || !due {
			return err
		}
		if err := lockContent(ctx, tx); err != nil {
			return err
		}
		type held struct {
			id, draft                 string
			submitter                 sendpolicy.Principal
			disabled, suspended       bool
			identityID, submitterName string
		}
		rows, err := tx.Query(ctx, `
			SELECT o.id, o.draft_id, o.identity_id, o.submitted_by, sp.kind, sp.name,
			       i.disabled_at IS NOT NULL OR sp.disabled_at IS NOT NULL, coalesce(g.suspended_at IS NOT NULL, false)
			FROM outbox o
			JOIN identities i ON i.id = o.identity_id
			JOIN principals sp ON sp.id = o.submitted_by
			LEFT JOIN identity_grants g ON g.identity_id = o.identity_id AND g.principal_id = o.submitted_by
			WHERE o.state = 'held' AND o.hold_until <= now()
			ORDER BY o.hold_until, o.id
			LIMIT $1
			FOR UPDATE OF o SKIP LOCKED`, promoteBatch)
		if err != nil {
			return err
		}
		var hs []held
		var h held
		if _, err := pgx.ForEachRow(rows, []any{
			&h.id, &h.draft, &h.identityID, &h.submitter.ID, &h.submitter.Kind,
			&h.submitterName, &h.disabled, &h.suspended,
		}, func() error {
			hs = append(hs, h)
			return nil
		}); err != nil {
			return err
		}
		now := time.Now()
		for _, h := range hs {
			r := Released{OutboxID: h.id, DraftID: h.draft, State: OutboxWithdrawn}
			switch {
			case h.disabled:
				r.Reason = sendpolicy.HoldPolicy
				err = withdrawHeld(ctx, tx, h.id, h.draft, SystemPrincipalID, r.Reason,
					"the identity or the submitter was disabled before the recall window ended", now)
			case h.suspended:
				r.Reason = sendpolicy.HoldSuspended
				err = withdrawHeld(ctx, tx, h.id, h.draft, SystemPrincipalID, r.Reason,
					"the grant was suspended before the recall window ended", now)
			default:
				r.State = OutboxQueued
				approver := sendpolicy.ReleasedBy(h.submitter)
				// The one statement that moves a row from held to queued.
				if _, err = tx.Exec(ctx, `UPDATE outbox SET state = 'queued', next_retry_at = now(), approved_by = $2
					WHERE id = $1 AND state = 'held'`, h.id, approver); err == nil {
					err = outboxChanged(ctx, tx, h.id)
				}
			}
			if err != nil {
				return fmt.Errorf("outbox %s: %w", h.id, err)
			}
			tool := "outbox.release"
			if r.State == OutboxWithdrawn {
				tool = "outbox.withdraw"
			}
			if err := insertAction(ctx, tx, Action{
				ID: ulid.New(now), Actor: SystemPrincipalID, Tool: tool, At: now,
				Args: map[string]any{
					"outbox_id": h.id, "draft_id": h.draft, "identity_id": h.identityID,
					"submitted_by": h.submitter.ID, "state": r.State, "reason": r.Reason,
				},
			}); err != nil {
				return err
			}
			out = append(out, r)
		}
		return nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("promote held: %w", err)
	}
	var secs *float64
	if err := db.pool.QueryRow(ctx, `SELECT extract(epoch FROM min(hold_until) - now())::float8 FROM outbox WHERE state = 'held'`).
		Scan(&secs); err != nil {
		return out, 0, fmt.Errorf("promote held: next window: %w", err)
	}
	if secs == nil {
		return out, -1, nil
	}
	return out, max(time.Duration(*secs*float64(time.Second)), 0), nil
}

// withdrawHeld moves held row id back to its draft, by whom and why. The
// caller holds the content lock and the row.
func withdrawHeld(ctx context.Context, tx pgx.Tx, id, draft, by string, reason sendpolicy.HoldReason, detail string, at time.Time) error {
	tag, err := tx.Exec(ctx, `UPDATE outbox SET state = 'withdrawn', withdrawn_by = $2 WHERE id = $1 AND state = 'held'`, id, by)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: not held", ErrOutboxState)
	}
	if _, err := tx.Exec(ctx, `UPDATE drafts SET hold_reason = $2, hold_detail = $3, review = NULL, updated_at = $4 WHERE id = $1`,
		draft, string(reason), detail, at); err != nil {
		return err
	}
	if err := logDraftChange(ctx, tx, draft); err != nil {
		return err
	}
	return outboxChanged(ctx, tx, id)
}

// Withdrawal is what WithdrawOutbox did.
type Withdrawal struct {
	OutboxID string
	// DraftID is the draft it went back to, hold reason withdrawn, to be
	// edited and submitted again or deleted.
	DraftID string
	// Suspended is set when this recall tripped the circuit breaker.
	Suspended *Suspension
}

// Suspension is a grant the circuit breaker suspended (§8.9).
type Suspension struct {
	IdentityID  string
	PrincipalID string
	// Identity is the identity's address (*@zone for a pattern) and
	// Principal its submitter, read in the same transaction: what a
	// person needs to name the grant to resume it.
	Identity  string
	Principal Principal
	// Recalls by humans of the grant's mail submitted in the last 24
	// hours, this one included.
	Recalls int
	// Withdrawn are the grant's other held rows, withdrawn with it.
	Withdrawn []string
}

// WithdrawOutbox recalls held row id as v at time at: it becomes
// withdrawn and its draft gets hold reason withdrawn, editable and
// submittable again. A human may withdraw any row they can see, an agent
// or a service only what it submitted (ErrNotYours); a row v cannot see
// is ErrNotFound. Only held can be withdrawn: a row already released,
// even if not yet sent, is ErrOutboxState with its state, since queued
// mail may be on the wire at any moment.
//
// via is added to the audit entry's args (outbox.withdraw): how the
// recall came, such as {"via": "telegram", "telegram_user_id": ...}.
//
// A recall by a human counts toward the grant's circuit breaker: when
// humans have withdrawn sendpolicy.MaxRecallsPerDay of its rows submitted
// in the last 24 hours, the grant is suspended (a row is written with the
// effective values if it had none) and its other held rows withdrawn, in
// the same transaction. Agents withdrawing their own mail do not count:
// that is an agent correcting itself, not a person saying no.
func (db *DB) WithdrawOutbox(ctx context.Context, v *auth.Viewer, id string, via map[string]any, at time.Time) (Withdrawal, error) {
	if err := needViewer(v, "withdraw"); err != nil {
		return Withdrawal{}, err
	}
	w := Withdrawal{OutboxID: id}
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		var identityID, submitter string
		err := tx.QueryRow(ctx, `SELECT o.identity_id, o.submitted_by FROM outbox o JOIN identities i ON i.id = o.identity_id
			WHERE o.id = $1 AND `+db.readableAccount(v, "i.account_id"), id).Scan(&identityID, &submitter)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if v.PrincipalKind != sendpolicy.KindHuman && submitter != v.PrincipalID {
			return ErrNotYours
		}
		// The grant's lock first, as WithDecision takes it, then the
		// content lock and the row: the breaker below reads and writes
		// the grant, and the lock order stays grant, content, rows.
		if err := lockGrant(ctx, tx, identityID, submitter); err != nil {
			return err
		}
		if err := lockContent(ctx, tx); err != nil {
			return err
		}
		var state string
		var draft *string
		if err := tx.QueryRow(ctx, `SELECT state, draft_id FROM outbox WHERE id = $1 FOR UPDATE`, id).Scan(&state, &draft); err != nil {
			return err
		}
		if state != OutboxHeld || draft == nil {
			return fmt.Errorf("%w: it is %s, and only held mail can be withdrawn", ErrOutboxState, state)
		}
		w.DraftID = *draft
		if err := withdrawHeld(ctx, tx, id, w.DraftID, v.PrincipalID, sendpolicy.HoldWithdrawn,
			"withdrawn by "+v.String()+" during the recall window", at); err != nil {
			return err
		}
		args := map[string]any{"outbox_id": id, "draft_id": w.DraftID, "identity_id": identityID, "submitted_by": submitter}
		maps.Copy(args, via)
		if err := insertAction(ctx, tx, Action{
			ID: ulid.New(at), Actor: v.PrincipalID, CredentialID: v.CredentialID, Tool: "outbox.withdraw", Args: args, At: at,
		}); err != nil {
			return err
		}
		if v.PrincipalKind != sendpolicy.KindHuman {
			return nil
		}
		w.Suspended, err = breaker(ctx, tx, identityID, submitter, at)
		return err
	})
	if err != nil {
		return Withdrawal{}, fmt.Errorf("withdraw %s: %w", id, err)
	}
	return w, nil
}

// breaker suspends the grant of (identity, submitter) when humans have
// withdrawn sendpolicy.MaxRecallsPerDay of its rows submitted in the last
// 24 hours, the window every count of a grant uses (§13.5), and withdraws
// its other held rows. nil when it does not trip, or the grant is
// suspended already.
func breaker(ctx context.Context, tx pgx.Tx, identityID, submitter string, at time.Time) (*Suspension, error) {
	s := &Suspension{IdentityID: identityID, PrincipalID: submitter}
	err := tx.QueryRow(ctx, `
		SELECT count(*) FROM outbox o JOIN principals wp ON wp.id = o.withdrawn_by
		WHERE o.identity_id = $1 AND o.submitted_by = $2 AND o.state = 'withdrawn' AND wp.kind = 'human'
		  AND o.created_at > now() - interval '24 hours'`, identityID, submitter).Scan(&s.Recalls)
	if err != nil {
		return nil, fmt.Errorf("breaker: %w", err)
	}
	if s.Recalls < sendpolicy.MaxRecallsPerDay {
		return nil, nil
	}
	i, err := identityByID(ctx, tx, identityID)
	if err != nil {
		return nil, fmt.Errorf("breaker: %w", err)
	}
	s.Identity = i.Address()
	if s.Principal, err = scanPrincipal(tx.QueryRow(ctx, `SELECT `+principalCols+` FROM principals WHERE id = $1`, submitter)); err != nil {
		return nil, fmt.Errorf("breaker: principal %s: %w", submitter, err)
	}
	g, err := grantFor(ctx, tx, identityID, submitter)
	switch {
	case err == nil && g.SuspendedAt != nil:
		return nil, nil
	case err == nil:
		_, err = tx.Exec(ctx, `UPDATE identity_grants SET suspended_at = $3 WHERE identity_id = $1 AND principal_id = $2`,
			identityID, submitter, at)
	case errors.Is(err, ErrNotFound):
		// No row: the default applied. Write it out as it was, suspended,
		// so resuming restores exactly what applied before.
		var in sendpolicy.Input
		in.Identity.ID = identityID
		if err = tx.QueryRow(ctx, `SELECT i.owner_id, op.kind, sp.kind FROM identities i
			JOIN principals op ON op.id = i.owner_id, principals sp WHERE i.id = $1 AND sp.id = $2`, identityID, submitter).
			Scan(&in.Identity.OwnerID, &in.Identity.OwnerKind, &in.Submitter.Kind); err != nil {
			return nil, fmt.Errorf("breaker: %w", err)
		}
		in.Submitter.ID = submitter
		eff := sendpolicy.Effective(in)
		_, err = tx.Exec(ctx, `
			INSERT INTO identity_grants (identity_id, principal_id, send_policy, recipient_scope, recipient_allowlist,
			                             suspended_at, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $6)`,
			identityID, submitter, string(eff.Policy), string(eff.RecipientScope), append([]string{}, eff.Allowlist...), at)
	}
	if err != nil {
		return nil, fmt.Errorf("breaker: suspend: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT id, draft_id FROM outbox WHERE identity_id = $1 AND submitted_by = $2 AND state = 'held'
		ORDER BY id FOR UPDATE`, identityID, submitter)
	if err != nil {
		return nil, fmt.Errorf("breaker: %w", err)
	}
	type held struct{ id, draft string }
	hs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (held, error) {
		var h held
		err := r.Scan(&h.id, &h.draft)
		return h, err
	})
	if err != nil {
		return nil, fmt.Errorf("breaker: %w", err)
	}
	for _, h := range hs {
		if err := withdrawHeld(ctx, tx, h.id, h.draft, SystemPrincipalID, sendpolicy.HoldSuspended,
			"the grant was suspended: its mail was recalled too often", at); err != nil {
			return nil, fmt.Errorf("breaker: withdraw %s: %w", h.id, err)
		}
		s.Withdrawn = append(s.Withdrawn, h.id)
	}
	err = insertAction(ctx, tx, Action{
		ID: ulid.New(at), Actor: SystemPrincipalID, Tool: "grant.suspend", At: at,
		Args: map[string]any{
			"identity_id": identityID, "principal_id": submitter, "reason": "circuit breaker",
			"recalls": s.Recalls, "withdrawn": s.Withdrawn,
		},
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}
