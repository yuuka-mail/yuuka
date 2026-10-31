package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/leak"
)

// Leak detection (§3 principle 3) is computed when mail is read, like the
// §4.3 downgrade, from facts the store keeps anyway: the trusted
// receiver's DMARC result in deliveries.auth, the From domain in
// messages.from_addr and the operator's prefixes.expected_domains.
// Nothing is stored, for three reasons:
//
//   - the observed domains are derived (both inputs are), so a stored
//     copy would be one more thing reparse has to rebuild;
//   - editing the expected domains takes effect on the next read, with no
//     backfill;
//   - visibility (§13.8): a domain is observed by a viewer only through a
//     delivery that viewer may read. A column on prefixes would show every
//     viewer the senders of mail in accounts it cannot read.
//
// prefixes.observed_domains and prefixes.leak_suspect (0002) therefore
// stay unwritten. The organizational domain needs the Public Suffix List,
// which lives in Go (internal/leak): SQL narrows the deliveries to the
// few distinct (DMARC result, From domain) pairs per prefix, and Go
// judges those.

// senderCols are, for a delivery d of message m: the trusted receiver's
// DMARC result ("" without one), the header.from it judged (of the first
// dmarc result, as authres.Auth.DMARC is), and the From domain of m.
// leak.NewSender takes them in this order.
const senderCols = `CASE WHEN coalesce((d.auth->>'trusted')::boolean, false) THEN coalesce(d.auth->>'dmarc', '') ELSE '' END,
	coalesce((SELECT r.e->'props'->>'header.from'
		FROM jsonb_array_elements(CASE jsonb_typeof(d.auth->'results') WHEN 'array' THEN d.auth->'results' ELSE '[]'::jsonb END)
			WITH ORDINALITY r(e, n)
		WHERE r.e->>'method' = 'dmarc' ORDER BY r.n LIMIT 1), ''),
	CASE WHEN strpos(m.from_addr, '@') > 0 THEN lower(regexp_replace(m.from_addr, '^.*@', '')) ELSE '' END`

// judged is the condition on a delivery d (joined with its prefix row p)
// that the leak check looks at: to a claimed prefix. Unclaimed prefixes
// collect dictionary attacks. To a claimed prefix the only downgraded
// mail is a DMARC failure, which leak.Judge counts when it forges an
// expected sender (a spoof, still downgraded) and ignores otherwise.
const judged = `(p.state = 1)`

// ObservedDomain is one organizational domain that DMARC proved to have
// sent mail to a prefix.
type ObservedDomain struct {
	Domain   string
	Messages int
	LastSeen time.Time
	// Expected is whether the prefix expects mail from it.
	Expected bool
}

// LeakStats is the leak check of a claimed prefix over the mail a viewer
// may read: all zero for a discovered prefix.
type LeakStats struct {
	// Observed are the DMARC-verified sender domains, most mail first.
	Observed []ObservedDomain
	// Leaks counts messages from senders outside ExpectedDomains, of
	// which Unverified proved nothing about their sender and so are
	// attributed to no domain. Spoofs counts messages whose From claims
	// an expected domain without DMARC passing. All three stay zero
	// while no domains are expected. A message delivered to the prefix
	// twice with different results counts once for each.
	Leaks, Spoofs, Unverified int
	// LastOffense is when the latest leak or spoof arrived.
	LastOffense *time.Time
}

// Suspect is whether anything to the prefix leaked or was spoofed.
func (l LeakStats) Suspect() bool { return l.Leaks+l.Spoofs > 0 }

