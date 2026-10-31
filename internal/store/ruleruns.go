package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/classify"
	"github.com/yuuka-mail/yuuka/internal/leak"
	"github.com/yuuka-mail/yuuka/internal/rules"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Where rules run (§8.5). There is one evaluator (internal/rules) and
// one loader of what it sees (loadFacts); ingest, the retroactive runs
// and the preview all use both.
//
//   - Ingest: Fold evaluates every enabled rule on the new delivery,
//     inside its transaction, after the message, its auth facts and its
//     extractions are written, so no delivery is ever committed with its
//     rules not run. The backlog pass folds through Fold too; Rebuild
//     (reparse, the backfills of new columns) never runs rules: their
//     effects were decisions made on the facts at arrival.
//   - ApplyRule and RevertRule: one rule over past mail, on a person's
//     explicit request, in short transactions of retroPage messages.
//     They are idempotent, so an interrupted run is finished by running
//     it again. They never notify.
//   - PreviewRule: read only, over what the viewer may see.
//
// Rules match inbound deliveries only: a sent copy has none, so our own
// mail is never matched.

// ruleDeadline bounds evaluating the rules on one message; rules not
// reached by then fail closed (rules.Combine).
const ruleDeadline = 3 * time.Second

// retroPage is how many messages one transaction of a retroactive run
// covers: short enough not to hold contentLock against ingest for long.
const retroPage = 200

