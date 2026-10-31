package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
)

// ErrOwnerCap: a grant above draft for someone other than the human who
// owns the identity (§8.9). It would never take effect, so it is not
// stored.
var ErrOwnerCap = errors.New("the identity belongs to a human; anyone else is capped at draft")

// Grant is a row of identity_grants (§13.5) with the names lists show.
type Grant struct {
	IdentityID     string
	PrincipalID    string
	Policy         sendpolicy.Tier
	RecipientScope sendpolicy.RecipientScope
	Allowlist      []string
	// nil: no limit of its own; sendpolicy.DefaultLimits applies.
	MaxPerHour             *int
	MaxPerDay              *int
	MaxNewRecipientsPerDay *int
	HoldSeconds            int
	SuspendedAt            *time.Time
	CreatedAt              time.Time

	// Read only, filled by the reads.
	IdentityAddress string
	PrincipalKind   string
	PrincipalName   string
}

// ForPolicy converts the row for sendpolicy.
func (g Grant) ForPolicy() sendpolicy.Grant {
	return sendpolicy.Grant{
		Policy:         g.Policy,
		RecipientScope: g.RecipientScope,
		Allowlist:      g.Allowlist,
		Limits: sendpolicy.Limits{
			MaxPerHour: g.MaxPerHour, MaxPerDay: g.MaxPerDay, MaxNewRecipientsPerDay: g.MaxNewRecipientsPerDay,
		},
		Suspended: g.SuspendedAt != nil,
	}
}

const grantCols = `g.identity_id, g.principal_id, g.send_policy, g.recipient_scope, g.recipient_allowlist,
	g.max_per_hour, g.max_per_day, g.max_new_recipients_per_day, g.hold_seconds, g.suspended_at, g.created_at,
	coalesce(i.prefix, '*') || '@' || i.zone, p.kind, p.name`

const grantFrom = ` FROM identity_grants g
	JOIN identities i ON i.id = g.identity_id
	JOIN principals p ON p.id = g.principal_id`

func scanGrant(row pgx.Row) (Grant, error) {
	var g Grant
	var policy, scope string
	err := row.Scan(&g.IdentityID, &g.PrincipalID, &policy, &scope, &g.Allowlist,
		&g.MaxPerHour, &g.MaxPerDay, &g.MaxNewRecipientsPerDay, &g.HoldSeconds, &g.SuspendedAt, &g.CreatedAt,
		&g.IdentityAddress, &g.PrincipalKind, &g.PrincipalName)
	if errors.Is(err, pgx.ErrNoRows) {
		return g, ErrNotFound
	}
	g.Policy, g.RecipientScope = sendpolicy.Tier(policy), sendpolicy.RecipientScope(scope)
	return g, err
}

// GrantFor returns the grant of (identity, principal), or ErrNotFound.
func (db *DB) GrantFor(ctx context.Context, identityID, principalID string) (Grant, error) {
	return grantFor(ctx, db.pool, identityID, principalID)
}

func grantFor(ctx context.Context, q querier, identityID, principalID string) (Grant, error) {
	g, err := scanGrant(q.QueryRow(ctx, `SELECT `+grantCols+grantFrom+`
		WHERE g.identity_id = $1 AND g.principal_id = $2`, identityID, principalID))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return g, fmt.Errorf("grant: %w", err)
	}
	return g, err
}

