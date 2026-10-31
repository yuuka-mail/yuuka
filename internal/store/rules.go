package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/rules"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Rule storage (§8.5, migration 0009). The format and its meaning belong
// to internal/rules; the store validates on save, keeps the canonical
// form of what it parsed, and gives every rule a principal rule:<id>
// that is the actor of everything the rule does. Rules are configuration
// the operator keeps, not mail-derived data, so their reads take no
// viewer.

// Rule is a row of rules.
type Rule struct {
	ID          string
	PrincipalID string
	Name        string
	Priority    int
	Spec        rules.Spec
	Enabled     bool
	Revision    int
	ProposalID  string // "" when not from a proposal
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   *time.Time
	// Unsupported lists why this binary cannot evaluate the stored rule
	// (rules.Program.Unsupported); such a rule never matches.
	Unsupported []string
}

// NewRule is a rule to create.
type NewRule struct {
	Name     string
	Priority int
	// Spec is {"v":1,"match":…,"actions":…}.
	Spec     []byte
	Disabled bool
}

// RuleUpdate changes the fields that are set.
type RuleUpdate struct {
	Name     *string
	Priority *int
	Spec     []byte // nil: keep
	Enabled  *bool
}

// ErrRulePrincipal: a rule principal was given something only principals
// that act on their own may hold, a credential or an identity grant. A
// rule is a standing capability nobody re-approves; it must never be
// able to send (§8.5).
var ErrRulePrincipal = errors.New("rule principals hold no credentials, identities or grants")

// refuseRulePrincipal returns ErrRulePrincipal if principal is a rule.
func refuseRulePrincipal(ctx context.Context, q querier, principal string) error {
	var isRule bool
	err := q.QueryRow(ctx, `SELECT kind = 'rule' FROM principals WHERE id = $1`, principal).Scan(&isRule)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("principal %s: %w", principal, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("principal %s: %w", principal, err)
	}
	if isRule {
		return ErrRulePrincipal
	}
	return nil
}

// ParseRuleSpec decodes and validates a rule for saving, returning its
// warnings. Errors wrap rules.ErrInvalid.
func ParseRuleSpec(b []byte) (rules.Spec, []rules.Warning, error) {
	s, err := rules.ParseSpec(b)
	if err != nil {
		return s, nil, err
	}
	warns, err := s.Validate()
	return s, warns, err
}

const ruleCols = `id, principal_id, name, priority, match, actions, enabled, revision, coalesce(proposal_id, ''),
	created_by, created_at, updated_at, deleted_at`

