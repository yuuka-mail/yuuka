package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yuuka-mail/yuuka/internal/auth"
)

// ErrNoIdentity: no enabled identity covers an address.
var ErrNoIdentity = errors.New("no identity for this address")

// dbtx is what both a pool and a transaction offer, queries included.
type dbtx interface {
	querier
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Identity is a row of identities (§9.3) with its owner's kind and name.
type Identity struct {
	ID        string
	AccountID string
	// Zone and Prefix are lowercase; Prefix "" is the pattern *@zone.
	Zone             string
	Prefix           string
	DisplayName      string
	Signature        string
	PlainStripMarkup bool
	Transport        string
	OwnerID          string
	OwnerKind        string
	OwnerName        string
	CreatedAt        time.Time
	DisabledAt       *time.Time
}

// Address is the identity as people write it: local@zone, or *@zone for
// the pattern.
func (i Identity) Address() string {
	if i.Prefix == "" {
		return "*@" + i.Zone
	}
	return i.Prefix + "@" + i.Zone
}

// Account is a row of accounts.
type Account struct {
	ID   string
	Name string
	Kind string
}

// Accounts lists every account by name.
func (db *DB) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := db.pool.Query(ctx, `SELECT id, name, kind FROM accounts ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Account, error) {
		var a Account
		return a, r.Scan(&a.ID, &a.Name, &a.Kind)
	})
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	return out, nil
}

// SplitIdentityAddress reads local@zone or *@zone the way identities
// store it: both parts lowercased like ingest folds prefixes, so an
// identity matches the deliveries of the same address. prefix "" is the
// pattern.
func SplitIdentityAddress(addr string) (zone, prefix string, err error) {
	if addr == "" || strings.IndexFunc(addr, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", "", fmt.Errorf("%q: an address has no spaces or control characters", addr)
	}
	i := strings.LastIndexByte(addr, '@')
	if i <= 0 || i == len(addr)-1 {
		return "", "", fmt.Errorf("%q: want local@zone or *@zone", addr)
	}
	prefix, zone = strings.ToLower(addr[:i]), strings.ToLower(addr[i+1:])
	if prefix == "*" {
		prefix = ""
	}
	return zone, prefix, nil
}

// splitSendAddress is SplitIdentityAddress for a concrete address to send
// from: the pattern itself is not one.
func splitSendAddress(addr string) (zone, prefix string, err error) {
	zone, prefix, err = SplitIdentityAddress(addr)
	if err == nil && prefix == "" {
		err = fmt.Errorf("%q: not an address to send from", addr)
	}
	return zone, prefix, err
}

const identityCols = `i.id, i.account_id, i.zone, coalesce(i.prefix, ''), i.display_name, i.signature,
	i.plain_strip_markup, i.transport, i.owner_id, p.kind, p.name, i.created_at, i.disabled_at`

const identityFrom = ` FROM identities i JOIN principals p ON p.id = i.owner_id`

func scanIdentity(row pgx.Row) (Identity, error) {
	var i Identity
	err := row.Scan(&i.ID, &i.AccountID, &i.Zone, &i.Prefix, &i.DisplayName, &i.Signature,
		&i.PlainStripMarkup, &i.Transport, &i.OwnerID, &i.OwnerKind, &i.OwnerName, &i.CreatedAt, &i.DisabledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return i, ErrNotFound
	}
	return i, err
}

// AddIdentity inserts i and records a. Zone and Prefix must be lowercase
// (SplitIdentityAddress). It returns ErrDuplicate when an enabled
// identity has the same address.
func (db *DB) AddIdentity(ctx context.Context, i Identity, a Action) error {
	if i.Zone == "" || i.Zone != strings.ToLower(i.Zone) || i.Prefix != strings.ToLower(i.Prefix) {
		return fmt.Errorf("add identity %s: zone and prefix must be lowercase and the zone non-empty", i.Address())
	}
	return pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if err := refuseRulePrincipal(ctx, tx, i.OwnerID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO identities (id, account_id, zone, prefix, display_name, signature, plain_strip_markup,
			                        transport, owner_id, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			i.ID, i.AccountID, i.Zone, nullable(i.Prefix), i.DisplayName, i.Signature, i.PlainStripMarkup,
			i.Transport, i.OwnerID, i.CreatedAt)
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
			return ErrDuplicate
		}
		if err != nil {
			return fmt.Errorf("add identity %s: %w", i.Address(), err)
		}
		return insertAction(ctx, tx, a)
	})
}

// Identities lists identities by address, disabled ones too when all.
func (db *DB) Identities(ctx context.Context, all bool) ([]Identity, error) {
	rows, err := db.pool.Query(ctx, `SELECT `+identityCols+identityFrom+`
		WHERE $1 OR i.disabled_at IS NULL
		ORDER BY i.zone, i.prefix NULLS FIRST, i.created_at`, all)
	if err != nil {
		return nil, fmt.Errorf("list identities: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Identity, error) { return scanIdentity(r) })
	if err != nil {
		return nil, fmt.Errorf("list identities: %w", err)
	}
	return out, nil
}

// IdentityByID returns an identity, disabled or not.
func (db *DB) IdentityByID(ctx context.Context, id string) (Identity, error) {
	return identityByID(ctx, db.pool, id)
}

func identityByID(ctx context.Context, q querier, id string) (Identity, error) {
	i, err := scanIdentity(q.QueryRow(ctx, `SELECT `+identityCols+identityFrom+` WHERE i.id = $1`, id))
	if err != nil {
		return i, fmt.Errorf("identity %s: %w", id, err)
	}
	return i, nil
}

// IdentityAt returns the enabled identity whose address is exactly
// zone/prefix (prefix "" for the pattern), without falling back from an
// address to its zone's pattern; for `yuuka identity rm` and grants,
// which name an identity, not an address to send from.
func (db *DB) IdentityAt(ctx context.Context, zone, prefix string) (Identity, error) {
	i, err := scanIdentity(db.pool.QueryRow(ctx, `SELECT `+identityCols+identityFrom+`
		WHERE i.zone = $1 AND i.prefix IS NOT DISTINCT FROM $2 AND i.disabled_at IS NULL`, zone, nullable(prefix)))
	if err != nil {
		return i, fmt.Errorf("identity %s: %w", Identity{Zone: zone, Prefix: prefix}.Address(), err)
	}
	return i, nil
}

// DisableIdentity disables the enabled identity id and records a. Drafts,
// outbox rows and grants keep pointing at it; nothing can be sent as it
// any more (sendpolicy refuses a disabled identity, and resolution skips
// it). ErrNotFound if there is no such enabled identity.
func (db *DB) DisableIdentity(ctx context.Context, id string, a Action) error {
	return pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE identities SET disabled_at = $2 WHERE id = $1 AND disabled_at IS NULL`, id, a.At)
		if err != nil {
			return fmt.Errorf("disable identity %s: %w", id, err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return insertAction(ctx, tx, a)
	})
}