// loadFacts returns what the rules see of each message in ids: one
// rules.Facts per delivery that delCond (a condition on deliveries d,
// whose parameters start at $2) selects, earliest first. Messages
// without such a delivery are absent.
func loadFacts(ctx context.Context, tx pgx.Tx, ids []string, delCond string, args ...any) (map[string][]rules.Facts, error) {
	type content struct {
		from, subject, body, listID string
		class                       classify.Class
		attach                      bool
		extractions                 []string
	}
	msgs := map[string]*content{}
	rows, err := tx.Query(ctx, `
		SELECT m.id, m.from_addr, m.subject, m.body_text, coalesce(m.list_id, ''),
			coalesce(s.class_override, m.class), m.has_attach,
			coalesce((SELECT array_agg(DISTINCT e.kind ORDER BY e.kind) FROM extractions e WHERE e.message_id = m.id), '{}')
		FROM messages m LEFT JOIN message_state s ON s.message_id = m.id
		WHERE m.id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("rule facts: %w", err)
	}
	var id string
	var c content
	_, err = pgx.ForEachRow(rows, []any{&id, &c.from, &c.subject, &c.body, &c.listID, &c.class, &c.attach, &c.extractions}, func() error {
		cc := c
		msgs[id] = &cc
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("rule facts: %w", err)
	}
	rows, err = tx.Query(ctx, `
		SELECT d.message_id, d.zone, d.prefix, d.auth, p.state, p.expected_domains
		FROM deliveries d JOIN prefixes p ON p.zone = d.zone AND p.prefix = d.prefix
		WHERE d.message_id = ANY($1) AND `+delCond+`
		ORDER BY d.message_id, d.received_at, d.id`, append([]any{ids}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("rule facts: %w", err)
	}
	out := map[string][]rules.Facts{}
	var zone, prefix string
	var a *authres.Auth
	var state int
	var expected []string
	_, err = pgx.ForEachRow(rows, []any{&id, &zone, &prefix, &a, &state, &expected}, func() error {
		m := msgs[id]
		if m == nil {
			return nil
		}
		// The same definitions the read path uses (leak.SenderOf,
		// leak.Judge): from.verified is DMARC passing for the From the
		// reader sees, and the verdicts are only judged on claimed
		// prefixes.
		sender := leak.SenderOf(a, m.from)
		f := rules.Facts{
			FromAddr: m.from, FromVerified: sender.Verified != "",
			ToPrefix: prefix, ToZone: zone, PrefixClaimed: state == PrefixClaimed,
			Subject: m.subject, Body: m.body, ListID: m.listID,
			Class: m.class, Extractions: m.extractions, Attachment: m.attach,
		}
		if state == PrefixClaimed {
			switch leak.Judge(expected, sender).Verdict {
			case leak.Leak:
				f.Leak = true
			case leak.Spoof:
				f.Spoof = true
			}
		}
		out[id] = append(out[id], f)
		a = nil // Scan would otherwise decode into the previous value
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("rule facts: %w", err)
	}
	return out, nil
}

// annotationsOf turns a rule's actions into the annotations they ask
// for, and the notify level if one is set. Rules only ever add.
func annotationsOf(as []rules.Action) (ins []Annotation, notify string) {
	for _, a := range as {
		switch a.Kind {
		case rules.KindLabel:
			ins = append(ins, SetLabel(a.Label, true))
		case rules.KindArchive:
			ins = append(ins, SetArchived(true))
		case rules.KindClassify:
			c := classify.Machine
			if a.Class == rules.ClassHuman {
				c = classify.Human
			}
			ins = append(ins, SetClass(c))
		case rules.KindNotify:
			notify = a.Level
		}
	}
	return ins, notify
}

// ruleRun is what the rules decided for one new delivery.
type ruleRun struct {
	// notify is the level a rule set (rules.Notify*), or "".
	notify string
	// failures are the rules that failed closed.
	failures []rules.Hit
}

// runRules evaluates the enabled rules on delivery (of message) and
// writes the outcome at rule rank: one actions row (rule.run, actor
// rule:<id>) per rule whose effects changed something or set the notify
// level. The caller holds contentLock.
func (db *DB) runRules(ctx context.Context, tx pgx.Tx, w *writer, message, delivery string) (ruleRun, error) {
	var run ruleRun
	active, err := db.enabledRules(ctx, tx)
	if err != nil || len(active) == 0 {
		return run, err
	}
	facts, err := loadFacts(ctx, tx, []string{message}, `d.id = $2`, delivery)
	if err != nil {
		return run, err
	}
	fs := facts[message]
	if len(fs) != 1 {
		return run, fmt.Errorf("rule facts of delivery %s: found %d", delivery, len(fs))
	}
	rs := make([]rules.Rule, len(active))
	byID := map[string]activeRule{}
	for i, r := range active {
		rs[i] = rules.Rule{ID: r.id, Priority: r.priority, Program: r.prog}
		byID[r.id] = r
	}
	ectx, cancel := context.WithTimeout(ctx, ruleDeadline)
	intent := rules.Combine(ectx, rs, &fs[0])
	cancel()
	for _, h := range intent.Hits {
		if h.Err != nil {
			run.failures = append(run.failures, h)
		}
	}
	now := time.Now()
	for i := 0; i < len(intent.Effects); {
		r := byID[intent.Effects[i].Rule]
		var as []rules.Action
		for ; i < len(intent.Effects) && intent.Effects[i].Rule == r.id; i++ {
			as = append(as, intent.Effects[i].Action)
		}
		ins, notify := annotationsOf(as)
		st, err := loadAnnState(ctx, tx, message)
		if err != nil {
			return run, err
		}
		p := annPlan{st: st, rank: RankRule}
		res := p.plan(ins)
		if notify != "" {
			run.notify = notify
		} else if !p.changed() {
			continue
		}
		args := map[string]any{"rule": r.id, "revision": r.revision, "message": message, "delivery": delivery, "effects": as}
		if notify != "" {
			args["notify"] = notify
		}
		a := Action{ID: ulid.New(now), Actor: r.principal, Tool: "rule.run", Args: args, Result: map[string]any{"outcomes": res}, At: now}
		if p.changed() {
			a.UndoToken = a.ID
		}
		if err := insertAction(ctx, tx, a); err != nil {
			return run, err
		}
		if err := p.write(ctx, tx, w, a.ID); err != nil {
			return run, err
		}
	}
	return run, nil
}

// ruleNotify is the notify level a rule set for delivery when it was
// folded, or "": for a caller that finds the delivery already folded.
func ruleNotify(ctx context.Context, tx pgx.Tx, delivery string, since time.Time) (string, error) {
	var level string
	err := tx.QueryRow(ctx, `
		SELECT args->>'notify' FROM actions
		WHERE at >= $2 AND tool = 'rule.run' AND args->>'delivery' = $1 AND args ? 'notify'
		LIMIT 1`, delivery, since).Scan(&level)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return level, err
}

// applyNotify narrows Folded.Notify by a rule's notify level. A rule can
// silence a notification or make it quiet, never cause one: mail that
// is downgraded (§4.3), or not new to the operator, is not pushed
// whatever a rule says.
func (f *Folded) applyNotify(level string) {
	switch level {
	case rules.NotifyOff:
		f.Notify = false
	case rules.NotifySilent:
		f.Silent = f.Notify
	}
}

// RetroStats counts what a retroactive run of a rule did.
type RetroStats struct {
	// Messages were looked at; Matched of them matched.
	Messages int `json:"messages"`
	Matched  int `json:"matched"`
	// Changed messages got (or, in a dry run, would get) a new value.
	Changed int `json:"changed"`
	// Overruled messages kept something a person or an agent decided.
	Overruled int `json:"overruled"`
	// Failed evaluations ran out of time and did not match.
	Failed int `json:"failed"`
}

// scope is the mail a retroactive run or a preview goes over: messages
// received at or after since with a delivery v may see (nil v: all).
type scope struct {
	db    *DB
	since time.Time
	v     *auth.Viewer
}

func (s scope) delCond() string {
	if s.v == nil {
		return "true"
	}
	return s.db.visibleDelivery(s.v, "d")
}

// page returns the next retroPage message ids before after ("" to
// start), newest first.
func (s scope) page(ctx context.Context, tx pgx.Tx, after string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT m.id FROM messages m
		WHERE m.received_at >= $1 AND ($2 = '' OR m.id < $2)
			AND EXISTS (SELECT 1 FROM deliveries d WHERE d.message_id = m.id AND `+s.delCond()+`)
		ORDER BY m.id DESC LIMIT $3`, s.since, after, retroPage)
	if err != nil {
		return nil, fmt.Errorf("rule scan: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("rule scan: %w", err)
	}
	return ids, nil
}

// matchAny evaluates prog on each delivery's facts: a message matches
// when any of its deliveries does. It reports a deadline failure as
// failed, never as a match.
func matchAny(ctx context.Context, prog *rules.Program, fs []rules.Facts) (matched, failed bool) {
	ectx, cancel := context.WithTimeout(ctx, ruleDeadline)
	defer cancel()
	for i := range fs {
		ok, err := prog.Eval(ectx, &fs[i])
		if err != nil {
			return false, true
		}
		if ok {
			return true, false
		}
	}
	return false, false
}

// retroVisit is called for each message a scan looks at, in its
// transaction, with whether the rule matched.
type retroVisit func(tx pgx.Tx, w *writer, id string, matched bool) error

// scan pages through s, evaluating prog, and calls visit. Each page is
// one transaction: under contentLock when it writes, read only
// otherwise. check runs first in each writing page.
func (s scope) scan(ctx context.Context, prog *rules.Program, write bool, check func(tx pgx.Tx) error, st *RetroStats, visit retroVisit) error {
	after := ""
	for {
		var ids []string
		body := func(tx pgx.Tx) error {
			if write {
				if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, contentLock); err != nil {
					return err
				}
				if err := check(tx); err != nil {
					return err
				}
			}
			var err error
			if ids, err = s.page(ctx, tx, after); err != nil || len(ids) == 0 {
				return err
			}
			facts, err := loadFacts(ctx, tx, ids, s.delCond())
			if err != nil {
				return err
			}
			w := writer{tx: tx}
			for _, id := range ids {
				st.Messages++
				matched, failed := matchAny(ctx, prog, facts[id])
				if failed {
					st.Failed++
				}
				if matched {
					st.Matched++
				}
				if err := visit(tx, &w, id, matched); err != nil {
					return err
				}
			}
			return w.flush(ctx)
		}
		var err error
		if write {
			err = pgx.BeginFunc(ctx, s.db.pool, body)
		} else {
			err = pgx.BeginTxFunc(ctx, s.db.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, body)
		}
		if err != nil {
			return err
		}
		if len(ids) < retroPage {
			return nil
		}
		after = ids[len(ids)-1]
	}
}