func scanRule(row pgx.Row) (Rule, error) {
	var r Rule
	var match, actions []byte
	err := row.Scan(&r.ID, &r.PrincipalID, &r.Name, &r.Priority, &match, &actions, &r.Enabled, &r.Revision,
		&r.ProposalID, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt, &r.DeletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	p := rules.Load(match, actions)
	r.Unsupported = p.Unsupported()
	// For display; a rule that does not decode keeps an empty match.
	r.Spec.Match, _ = rules.ParseMatch(match)
	r.Spec.Actions, _ = rules.ParseActions(actions)
	return r, nil
}

// Rules lists the rules in evaluation order (priority, id); deleted ones
// too when all.
func (db *DB) Rules(ctx context.Context, all bool) ([]Rule, error) {
	rows, err := db.pool.Query(ctx, `SELECT `+ruleCols+` FROM rules
		WHERE $1 OR deleted_at IS NULL ORDER BY priority, id`, all)
	if err != nil {
		return nil, fmt.Errorf("list rules: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Rule, error) { return scanRule(r) })
	if err != nil {
		return nil, fmt.Errorf("list rules: %w", err)
	}
	return out, nil
}

// RuleByID returns rule id, deleted or not.
func (db *DB) RuleByID(ctx context.Context, id string) (Rule, error) {
	r, err := scanRule(db.pool.QueryRow(ctx, `SELECT `+ruleCols+` FROM rules WHERE id = $1`, id))
	if err != nil {
		return r, fmt.Errorf("rule %s: %w", id, err)
	}
	return r, nil
}

// checkRegexBudget checks the regexes of every enabled rule but except,
// plus m, against rules.MaxRegexTotal. Stored rules that no longer
// decode count for nothing: they never run.
func checkRegexBudget(ctx context.Context, tx pgx.Tx, except string, m rules.Match) error {
	rows, err := tx.Query(ctx, `SELECT match FROM rules WHERE enabled AND id <> $1`, except)
	if err != nil {
		return fmt.Errorf("regex budget: %w", err)
	}
	ms := []rules.Match{m}
	var b []byte
	_, err = pgx.ForEachRow(rows, []any{&b}, func() error {
		if other, err := rules.ParseMatch(b); err == nil {
			ms = append(ms, other)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("regex budget: %w", err)
	}
	return rules.CheckRegexBudget(ms...)
}

// marshalSpec is the canonical JSON of a parsed spec's two columns.
func marshalSpec(s rules.Spec) (match, actions []byte, err error) {
	if match, err = json.Marshal(s.Match); err != nil {
		return nil, nil, err
	}
	if actions, err = json.Marshal(s.Actions); err != nil {
		return nil, nil, err
	}
	return match, actions, nil
}

// CreateRule validates n, creates the rule and its principal rule:<id>,
// and records a, whose actor becomes created_by. It returns the
// validation warnings. Errors from validation wrap rules.ErrInvalid.
func (db *DB) CreateRule(ctx context.Context, n NewRule, a Action) (Rule, []rules.Warning, error) {
	spec, warns, err := ParseRuleSpec(n.Spec)
	if err != nil {
		return Rule{}, nil, err
	}
	var r Rule
	err = pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, contentLock); err != nil {
			return err
		}
		w := writer{tx: tx}
		id, err := createRule(ctx, tx, &w, n, spec, "", a)
		if err != nil {
			return err
		}
		a.Args = withArg(a.Args, "rule", id)
		if err := insertAction(ctx, tx, a); err != nil {
			return err
		}
		if err := w.flush(ctx); err != nil {
			return err
		}
		r, err = scanRule(tx.QueryRow(ctx, `SELECT `+ruleCols+` FROM rules WHERE id = $1`, id))
		return err
	})
	if err != nil {
		return Rule{}, nil, fmt.Errorf("create rule: %w", err)
	}
	return r, warns, nil
}

// createRule inserts a validated rule and its principal; the caller
// holds contentLock and records a. a.Args gains the rule id.
func createRule(ctx context.Context, tx pgx.Tx, w *writer, n NewRule, spec rules.Spec, proposal string, a Action) (string, error) {
	match, actions, err := marshalSpec(spec)
	if err != nil {
		return "", err
	}
	if !n.Disabled {
		if err := checkRegexBudget(ctx, tx, "", spec.Match); err != nil {
			return "", err
		}
	}
	id := ulid.New(a.At)
	principal := ulid.New(a.At)
	display := "rule " + id
	if n.Name != "" {
		display = n.Name
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO principals (id, kind, name, display_name, created_at) VALUES ($1, 'rule', $2, $3, $4)`,
		principal, id, display, a.At); err != nil {
		return "", fmt.Errorf("rule principal: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO rules (id, principal_id, name, priority, version, match, actions, enabled, proposal_id,
		                   created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11)`,
		id, principal, n.Name, n.Priority, rules.Version, match, actions, !n.Disabled, nullable(proposal),
		a.Actor, a.At); err != nil {
		return "", fmt.Errorf("insert rule: %w", err)
	}
	w.change("rule", id)
	return id, nil
}

// UpdateRule changes rule id as u says, bumping its revision, and records
// a. A new spec is validated as on create; enabling a rule or changing
// an enabled one rechecks the regex budget. ErrNotFound for a deleted
// rule.
func (db *DB) UpdateRule(ctx context.Context, id string, u RuleUpdate, a Action) (Rule, []rules.Warning, error) {
	var warns []rules.Warning
	var spec *rules.Spec
	if u.Spec != nil {
		s, w, err := ParseRuleSpec(u.Spec)
		if err != nil {
			return Rule{}, nil, err
		}
		spec, warns = &s, w
	}
	var r Rule
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, contentLock); err != nil {
			return err
		}
		cur, err := scanRule(tx.QueryRow(ctx, `SELECT `+ruleCols+` FROM rules WHERE id = $1 AND deleted_at IS NULL`, id))
		if err != nil {
			return err
		}
		next := cur
		if u.Name != nil {
			next.Name = *u.Name
		}
		if u.Priority != nil {
			next.Priority = *u.Priority
		}
		if u.Enabled != nil {
			next.Enabled = *u.Enabled
		}
		match, actions, err := func() ([]byte, []byte, error) {
			if spec != nil {
				next.Spec = *spec
				return marshalSpec(*spec)
			}
			var m, as []byte
			err := tx.QueryRow(ctx, `SELECT match, actions FROM rules WHERE id = $1`, id).Scan(&m, &as)
			return m, as, err
		}()
		if err != nil {
			return err
		}
		if next.Enabled && (spec != nil || !cur.Enabled) {
			if len(cur.Unsupported) > 0 && spec == nil {
				return fmt.Errorf("%w: the stored rule cannot be enabled by this version: %v", rules.ErrInvalid, cur.Unsupported)
			}
			if err := checkRegexBudget(ctx, tx, id, next.Spec.Match); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE rules SET name = $2, priority = $3, match = $4, actions = $5, enabled = $6,
				revision = revision + 1, updated_at = $7
			WHERE id = $1`, id, next.Name, next.Priority, match, actions, next.Enabled, a.At); err != nil {
			return fmt.Errorf("update rule: %w", err)
		}
		if u.Name != nil && *u.Name != "" {
			if _, err := tx.Exec(ctx, `UPDATE principals SET display_name = $2 WHERE id = $1`, cur.PrincipalID, *u.Name); err != nil {
				return err
			}
		}
		a.Args = withArg(a.Args, "rule", id)
		if err := insertAction(ctx, tx, a); err != nil {
			return err
		}
		w := writer{tx: tx}
		w.change("rule", id)
		if err := w.flush(ctx); err != nil {
			return err
		}
		r, err = scanRule(tx.QueryRow(ctx, `SELECT `+ruleCols+` FROM rules WHERE id = $1`, id))
		return err
	})
	if err != nil {
		return Rule{}, nil, fmt.Errorf("update rule %s: %w", id, err)
	}
	return r, warns, nil
}

// DeleteRule soft-deletes rule id: it stops running, and its principal
// is disabled, while the history that names it stays. Its effects stay
// too; RevertRule takes them back. ErrNotFound if already deleted.
func (db *DB) DeleteRule(ctx context.Context, id string, a Action) error {
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, contentLock); err != nil {
			return err
		}
		var principal string
		err := tx.QueryRow(ctx, `
			UPDATE rules SET enabled = false, deleted_at = $2, updated_at = $2, revision = revision + 1
			WHERE id = $1 AND deleted_at IS NULL RETURNING principal_id`, id, a.At).Scan(&principal)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE principals SET disabled_at = $2 WHERE id = $1`, principal, a.At); err != nil {
			return err
		}
		a.Args = withArg(a.Args, "rule", id)
		if err := insertAction(ctx, tx, a); err != nil {
			return err
		}
		w := writer{tx: tx}
		w.change("rule", id)
		return w.flush(ctx)
	})
	if err != nil {
		return fmt.Errorf("delete rule %s: %w", id, err)
	}
	return nil
}

