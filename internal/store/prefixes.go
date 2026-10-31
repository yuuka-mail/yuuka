package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// PrefixChange says what SetPrefixState did.
type PrefixChange int

const (
	// PrefixUnchanged: the prefix was already in that state; nothing was
	// written or recorded.
	PrefixUnchanged PrefixChange = iota
	// PrefixUpdated: the state of a registered prefix changed.
	PrefixUpdated
	// PrefixCreated: a prefix nobody has delivered to was claimed and
	// entered the registry.
	PrefixCreated
	// PrefixRemoved: a prefix that was claimed before any mail reached it
	// was unclaimed and left the registry again; a discovered prefix
	// without a delivery would be a row about nothing.
	PrefixRemoved
)

// SetPrefixState is the operator's decision of §3.3: claim (PrefixClaimed)
// or unclaim (PrefixDiscovered) zone/prefix, which must be lowercase as
// ingest stores them. A change is recorded as a, whose Args (nil or a
// map[string]any) gain the zone, prefix, state and outcome, and logged in
// changes. Unclaiming a
// prefix that is not registered is ErrNotFound.
//
// Nothing is rewritten: the §4.3 downgrade is computed when mail is read,
// so the decision applies at once to everything already delivered, and
// the notification decision of later deliveries reads it on ingest.
func (db *DB) SetPrefixState(ctx context.Context, zone, prefix string, state int, a Action) (PrefixChange, error) {
	if state != PrefixClaimed && state != PrefixDiscovered {
		return 0, fmt.Errorf("prefix state %d", state)
	}
	extra, ok := a.Args.(map[string]any)
	if a.Args != nil && !ok {
		return 0, fmt.Errorf("prefix action args: want a map, got %T", a.Args)
	}
	var out PrefixChange
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		// The content writers hold this lock too, so a delivery folding
		// concurrently sees the state before or after, never a mix, and
		// changes.seq stays in commit order.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, contentLock); err != nil {
			return err
		}
		var err error
		if state == PrefixClaimed {
			out, err = claimPrefix(ctx, tx, zone, prefix)
		} else {
			out, err = unclaimPrefix(ctx, tx, zone, prefix)
		}
		if err != nil || out == PrefixUnchanged {
			return err
		}
		w := writer{tx: tx}
		w.change("prefix", prefix+"@"+zone)
		if err := w.flush(ctx); err != nil {
			return err
		}
		args := map[string]any{"zone": zone, "prefix": prefix, "state": stateName(state), "outcome": out.String()}
		for k, v := range extra {
			args[k] = v
		}
		a.Args = args
		return insertAction(ctx, tx, a)
	})
	if err != nil {
		return 0, fmt.Errorf("set prefix %s@%s: %w", prefix, zone, err)
	}
	return out, nil
}

func claimPrefix(ctx context.Context, tx pgx.Tx, zone, prefix string) (PrefixChange, error) {
	var inserted bool
	err := tx.QueryRow(ctx, `
		INSERT INTO prefixes (zone, prefix, state) VALUES ($1, $2, $3)
		ON CONFLICT (zone, prefix) DO UPDATE SET state = excluded.state WHERE prefixes.state <> excluded.state
		RETURNING xmax = 0`, zone, prefix, PrefixClaimed).Scan(&inserted)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return PrefixUnchanged, nil
	case err != nil:
		return 0, fmt.Errorf("claim: %w", err)
	case inserted:
		return PrefixCreated, nil
	}
	return PrefixUpdated, nil
}

func unclaimPrefix(ctx context.Context, tx pgx.Tx, zone, prefix string) (PrefixChange, error) {
	var state int
	var seen bool
	err := tx.QueryRow(ctx, `SELECT state, first_seen IS NOT NULL FROM prefixes WHERE zone = $1 AND prefix = $2`,
		zone, prefix).Scan(&state, &seen)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, ErrNotFound
	case err != nil:
		return 0, fmt.Errorf("unclaim: %w", err)
	case state == PrefixDiscovered:
		return PrefixUnchanged, nil
	case !seen:
		_, err = tx.Exec(ctx, `DELETE FROM prefixes WHERE zone = $1 AND prefix = $2`, zone, prefix)
		if err != nil {
			return 0, fmt.Errorf("unclaim: %w", err)
		}
		return PrefixRemoved, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE prefixes SET state = $3 WHERE zone = $1 AND prefix = $2`, zone, prefix, PrefixDiscovered); err != nil {
		return 0, fmt.Errorf("unclaim: %w", err)
	}
	return PrefixUpdated, nil
}

func stateName(state int) string {
	if state == PrefixClaimed {
		return "claimed"
	}
	return "discovered"
}

func (c PrefixChange) String() string {
	switch c {
	case PrefixUnchanged:
		return "unchanged"
	case PrefixUpdated:
		return "updated"
	case PrefixCreated:
		return "created"
	case PrefixRemoved:
		return "removed"
	}
	return fmt.Sprintf("PrefixChange(%d)", int(c))
}