// storedProgram loads rule id for a retroactive run: not deleted, and
// evaluable by this version.
func (db *DB) storedProgram(ctx context.Context, id string) (prog *rules.Program, principal string, revision int, err error) {
	var match, actions []byte
	err = db.pool.QueryRow(ctx, `SELECT match, actions, principal_id, revision FROM rules WHERE id = $1 AND deleted_at IS NULL`, id).
		Scan(&match, &actions, &principal, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", 0, ErrNotFound
	}
	if err != nil {
		return nil, "", 0, err
	}
	prog = rules.Load(match, actions)
	if u := prog.Unsupported(); len(u) > 0 {
		return nil, "", 0, fmt.Errorf("%w: %v", rules.ErrUnsupported, u)
	}
	return prog, principal, revision, nil
}

// errRuleChanged: the rule was edited or deleted while a run went over
// past mail; what was done so far stands and running again finishes it.
var errRuleChanged = errors.New("the rule changed during the run; run it again")

// ApplyRule runs rule id over the mail received at or after since, as
// requested by a, which it records with the counts as its result. What
// the rule changes hangs on one rule.apply action of rule:<id>, so
// RevertRule and Undo take it back like ingest's. It respects
// precedence, never notifies (the notify action is ignored), and is
// idempotent. With dry it writes nothing, not even a, and the counts say
// what it would do.
func (db *DB) ApplyRule(ctx context.Context, id string, since time.Time, dry bool, a Action) (RetroStats, error) {
	var st RetroStats
	prog, principal, revision, err := db.storedProgram(ctx, id)
	if err != nil {
		return st, fmt.Errorf("apply rule %s: %w", id, err)
	}
	a.Args = withArg(withArg(a.Args, "rule", id), "since", since)
	if !dry {
		if err := insertAction(ctx, db.pool, a); err != nil {
			return st, err
		}
	}
	ins, _ := annotationsOf(prog.Actions())
	runID := ""
	check := func(tx pgx.Tx) error {
		var rev int
		err := tx.QueryRow(ctx, `SELECT revision FROM rules WHERE id = $1 AND deleted_at IS NULL`, id).Scan(&rev)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && rev != revision {
			return errRuleChanged
		}
		return err
	}
	err = scope{db: db, since: since}.scan(ctx, prog, !dry, check, &st, func(tx pgx.Tx, w *writer, mid string, matched bool) error {
		if !matched || len(ins) == 0 {
			return nil
		}
		as, err := loadAnnState(ctx, tx, mid)
		if err != nil {
			return err
		}
		p := annPlan{st: as, rank: RankRule}
		for _, r := range p.plan(ins) {
			if r.Outcome == Overruled {
				st.Overruled++
				break
			}
		}
		if !p.changed() {
			return nil
		}
		st.Changed++
		if dry {
			return nil
		}
		if runID == "" {
			now := time.Now()
			run := Action{
				ID: ulid.New(now), Actor: principal, Tool: "rule.apply", At: now,
				Args: map[string]any{"rule": id, "revision": revision, "request": a.ID},
			}
			run.UndoToken = run.ID
			if err := insertAction(ctx, tx, run); err != nil {
				return err
			}
			runID = run.ID
		}
		return p.write(ctx, tx, w, runID)
	})
	if !dry {
		// Best effort: the counts so far, also when the run stopped.
		if _, uerr := db.pool.Exec(context.WithoutCancel(ctx), `UPDATE actions SET result = $2 WHERE id = $1`,
			a.ID, map[string]any{"stats": st, "run": runID, "complete": err == nil}); uerr != nil && err == nil {
			err = uerr
		}
	}
	if err != nil {
		return st, fmt.Errorf("apply rule %s: %w", id, err)
	}
	return st, nil
}