// ResolveIdentity returns the identity that sending from addr goes out
// as, and the From address in its stored (lowercase) form. An exact
// identity beats the *@zone pattern of its zone (§9.3), so a grant on the
// pattern never reaches an address someone owns explicitly; disabled
// identities are skipped. ErrNoIdentity if none covers addr.
func (db *DB) ResolveIdentity(ctx context.Context, addr string) (Identity, string, error) {
	return resolveIdentity(ctx, db.pool, addr)
}

func resolveIdentity(ctx context.Context, q querier, addr string) (Identity, string, error) {
	zone, prefix, err := splitSendAddress(addr)
	if err != nil {
		return Identity{}, "", err
	}
	i, err := scanIdentity(q.QueryRow(ctx, `SELECT `+identityCols+identityFrom+`
		WHERE i.zone = $1 AND (i.prefix = $2 OR i.prefix IS NULL) AND i.disabled_at IS NULL
		ORDER BY i.prefix IS NULL LIMIT 1`, zone, prefix))
	if errors.Is(err, ErrNotFound) {
		return i, "", fmt.Errorf("%s: %w", addr, ErrNoIdentity)
	}
	if err != nil {
		return i, "", fmt.Errorf("resolve identity %s: %w", addr, err)
	}
	return i, prefix + "@" + zone, nil
}

// ReplyIdentity is one identity a reply to a message could go out as.
type ReplyIdentity struct {
	Identity Identity
	// FromAddr is the concrete From: the delivery's address for a *@zone
	// identity.
	FromAddr string
	// Via says how the message reached us: the delivery id, or the outbox
	// id for mail we sent ourselves.
	Via string
	At  time.Time
}