// programCache holds compiled rules by (id, revision): a revision is
// immutable, so an entry is valid until the rule's revision moves on.
type programCache struct {
	mu    sync.Mutex
	progs map[string]cachedProgram
}

type cachedProgram struct {
	revision int
	prog     *rules.Program
}

// activeRule is an enabled rule as ingest needs it.
type activeRule struct {
	id, principal string
	revision      int
	priority      int
	prog          *rules.Program
}

// enabledRules loads the enabled rules, compiling only those whose
// revision the cache has not seen.
func (db *DB) enabledRules(ctx context.Context, tx pgx.Tx) ([]activeRule, error) {
	rows, err := tx.Query(ctx, `SELECT id, principal_id, revision, priority FROM rules WHERE enabled`)
	if err != nil {
		return nil, fmt.Errorf("enabled rules: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (activeRule, error) {
		var a activeRule
		return a, r.Scan(&a.id, &a.principal, &a.revision, &a.priority)
	})
	if err != nil {
		return nil, fmt.Errorf("enabled rules: %w", err)
	}
	db.progs.mu.Lock()
	defer db.progs.mu.Unlock()
	if db.progs.progs == nil {
		db.progs.progs = map[string]cachedProgram{}
	}
	for i := range out {
		r := &out[i]
		if c, ok := db.progs.progs[r.id]; ok && c.revision == r.revision {
			r.prog = c.prog
			continue
		}
		var match, actions []byte
		if err := tx.QueryRow(ctx, `SELECT match, actions FROM rules WHERE id = $1`, r.id).Scan(&match, &actions); err != nil {
			return nil, fmt.Errorf("load rule %s: %w", r.id, err)
		}
		r.prog = rules.Load(match, actions)
		db.progs.progs[r.id] = cachedProgram{r.revision, r.prog}
	}
	return out, nil
}