// RevertStats counts what RevertRule did.
type RevertStats struct {
	Messages   int `json:"messages"`
	Reverted   int `json:"reverted"`
	Superseded int `json:"superseded"`
}

// RevertRule takes back what rule id did to the mail received at or
// after since, at rule rank, and records a (with the counts as its
// result); the reverts hang on a. A change that someone (or the rule
// itself) changed again since is left as it is now and counted as
// superseded. Deleted rules can be reverted. It is idempotent.
func (db *DB) RevertRule(ctx context.Context, id string, since time.Time, a Action) (RevertStats, error) {
	var st RevertStats
	var principal string
	err := db.pool.QueryRow(ctx, `SELECT principal_id FROM rules WHERE id = $1`, id).Scan(&principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, fmt.Errorf("revert rule %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return st, err
	}
	a.Args = withArg(withArg(a.Args, "rule", id), "since", since)
	a.UndoToken = a.ID
	if err := insertAction(ctx, db.pool, a); err != nil {
		return st, err
	}
	// The rule's rows not reverted yet; RevertRule's own reverts are
	// a's, not the rule's, so they never show up here.
	const pending = `FROM annotation_log l JOIN actions ac ON ac.id = l.action_id
		WHERE ac.actor = $1 AND NOT EXISTS (SELECT 1 FROM annotation_log u WHERE u.undoes = l.seq)`
	after := ""
	for {
		var ids []string
		err = pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, contentLock); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `
				SELECT DISTINCT l.message_id `+pending+`
					AND ($3 = '' OR l.message_id < $3)
					AND (SELECT received_at FROM messages m WHERE m.id = l.message_id) >= $2
				ORDER BY 1 DESC LIMIT $4`, principal, since, after, retroPage)
			if err != nil {
				return err
			}
			if ids, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
				return err
			}
			w := writer{tx: tx}
			for _, mid := range ids {
				st.Messages++
				rows, err := tx.Query(ctx, `SELECT l.seq, l.attr, l.label, l.before `+pending+`
					AND l.message_id = $2 ORDER BY l.seq DESC`, principal, mid)
				if err != nil {
					return err
				}
				type row struct {
					seq    int64
					key    annKey
					before *int16
				}
				var rs []row
				var r row
				if _, err := pgx.ForEachRow(rows, []any{&r.seq, &r.key.attr, &r.key.label, &r.before}, func() error {
					rs = append(rs, r)
					return nil
				}); err != nil {
					return err
				}
				as, err := loadAnnState(ctx, tx, mid)
				if err != nil {
					return err
				}
				p := annPlan{st: as, rank: RankRule}
				for _, r := range rs {
					if p.undo(r.key, r.seq, r.before) {
						st.Reverted++
					} else {
						st.Superseded++
					}
				}
				if err := p.write(ctx, tx, &w, a.ID); err != nil {
					return err
				}
			}
			return w.flush(ctx)
		})
		if err != nil || len(ids) < retroPage {
			break
		}
		after = ids[len(ids)-1]
	}
	if _, uerr := db.pool.Exec(context.WithoutCancel(ctx), `UPDATE actions SET result = $2 WHERE id = $1`,
		a.ID, map[string]any{"stats": st, "complete": err == nil}); uerr != nil && err == nil {
		err = uerr
	}
	if err != nil {
		return st, fmt.Errorf("revert rule %s: %w", id, err)
	}
	return st, nil
}