// judgePrefixes fills the LeakStats of the claimed prefixes in ps.
func (db *DB) judgePrefixes(ctx context.Context, tx pgx.Tx, v *auth.Viewer, ps []PrefixStat) error {
	type key struct{ zone, prefix string }
	at := map[key]*PrefixStat{}
	var zones, prefixes []string
	for i := range ps {
		if p := &ps[i]; p.State == PrefixClaimed {
			at[key{p.Zone, p.Prefix}] = p
			zones, prefixes = append(zones, p.Zone), append(prefixes, p.Prefix)
		}
	}
	if len(at) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
		SELECT d.zone, d.prefix, `+senderCols+`, count(DISTINCT d.message_id), max(d.received_at)
		FROM deliveries d JOIN prefixes p ON p.zone = d.zone AND p.prefix = d.prefix
		JOIN messages m ON m.id = d.message_id
		WHERE (d.zone, d.prefix) IN (SELECT * FROM unnest($1::text[], $2::text[]))
			AND `+judged+` AND `+db.visibleDelivery(v, "d")+`
		GROUP BY 1, 2, 3, 4, 5`, zones, prefixes)
	if err != nil {
		return fmt.Errorf("leak check: %w", err)
	}
	observed := map[key]map[string]*ObservedDomain{}
	var k key
	var dmarc string
	var headerFrom, fromDomain string
	var n int
	var last time.Time
	_, err = pgx.ForEachRow(rows, []any{&k.zone, &k.prefix, &dmarc, &headerFrom, &fromDomain, &n, &last}, func() error {
		p := at[k]
		s := leak.NewSender(dmarc, headerFrom, fromDomain)
		if s.Verified != "" {
			if observed[k] == nil {
				observed[k] = map[string]*ObservedDomain{}
			}
			o := observed[k][s.Verified]
			if o == nil {
				o = &ObservedDomain{Domain: s.Verified, Expected: leak.IsExpected(p.ExpectedDomains, s.Verified)}
				observed[k][s.Verified] = o
			}
			o.Messages += n
			o.LastSeen = latest(o.LastSeen, last)
		}
		j := leak.Judge(p.ExpectedDomains, s)
		switch j.Verdict {
		case leak.Leak:
			p.Leaks += n
			if j.Domain == "" {
				p.Unverified += n
			}
		case leak.Spoof:
			p.Spoofs += n
		default:
			return nil
		}
		if p.LastOffense == nil || last.After(*p.LastOffense) {
			t := last
			p.LastOffense = &t
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("leak check: %w", err)
	}
	for k, ds := range observed {
		p := at[k]
		for _, o := range ds {
			p.Observed = append(p.Observed, *o)
		}
		slices.SortFunc(p.Observed, func(a, b ObservedDomain) int {
			return cmp.Or(cmp.Compare(b.Messages, a.Messages), cmp.Compare(a.Domain, b.Domain))
		})
	}
	return nil
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// Offense is one message that leaked or spoofed, and the judgement of
// its delivery to the prefix.
type Offense struct {
	Message MessageSummary
	leak.Judgement
	// DeliveredAt is when that delivery arrived.
	DeliveredAt time.Time
}

// PrefixDetail is one prefix with its leak check and the latest mail
// that offended against it.
type PrefixDetail struct {
	PrefixStat
	// Offenses are the latest leaks and spoofs, newest first, at most the
	// limit PrefixByAddress was given.
	Offenses []Offense
}

// PrefixByAddress returns prefix@zone (lowercase, as ingest stores them)
// as v sees it, with up to limit of the latest offending messages.
// ErrNotFound if it is not in the registry.
func (db *DB) PrefixByAddress(ctx context.Context, v *auth.Viewer, zone, prefix string, limit int) (PrefixDetail, error) {
	if err := needViewer(v, "prefix"); err != nil {
		return PrefixDetail{}, err
	}
	var out PrefixDetail
	err := pgx.BeginTxFunc(ctx, db.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		sum, err := db.prefixStats(ctx, tx, v, PrefixQuery{Zone: zone, Prefix: prefix})
		if err != nil {
			return err
		}
		if len(sum.Prefixes) == 0 {
			return ErrNotFound
		}
		out.PrefixStat = sum.Prefixes[0]
		out.Offenses = []Offense{}
		if out.State != PrefixClaimed || len(out.ExpectedDomains) == 0 || limit <= 0 {
			return nil
		}
		out.Offenses, err = db.offenses(ctx, tx, v, out.PrefixStat, limit)
		return err
	})
	if err != nil {
		return PrefixDetail{}, fmt.Errorf("prefix %s@%s: %w", prefix, zone, err)
	}
	return out, nil
}

// errEnough stops a row loop early.
var errEnough = errors.New("enough rows")

// offenses judges the visible deliveries to p newest first until it has
// limit offending messages, then reads those as list rows.
func (db *DB) offenses(ctx context.Context, tx pgx.Tx, v *auth.Viewer, p PrefixStat, limit int) ([]Offense, error) {
	rows, err := tx.Query(ctx, `
		SELECT d.message_id, d.received_at, `+senderCols+`
		FROM deliveries d JOIN prefixes p ON p.zone = d.zone AND p.prefix = d.prefix
		JOIN messages m ON m.id = d.message_id
		WHERE d.zone = $1 AND d.prefix = $2 AND `+judged+` AND `+db.visibleDelivery(v, "d")+`
		ORDER BY d.received_at DESC, d.id DESC`, p.Zone, p.Prefix)
	if err != nil {
		return nil, err
	}
	var out []Offense
	var ids []string
	var id string
	var at time.Time
	var dmarc string
	var headerFrom, fromDomain string
	_, err = pgx.ForEachRow(rows, []any{&id, &at, &dmarc, &headerFrom, &fromDomain}, func() error {
		if len(out) == limit {
			return errEnough
		}
		if slices.Contains(ids, id) {
			return nil
		}
		j := leak.Judge(p.ExpectedDomains, leak.NewSender(dmarc, headerFrom, fromDomain))
		if j.Verdict == leak.Leak || j.Verdict == leak.Spoof {
			out = append(out, Offense{Judgement: j, DeliveredAt: at, Message: MessageSummary{ID: id}})
			ids = append(ids, id)
		}
		return nil
	})
	if errors.Is(err, errEnough) {
		err = nil
	}
	if err != nil || len(out) == 0 {
		return []Offense{}, err
	}
	rows, err = tx.Query(ctx, `SELECT `+summaryCols+db.summaryFrom(v)+` AND m.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	ms, err := pgx.CollectRows(rows, scanSummary)
	if err != nil {
		return nil, err
	}
	for i := range out {
		j := slices.IndexFunc(ms, func(m MessageSummary) bool { return m.ID == out[i].Message.ID })
		if j < 0 {
			return nil, fmt.Errorf("offending message %s vanished", out[i].Message.ID)
		}
		out[i].Message = ms[j]
	}
	return out, nil
}

