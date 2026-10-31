package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
)

// Tightening (§13.10): a person in the WebUI may make the system more
// conservative in one step, never less. These functions compare what a
// change leaves with what applies now and refuse anything that would let
// more through; widening stays with the host CLI (SetGrant, RemoveGrant,
// AddIdentity), which writes through the same functions underneath.

// WideningError refuses a tightening that would let more through in
// Fields (the identity_grants columns, sendpolicy.Field*); Current is
// the grant as it stands.
type WideningError struct {
	Fields  []string
	Current Grant
}

func (e *WideningError) Error() string {
	return "would widen " + strings.Join(e.Fields, ", ") + "; widening a grant is done on the host (yuuka grant)"
}

// ErrPatternTakesOver refuses to disable an address identity while its
// zone's *@zone identity is enabled: mail from the address would then
// resolve to the pattern and its grants (§9.3), which may let more
// through. The host CLI (yuuka identity rm) can still do it.
var ErrPatternTakesOver = errors.New("the zone's *@ identity would take the address over")

// GrantTightening is what a tightening asks for; a nil field leaves the
// column as it is.
type GrantTightening struct {
	Policy         *sendpolicy.Tier
	RecipientScope *sendpolicy.RecipientScope
	// Allowlist, when not nil, is the list to keep: entries of the
	// stored one, compared folded. Stored entries keep their spelling.
	Allowlist              []string
	MaxPerHour             *int
	MaxPerDay              *int
	MaxNewRecipientsPerDay *int
	// HoldSeconds is a recall window of its own, at least the one that
	// applies now (sendpolicy.EffectiveHoldSeconds).
	HoldSeconds *int
	Suspended   *bool
}

// RemovalWidenings lists what removing g would let through that g does
// not, none when removing it is a tightening: what applies without the
// row (Default) against the row (sendpolicy.RemovalWidenings). Nothing
// sends as a disabled identity, with or without its grants, so removing
// one never widens.
func (g Grant) RemovalWidenings() []string {
	if g.IdentityDisabled {
		return nil
	}
	return sendpolicy.RemovalWidenings(g.Applied(), g.Default())
}

// TightenGrant changes the grant of (identityID, principalID) as t asks,
// if what results lets nothing more through than the grant does now
// (sendpolicy.Widenings on both, applied: limits resolved, the owner cap
// taken), and records a with the result added to its Args (nil or a
// map[string]any). Otherwise it changes nothing and returns a
// *WideningError. A request that changes nothing records nothing.
//
// ErrNotFound if there is no such row: a pair without one is at its
// default, which only the host CLI changes. The row is written by
// setGrant, so the owner cap holds as for the CLI: review or auto where
// it applies would act as draft and widen nothing, and is ErrOwnerCap.
func (db *DB) TightenGrant(ctx context.Context, identityID, principalID string, t GrantTightening, a Action) (Grant, error) {
	extra, ok := a.Args.(map[string]any)
	if a.Args != nil && !ok {
		return Grant{}, fmt.Errorf("grant action args: want a map, got %T", a.Args)
	}
	var out Grant
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		cur, err := lockedGrant(ctx, tx, identityID, principalID)
		if err != nil {
			return err
		}
		next := cur
		if t.Policy != nil {
			next.Policy = *t.Policy
		}
		if t.RecipientScope != nil {
			next.RecipientScope = *t.RecipientScope
		}
		if t.Allowlist != nil {
			next.Allowlist = t.Allowlist // as asked, for the comparison
		}
		for _, l := range []struct {
			to   **int
			from *int
		}{
			{&next.MaxPerHour, t.MaxPerHour},
			{&next.MaxPerDay, t.MaxPerDay},
			{&next.MaxNewRecipientsPerDay, t.MaxNewRecipientsPerDay},
			{&next.HoldSeconds, t.HoldSeconds},
		} {
			if l.from != nil {
				if *l.from < 0 {
					return fmt.Errorf("a limit of %d: want >= 0", *l.from)
				}
				*l.to = l.from
			}
		}
		if s := t.Suspended; s != nil && *s && cur.SuspendedAt == nil {
			at := a.At
			next.SuspendedAt = &at
		} else if s != nil && !*s {
			next.SuspendedAt = nil
		}
		if w := sendpolicy.Widenings(cur.Applied(), next.Applied()); len(w) > 0 {
			return &WideningError{Fields: w, Current: cur}
		}
		if t.Allowlist != nil {
			// Nothing new can be in it now; store the entries as they
			// were written on the host.
			keep := lowered(t.Allowlist)
			next.Allowlist = slices.DeleteFunc(slices.Clone(cur.Allowlist), func(e string) bool {
				return !slices.Contains(keep, strings.ToLower(e))
			})
		}
		out = next
		if sameGrant(cur, next) {
			return nil
		}
		args := grantArgs(next)
		args["was"] = grantArgs(cur)
		for k, v := range extra {
			args[k] = v
		}
		a.Args = args
		return setGrant(ctx, tx, next, a)
	})
	if err != nil {
		return Grant{}, fmt.Errorf("tighten grant: %w", err)
	}
	return out, nil
}