// PreviewQuery narrows PreviewRule.
type PreviewQuery struct {
	// Since keeps messages received at or after it; zero is all mail.
	Since time.Time
	// Samples is how many matching messages to return, newest first:
	// default 10, at most 50.
	Samples int
}

// Preview is what a rule would do to the mail a viewer can see.
type Preview struct {
	RetroStats
	// Warnings are the rule's validation warnings.
	Warnings []rules.Warning
	// Samples are matching messages, newest first.
	Samples []MessageSummary
}

// PreviewRule evaluates spec ({"v":1,"match":…,"actions":…}, validated
// as on save) over the mail v can see, read only: how many messages
// match and how many would change at rule rank, with samples. It is the
// same evaluator as ingest's, so what it shows is what a saved rule
// would do. Errors from validation wrap rules.ErrInvalid.
func (db *DB) PreviewRule(ctx context.Context, v *auth.Viewer, spec []byte, q PreviewQuery) (Preview, error) {
	if err := needViewer(v, "preview rule"); err != nil {
		return Preview{}, err
	}
	s, warns, err := ParseRuleSpec(spec)
	if err != nil {
		return Preview{}, err
	}
	n := q.Samples
	if n <= 0 {
		n = 10
	}
	n = min(n, 50)
	out := Preview{Warnings: warns}
	prog := rules.Compile(s)
	ins, _ := annotationsOf(s.Actions)
	var samples []string
	err = scope{db: db, since: q.Since, v: v}.scan(ctx, prog, false, nil, &out.RetroStats,
		func(tx pgx.Tx, _ *writer, mid string, matched bool) error {
			if !matched {
				return nil
			}
			if len(samples) < n {
				samples = append(samples, mid)
			}
			as, err := loadAnnState(ctx, tx, mid)
			if err != nil {
				return err
			}
			p := annPlan{st: as, rank: RankRule}
			for _, r := range p.plan(ins) {
				if r.Outcome == Overruled {
					out.Overruled++
					break
				}
			}
			if p.changed() {
				out.Changed++
			}
			return nil
		})
	if err != nil {
		return Preview{}, fmt.Errorf("preview rule: %w", err)
	}
	if len(samples) > 0 {
		rows, err := db.pool.Query(ctx, `SELECT `+summaryCols+db.summaryFrom(v)+`
			AND m.id = ANY($1) ORDER BY m.id DESC`, samples)
		if err != nil {
			return Preview{}, fmt.Errorf("preview rule: %w", err)
		}
		if out.Samples, err = pgx.CollectRows(rows, scanSummary); err != nil {
			return Preview{}, fmt.Errorf("preview rule: %w", err)
		}
	}
	return out, nil
}