// ReplyIdentities returns the identities a reply to message id would
// default to, best first, as v sees the message (§13.8). The default is
// the address the mail came in through (§9.3), never a global one: each
// visible delivery's (account, zone, prefix) resolves like
// ResolveIdentity, within the delivery's account; for mail we sent, the
// identity it went out as. Deliveries no enabled identity covers add
// nothing, so the result may be empty.
//
// A message delivered to several of our addresses (To one, Cc another)
// has several candidates. They are ordered by
//
//  1. identities v's principal owns: an agent CC'd next to the operator
//     answers as itself, and the operator as themselves, instead of
//     landing on the other's address and being held by the owner cap;
//  2. exact identities before *@zone ones: an address someone configured
//     says more than a catch-all;
//  3. the earliest delivery, then the address, so the choice is stable.
//
// ErrNotFound if the message does not exist or v cannot see it.
func (db *DB) ReplyIdentities(ctx context.Context, v *auth.Viewer, id string) ([]ReplyIdentity, error) {
	if err := needViewer(v, "reply identities"); err != nil {
		return nil, err
	}
	var out []ReplyIdentity
	err := pgx.BeginTxFunc(ctx, db.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var mid string
		err := tx.QueryRow(ctx, `SELECT m.id FROM messages m
			WHERE m.id = coalesce((SELECT new_id FROM message_aliases WHERE old_id = $1), $1)
			  AND `+db.visibleMessage(v, "m"), id).Scan(&mid)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		// Each delivery resolves to at most one identity: the exact one
		// if it exists, else its zone's pattern.
		rows, err := tx.Query(ctx, `
			SELECT `+identityCols+`, d.prefix || '@' || d.zone, d.id, d.received_at
			FROM deliveries d
			CROSS JOIN LATERAL (
				SELECT * FROM identities x
				WHERE x.account_id = d.account_id AND x.zone = d.zone
				  AND (x.prefix = d.prefix OR x.prefix IS NULL) AND x.disabled_at IS NULL
				ORDER BY x.prefix IS NULL LIMIT 1) i
			JOIN principals p ON p.id = i.owner_id
			WHERE d.message_id = $1 AND `+db.visibleDelivery(v, "d")+`
			UNION ALL
			SELECT `+identityCols+`, o.from_addr, o.id, o.created_at
			FROM messages m
			CROSS JOIN LATERAL (SELECT so.identity_id, so.from_addr, so.id, so.created_at `+db.visibleOutbox(v, "m")+`) o
			JOIN identities i ON i.id = o.identity_id AND i.disabled_at IS NULL
			JOIN principals p ON p.id = i.owner_id
			WHERE m.id = $1`, mid)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (ReplyIdentity, error) {
			var c ReplyIdentity
			i := &c.Identity
			err := r.Scan(&i.ID, &i.AccountID, &i.Zone, &i.Prefix, &i.DisplayName, &i.Signature,
				&i.PlainStripMarkup, &i.Transport, &i.OwnerID, &i.OwnerKind, &i.OwnerName, &i.CreatedAt, &i.DisabledAt,
				&c.FromAddr, &c.Via, &c.At)
			return c, err
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("reply identities of %s: %w", id, err)
	}
	slices.SortStableFunc(out, func(a, b ReplyIdentity) int {
		if ao, bo := a.Identity.OwnerID == v.PrincipalID, b.Identity.OwnerID == v.PrincipalID; ao != bo {
			if ao {
				return -1
			}
			return 1
		}
		if ae, be := a.Identity.Prefix != "", b.Identity.Prefix != ""; ae != be {
			if ae {
				return -1
			}
			return 1
		}
		if c := a.At.Compare(b.At); c != 0 {
			return c
		}
		return strings.Compare(a.FromAddr, b.FromAddr)
	})
	// One candidate per (identity, From), at its best place.
	seen := map[[2]string]bool{}
	return slices.DeleteFunc(out, func(c ReplyIdentity) bool {
		k := [2]string{c.Identity.ID, c.FromAddr}
		dup := seen[k]
		seen[k] = true
		return dup
	}), nil
}