// ErrNotClaimed is returned for a decision that only a claimed prefix
// can carry.
var ErrNotClaimed = errors.New("prefix is not claimed")

// SetPrefixExpected is the operator's decision of who prefix@zone was
// handed out to (§3 principle 3): it replaces the expected domains with
// domains, normalized by leak.Expected, and returns what was stored.
// Only a claimed prefix can expect anything: leak detection ignores
// discovered ones. It is recorded as a with the zone, prefix and domains
// added to its Args (nil or a map[string]any), and logged in changes;
// changed is false, and nothing is written, when the domains were
// already those.
//
// Nothing is rewritten: the leak check is computed when mail is read, so
// the new expectation judges the mail already delivered too.
func (db *DB) SetPrefixExpected(ctx context.Context, zone, prefix string, domains []string, a Action) (stored []string, changed bool, err error) {
	stored, err = leak.Expected(domains)
	if err != nil {
		return nil, false, err
	}
	extra, ok := a.Args.(map[string]any)
	if a.Args != nil && !ok {
		return nil, false, fmt.Errorf("prefix action args: want a map, got %T", a.Args)
	}
	err = pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		// As SetPrefixState: changes.seq stays in commit order.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, contentLock); err != nil {
			return err
		}
		var state int
		var cur []string
		err := tx.QueryRow(ctx, `SELECT state, expected_domains FROM prefixes WHERE zone = $1 AND prefix = $2 FOR UPDATE`,
			zone, prefix).Scan(&state, &cur)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return err
		case state != PrefixClaimed:
			return ErrNotClaimed
		case slices.Equal(cur, stored):
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE prefixes SET expected_domains = $3 WHERE zone = $1 AND prefix = $2`,
			zone, prefix, stored); err != nil {
			return err
		}
		changed = true
		w := writer{tx: tx}
		w.change("prefix", prefix+"@"+zone)
		if err := w.flush(ctx); err != nil {
			return err
		}
		args := map[string]any{"zone": zone, "prefix": prefix, "expected_domains": stored, "was": cur}
		for k, v := range extra {
			args[k] = v
		}
		a.Args = args
		return insertAction(ctx, tx, a)
	})
	if err != nil {
		return nil, false, fmt.Errorf("prefix %s@%s: %w", prefix, zone, err)
	}
	return stored, changed, nil
}