// RemoveGrantIfTighter removes the grant of (identityID, principalID) if
// that widens nothing (Grant.RemovalWidenings), records a and returns
// the default that applies now. Otherwise it removes nothing and returns
// a *WideningError: removing an agent's draft grant on its own identity,
// say, hands it the review default. ErrNotFound if there is no such row.
func (db *DB) RemoveGrantIfTighter(ctx context.Context, identityID, principalID string, a Action) (sendpolicy.Grant, error) {
	extra, ok := a.Args.(map[string]any)
	if a.Args != nil && !ok {
		return sendpolicy.Grant{}, fmt.Errorf("grant action args: want a map, got %T", a.Args)
	}
	var def sendpolicy.Grant
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		cur, err := lockedGrant(ctx, tx, identityID, principalID)
		if err != nil {
			return err
		}
		def = cur.Default()
		if w := cur.RemovalWidenings(); len(w) > 0 {
			return &WideningError{Fields: w, Current: cur}
		}
		args := map[string]any{"was": grantArgs(cur), "default": string(def.Policy)}
		for k, v := range extra {
			args[k] = v
		}
		a.Args = args
		return removeGrant(ctx, tx, identityID, principalID, a)
	})
	if err != nil {
		return sendpolicy.Grant{}, fmt.Errorf("remove grant: %w", err)
	}
	return def, nil
}

// lockedGrant reads the grant row under the grant's lock and a row lock,
// in the order WithDecision takes them.
func lockedGrant(ctx context.Context, tx pgx.Tx, identityID, principalID string) (Grant, error) {
	if err := lockGrant(ctx, tx, identityID, principalID); err != nil {
		return Grant{}, err
	}
	return scanGrant(tx.QueryRow(ctx, `SELECT `+grantCols+grantFrom+`
		WHERE g.identity_id = $1 AND g.principal_id = $2 FOR UPDATE OF g`, identityID, principalID))
}

func sameGrant(a, b Grant) bool {
	eq := func(x, y *int) bool { return x == nil && y == nil || x != nil && y != nil && *x == *y }
	return a.Policy == b.Policy && a.RecipientScope == b.RecipientScope && slices.Equal(a.Allowlist, b.Allowlist) &&
		eq(a.MaxPerHour, b.MaxPerHour) && eq(a.MaxPerDay, b.MaxPerDay) && eq(a.MaxNewRecipientsPerDay, b.MaxNewRecipientsPerDay) && eq(a.HoldSeconds, b.HoldSeconds) &&
		(a.SuspendedAt == nil) == (b.SuspendedAt == nil)
}

// grantArgs is a grant's columns for the audit log, as yuuka grant set
// records them.
func grantArgs(g Grant) map[string]any {
	return map[string]any{
		"identity_id": g.IdentityID, "identity": g.IdentityAddress,
		"principal_id": g.PrincipalID, "principal": g.PrincipalKind + ":" + g.PrincipalName,
		"policy": g.Policy, "recipient_scope": g.RecipientScope, "allowlist": g.Allowlist,
		"max_per_hour": g.MaxPerHour, "max_per_day": g.MaxPerDay, "max_new_recipients_per_day": g.MaxNewRecipientsPerDay,
		"hold_seconds": g.HoldSeconds, "suspended": g.SuspendedAt != nil,
	}
}

func lowered(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strings.ToLower(strings.TrimSpace(s))
	}
	return out
}

// TakenOverBy returns the enabled identity among enabled that would send
// i's address once i is disabled: its zone's *@zone identity, for an
// address identity. An identity is never taken over by itself, and a
// pattern by nothing.
func (i Identity) TakenOverBy(enabled []Identity) (Identity, bool) {
	if i.Prefix == "" {
		return Identity{}, false
	}
	for _, o := range enabled {
		if o.Zone == i.Zone && o.Prefix == "" && o.DisabledAt == nil && o.ID != i.ID {
			return o, true
		}
	}
	return Identity{}, false
}

// DisableIdentityIfTighter disables the enabled identity id like
// DisableIdentity, unless another identity would take its address over
// (Identity.TakenOverBy): then it returns ErrPatternTakesOver and changes
// nothing. ErrNotFound if there is no such enabled identity.
func (db *DB) DisableIdentityIfTighter(ctx context.Context, id string, a Action) error {
	return pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		// The enabled identities of id's zone, locked so that the one
		// checked is still the one disabled.
		rows, err := tx.Query(ctx, `SELECT `+identityCols+identityFrom+`
			WHERE i.zone = (SELECT zone FROM identities WHERE id = $1) AND i.disabled_at IS NULL
			FOR UPDATE OF i`, id)
		if err != nil {
			return fmt.Errorf("disable identity %s: %w", id, err)
		}
		zone, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Identity, error) { return scanIdentity(r) })
		if err != nil {
			return fmt.Errorf("disable identity %s: %w", id, err)
		}
		i := slices.IndexFunc(zone, func(x Identity) bool { return x.ID == id })
		if i < 0 {
			return ErrNotFound
		}
		if p, ok := zone[i].TakenOverBy(zone); ok {
			return fmt.Errorf("%s: %s would send as it: %w", zone[i].Address(), p.Address(), ErrPatternTakesOver)
		}
		return disableIdentity(ctx, tx, id, a)
	})
}