// Grants lists grants, narrowed to one identity and/or one principal when
// those are non-empty.
func (db *DB) Grants(ctx context.Context, identityID, principalID string) ([]Grant, error) {
	rows, err := db.pool.Query(ctx, `SELECT `+grantCols+grantFrom+`
		WHERE ($1::text IS NULL OR g.identity_id = $1) AND ($2::text IS NULL OR g.principal_id = $2)
		ORDER BY i.zone, i.prefix NULLS FIRST, p.kind, p.name`, nullable(identityID), nullable(principalID))
	if err != nil {
		return nil, fmt.Errorf("list grants: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Grant, error) { return scanGrant(r) })
	if err != nil {
		return nil, fmt.Errorf("list grants: %w", err)
	}
	return out, nil
}

// SetGrant creates or replaces the grant of (g.IdentityID, g.PrincipalID)
// and records a. CreatedAt is kept on replace.
//
// It refuses (ErrOwnerCap) a tier above draft for anyone but the human
// who owns the identity. sendpolicy.Decide caps it anyway; refusing here
// keeps the table from saying something that never happens, and tells
// the operator at the moment they try instead of at the first held mail.
func (db *DB) SetGrant(ctx context.Context, g Grant, a Action) error {
	if _, err := sendpolicy.ParseTier(string(g.Policy)); err != nil {
		return err
	}
	if _, err := sendpolicy.ParseRecipientScope(string(g.RecipientScope)); err != nil {
		return err
	}
	if g.Allowlist == nil {
		g.Allowlist = []string{}
	}
	return pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		// FOR SHARE: the owner checked is the owner when this commits.
		var ownerID, ownerKind string
		err := tx.QueryRow(ctx, `
			SELECT i.owner_id, p.kind FROM identities i JOIN principals p ON p.id = i.owner_id
			WHERE i.id = $1 FOR SHARE OF i`, g.IdentityID).Scan(&ownerID, &ownerKind)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("identity %s: %w", g.IdentityID, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("set grant: %w", err)
		}
		capped := sendpolicy.OwnerCapped(g.PrincipalID, sendpolicy.Identity{OwnerID: ownerID, OwnerKind: ownerKind})
		if capped && g.Policy != sendpolicy.None && g.Policy != sendpolicy.Draft {
			return ErrOwnerCap
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO identity_grants (identity_id, principal_id, send_policy, recipient_scope, recipient_allowlist,
			                             max_per_hour, max_per_day, max_new_recipients_per_day, hold_seconds,
			                             suspended_at, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (identity_id, principal_id) DO UPDATE SET
				send_policy = excluded.send_policy, recipient_scope = excluded.recipient_scope,
				recipient_allowlist = excluded.recipient_allowlist, max_per_hour = excluded.max_per_hour,
				max_per_day = excluded.max_per_day, max_new_recipients_per_day = excluded.max_new_recipients_per_day,
				hold_seconds = excluded.hold_seconds, suspended_at = excluded.suspended_at`,
			g.IdentityID, g.PrincipalID, string(g.Policy), string(g.RecipientScope), g.Allowlist,
			g.MaxPerHour, g.MaxPerDay, g.MaxNewRecipientsPerDay, g.HoldSeconds, g.SuspendedAt, a.At)
		if err != nil {
			return fmt.Errorf("set grant: %w", err)
		}
		return insertAction(ctx, tx, a)
	})
}

// RemoveGrant deletes the grant of (identity, principal) and records a.
// What applies afterwards is the default of sendpolicy.Effective, not
// nothing. ErrNotFound if there is no such grant.
func (db *DB) RemoveGrant(ctx context.Context, identityID, principalID string, a Action) error {
	return pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM identity_grants WHERE identity_id = $1 AND principal_id = $2`, identityID, principalID)
		if err != nil {
			return fmt.Errorf("remove grant: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return insertAction(ctx, tx, a)
	})
}

// grantLockKey is the first key of the advisory lock lockGrant takes;
// the second is a hash of the grant.
const grantLockKey = 0x79756b

// lockGrant serializes submissions of one (identity, principal) until tx
// ends, so that two concurrent submissions cannot both pass a limit with
// one slot left. An advisory lock rather than a row lock: a human sending
// as their own identity usually has no grant row to lock. WithDecision
// takes it, reads the policy input again under it and decides on that.
func lockGrant(ctx context.Context, tx pgx.Tx, identityID, principalID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2))`, grantLockKey, identityID+"/"+principalID)
	if err != nil {
		return fmt.Errorf("lock grant: %w", err)
	}
	return nil
}
