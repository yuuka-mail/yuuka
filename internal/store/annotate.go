package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/classify"
	"github.com/yuuka-mail/yuuka/internal/rules"
)

// Annotations are a message's class override, archived state and labels
// (message_state). Everyone writes them through one core, here: people
// and the host CLI, agents and services, accepted proposals, and rules
// at ingest or in a retroactive run. Every change is a row of
// annotation_log (migration 0009), which is what undo reverts and what
// decides precedence:
//
//   - A writer has a rank (Rank). It may change an attribute (the class,
//     archived, or one label) only while the latest row for that
//     attribute has a rank no higher than its own. A person always wins;
//     an agent cannot change what a person decided and a rule cannot
//     change what a person or an agent decided. That is not an error:
//     the outcome says Overruled.
//   - Among equals the later writer wins.
//   - A value that is already what is asked writes nothing: no log row,
//     no change entry, no action.
//
// Undo writes rows too, at the undoer's rank, so a person undoing a
// rule's label leaves a rank 3 row that later rule runs respect. The
// read state (flags) is not an annotation: it is not logged.

// Rank is a writer's place in the precedence ladder (annotation_log.rank).
type Rank int16

const (
	RankRule   Rank = 1
	RankAgent  Rank = 2 // agents and services
	RankPerson Rank = 3 // humans and the system (the host CLI)
)

// RankOf is the rank of a principal kind.
func RankOf(kind string) (Rank, error) {
	switch kind {
	case "human", "system":
		return RankPerson, nil
	case "agent", "service":
		return RankAgent, nil
	case "rule":
		return RankRule, nil
	}
	return 0, fmt.Errorf("principal kind %q has no annotation rank", kind)
}

// Annotation attributes (annotation_log.attr).
const (
	AttrClass    = "class"
	AttrArchived = "archived"
	AttrLabel    = "label"
)

// Annotation is one value asked of one attribute of a message. Build it
// with SetLabel, SetArchived, SetClass or ClearClass.
type Annotation struct {
	Attr  string `json:"attr"`
	Label string `json:"label,omitempty"` // AttrLabel only
	// To is the value asked: for the class, the override (nil: none);
	// for archived and a label, 1 on and 0 off.
	To *int16 `json:"to"`
}

func flag(on bool) *int16 {
	v := int16(0)
	if on {
		v = 1
	}
	return &v
}

// SetLabel adds (on) or removes label.
func SetLabel(label string, on bool) Annotation {
	return Annotation{Attr: AttrLabel, Label: label, To: flag(on)}
}

// SetArchived archives (on) or unarchives the message.
func SetArchived(on bool) Annotation { return Annotation{Attr: AttrArchived, To: flag(on)} }

// SetClass overrides the classifier's class.
func SetClass(c classify.Class) Annotation {
	v := int16(c)
	return Annotation{Attr: AttrClass, To: &v}
}

// ClearClass removes the override: the classifier's class applies again.
func ClearClass() Annotation { return Annotation{Attr: AttrClass} }

func (a Annotation) validate() error {
	switch a.Attr {
	case AttrLabel:
		if err := rules.ValidLabel(a.Label); err != nil {
			return err
		}
		if a.To == nil || *a.To > 1 || *a.To < 0 {
			return errors.New("a label is on or off")
		}
	case AttrArchived:
		if a.Label != "" || a.To == nil || *a.To > 1 || *a.To < 0 {
			return errors.New("archived is on or off")
		}
	case AttrClass:
		if a.Label != "" || a.To != nil && (*a.To < 0 || *a.To > 2) {
			return errors.New("class is unknown, machine, human or none")
		}
	default:
		return fmt.Errorf("unknown attribute %q", a.Attr)
	}
	return nil
}

// AnnotateOutcome says what became of one Annotation.
type AnnotateOutcome string

const (
	// Applied: written.
	Applied AnnotateOutcome = "applied"
	// Unchanged: it already had that value; nothing was written.
	Unchanged AnnotateOutcome = "unchanged"
	// Overruled: a writer of higher rank set it last; left alone.
	Overruled AnnotateOutcome = "overruled"
	// LabelsFull: the message already has MaxMessageLabels labels.
	LabelsFull AnnotateOutcome = "labels_full"
)

// MaxMessageLabels is the bound of message_state_labels_bounded.
const MaxMessageLabels = 64

// AnnotationResult is an Annotation and its outcome.
type AnnotationResult struct {
	Annotation
	Outcome AnnotateOutcome `json:"outcome"`
}

type annKey struct{ attr, label string }

// annState is a message's annotations and, per attribute, the latest
// annotation_log row, read inside the writing transaction.
type annState struct {
	message  string
	class    *int16
	archived bool
	labels   []string
	latest   map[annKey]logHead
	exists   bool // a message_state row exists
}

type logHead struct {
	seq  int64
	rank Rank
}

func (s *annState) value(k annKey) *int16 {
	switch k.attr {
	case AttrClass:
		return s.class
	case AttrArchived:
		return flag(s.archived)
	}
	return flag(slices.Contains(s.labels, k.label))
}

func (s *annState) set(k annKey, v *int16) {
	switch k.attr {
	case AttrClass:
		s.class = v
	case AttrArchived:
		s.archived = *v == 1
	default:
		if *v == 1 {
			s.labels = append(s.labels, k.label)
		} else {
			s.labels = slices.DeleteFunc(s.labels, func(l string) bool { return l == k.label })
		}
	}
}

func sameValue(a, b *int16) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

// loadAnnState reads message's annotations and log heads. Writers hold
// contentLock, so nothing changes them until the transaction ends.
func loadAnnState(ctx context.Context, tx pgx.Tx, message string) (*annState, error) {
	s := &annState{message: message, labels: []string{}, latest: map[annKey]logHead{}}
	err := tx.QueryRow(ctx, `SELECT class_override, archived, labels FROM message_state WHERE message_id = $1`, message).
		Scan(&s.class, &s.archived, &s.labels)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return nil, fmt.Errorf("annotations of %s: %w", message, err)
	default:
		s.exists = true
	}
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT ON (attr, label) attr, label, seq, rank FROM annotation_log
		WHERE message_id = $1 ORDER BY attr, label, seq DESC`, message)
	if err != nil {
		return nil, fmt.Errorf("annotation log of %s: %w", message, err)
	}
	var k annKey
	var h logHead
	_, err = pgx.ForEachRow(rows, []any{&k.attr, &k.label, &h.seq, &h.rank}, func() error {
		s.latest[k] = h
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("annotation log of %s: %w", message, err)
	}
	return s, nil
}

// logRow is one annotation_log row to write.
type logRow struct {
	key           annKey
	before, after *int16
	undoes        *int64
}

// annPlan is what one writer at one rank will change on one message.
type annPlan struct {
	st   *annState
	rank Rank
	rows []logRow
}

func (p *annPlan) changed() bool { return len(p.rows) > 0 }

// plan decides each of ins against the precedence ladder and the current
// values, and updates p.st as if written. Later annotations of the same
// attribute see earlier ones.
func (p *annPlan) plan(ins []Annotation) []AnnotationResult {
	out := make([]AnnotationResult, len(ins))
	for i, a := range ins {
		k := annKey{a.Attr, a.Label}
		cur := p.st.value(k)
		out[i] = AnnotationResult{Annotation: a}
		switch {
		case sameValue(cur, a.To):
			out[i].Outcome = Unchanged
		case p.st.latest[k].rank > p.rank:
			out[i].Outcome = Overruled
		case a.Attr == AttrLabel && *a.To == 1 && len(p.st.labels) >= MaxMessageLabels:
			out[i].Outcome = LabelsFull
		default:
			out[i].Outcome = Applied
			p.rows = append(p.rows, logRow{key: k, before: cur, after: a.To})
			p.st.set(k, a.To)
			p.st.latest[k] = logHead{rank: p.rank}
		}
	}
	return out
}

// write stores the planned rows as done by actionID and the resulting
// values, and logs a flag change of the message.
func (p *annPlan) write(ctx context.Context, tx pgx.Tx, w *writer, actionID string) error {
	if !p.changed() {
		return nil
	}
	n := len(p.rows)
	attrs, labels := make([]string, n), make([]string, n)
	before, after := make([]*int16, n), make([]*int16, n)
	undoes := make([]*int64, n)
	for i, r := range p.rows {
		attrs[i], labels[i], before[i], after[i], undoes[i] = r.key.attr, r.key.label, r.before, r.after, r.undoes
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO annotation_log (action_id, message_id, attr, label, before, after, rank, undoes)
		SELECT $1, $2, a, l, b, f, $3, u
		FROM unnest($4::text[], $5::text[], $6::smallint[], $7::smallint[], $8::bigint[]) WITH ORDINALITY AS r(a, l, b, f, u, n)
		ORDER BY n`, actionID, p.st.message, int16(p.rank), attrs, labels, before, after, undoes)
	if err != nil {
		return fmt.Errorf("annotation log: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO message_state (message_id, class_override, archived, labels) VALUES ($1, $2, $3, $4)
		ON CONFLICT (message_id) DO UPDATE SET
			class_override = excluded.class_override, archived = excluded.archived, labels = excluded.labels`,
		p.st.message, p.st.class, p.st.archived, p.st.labels)
	if err != nil {
		return fmt.Errorf("message state: %w", err)
	}
	w.change("flag", p.st.message)
	p.rows = nil
	return nil
}

// annotate is the core every annotation writer goes through: decide ins
// for message at rank, and if anything changes, record a (with the
// outcomes as its result and its own id as the undo token) and write.
// It reports whether a was recorded. The caller holds contentLock.
func annotate(ctx context.Context, tx pgx.Tx, w *writer, a Action, rank Rank, message string, ins []Annotation) ([]AnnotationResult, bool, error) {
	st, err := loadAnnState(ctx, tx, message)
	if err != nil {
		return nil, false, err
	}
	p := annPlan{st: st, rank: rank}
	res := p.plan(ins)
	if !p.changed() {
		return res, false, nil
	}
	a.UndoToken = a.ID
	if a.Result == nil {
		a.Result = map[string]any{"message": message, "outcomes": res}
	}
	if err := insertAction(ctx, tx, a); err != nil {
		return nil, false, err
	}
	return res, true, p.write(ctx, tx, w, a.ID)
}

// ErrNotPermitted: the principal may not do this, whatever its scopes:
// an agent undoing someone else's action, anyone but a person deciding a
// proposal.
var ErrNotPermitted = errors.New("not permitted for this principal")

// Annotate is the annotation entry point of people, agents and services
// (REST, MCP, notification buttons): it writes ins to message as v, at
// the rank of v's principal kind, and records a when anything changed
// (it reports whether). v must be able to see the message (§13.8);
// otherwise ErrNotFound. Rules do not come through here: they run inside
// ingest (Fold) and the retroactive runs.
func (db *DB) Annotate(ctx context.Context, v *auth.Viewer, message string, ins []Annotation, a Action) ([]AnnotationResult, bool, error) {
	if err := needViewer(v, "annotate"); err != nil {
		return nil, false, err
	}
	rank, err := RankOf(v.PrincipalKind)
	if err != nil || rank == RankRule {
		return nil, false, fmt.Errorf("annotate as %s: %w", v, ErrNotPermitted)
	}
	if len(ins) == 0 {
		return nil, false, errors.New("annotate: nothing asked")
	}
	for _, in := range ins {
		if err := in.validate(); err != nil {
			return nil, false, fmt.Errorf("annotate: %w", err)
		}
	}
	var res []AnnotationResult
	var recorded bool
	err = pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, contentLock); err != nil {
			return err
		}
		id, err := db.visibleMessageID(ctx, tx, v, message)
		if err != nil {
			return err
		}
		w := writer{tx: tx}
		if res, recorded, err = annotate(ctx, tx, &w, a, rank, id, ins); err != nil {
			return err
		}
		return w.flush(ctx)
	})
	if err != nil {
		return nil, false, fmt.Errorf("annotate %s: %w", message, err)
	}
	return res, recorded, nil
}

// visibleMessageID resolves an alias and returns the message id if v may
// see it, ErrNotFound otherwise.
func (db *DB) visibleMessageID(ctx context.Context, q querier, v *auth.Viewer, id string) (string, error) {
	var got string
	err := q.QueryRow(ctx, `SELECT m.id FROM messages m
		WHERE m.id = coalesce((SELECT new_id FROM message_aliases WHERE old_id = $1), $1) AND `+db.visibleMessage(v, "m"), id).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("message %s: %w", id, err)
	}
	return got, nil
}

// UndoResult counts what Undo did with the rows of an action.
type UndoResult struct {
	// Reverted rows were set back to their value before the action.
	Reverted int `json:"reverted"`
	// Superseded rows were changed again since, or already undone; they
	// are left as they are now.
	Superseded int `json:"superseded"`
	// Hidden rows are on messages v may not see.
	Hidden int `json:"hidden"`
}

// Undo reverts the annotations action wrote, as v, and records a. Each
// row is reverted only while it is still the latest for its attribute;
// the revert is written at v's rank. Agents and services may undo only
// their own actions (ErrNotPermitted otherwise). ErrNotFound if the
// action wrote no annotations.
func (db *DB) Undo(ctx context.Context, v *auth.Viewer, action string, a Action) (UndoResult, error) {
	var res UndoResult
	if err := needViewer(v, "undo"); err != nil {
		return res, err
	}
	rank, err := RankOf(v.PrincipalKind)
	if err != nil || rank == RankRule {
		return res, fmt.Errorf("undo as %s: %w", v, ErrNotPermitted)
	}
	err = pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, contentLock); err != nil {
			return err
		}
		var actor *string
		err := tx.QueryRow(ctx, `SELECT actor FROM actions WHERE id = $1`, action).Scan(&actor)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if rank < RankPerson && (actor == nil || *actor != v.PrincipalID) {
			return ErrNotPermitted
		}
		rows, err := tx.Query(ctx, `
			SELECT l.seq, l.message_id, l.attr, l.label, l.before, `+db.visibleMessage(v, "m")+`
			FROM annotation_log l JOIN messages m ON m.id = l.message_id
			WHERE l.action_id = $1 ORDER BY l.message_id, l.seq DESC`, action)
		if err != nil {
			return err
		}
		type undoRow struct {
			seq     int64
			message string
			key     annKey
			before  *int16
			visible bool
		}
		var all []undoRow
		var r undoRow
		_, err = pgx.ForEachRow(rows, []any{&r.seq, &r.message, &r.key.attr, &r.key.label, &r.before, &r.visible}, func() error {
			all = append(all, r)
			return nil
		})
		if err != nil {
			return err
		}
		if len(all) == 0 {
			return ErrNotFound
		}
		a.Args = withArg(a.Args, "action", action)
		if err := insertAction(ctx, tx, a); err != nil {
			return err
		}
		w := writer{tx: tx}
		for i := 0; i < len(all); {
			j := i
			for j < len(all) && all[j].message == all[i].message {
				j++
			}
			group := all[i:j]
			i = j
			if !group[0].visible {
				res.Hidden += len(group)
				continue
			}
			st, err := loadAnnState(ctx, tx, group[0].message)
			if err != nil {
				return err
			}
			p := annPlan{st: st, rank: rank}
			for _, r := range group {
				if !p.undo(r.key, r.seq, r.before) {
					res.Superseded++
					continue
				}
				res.Reverted++
			}
			if err := p.write(ctx, tx, &w, a.ID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE actions SET result = $2, undo_token = $1 WHERE id = $1`, a.ID, res); err != nil {
			return err
		}
		return w.flush(ctx)
	})
	if err != nil {
		return UndoResult{}, fmt.Errorf("undo %s: %w", action, err)
	}
	return res, nil
}

// undo plans reverting the row seq of attribute k, which set it from
// before. It does so only while seq is the latest row for k, so a later
// change (or an undo already done) is never overwritten, and only if the
// planner's rank is at least that row's. It reports whether it planned
// anything.
func (p *annPlan) undo(k annKey, seq int64, before *int16) bool {
	head, ok := p.st.latest[k]
	if !ok || head.seq != seq || head.rank > p.rank {
		return false
	}
	cur := p.st.value(k)
	if sameValue(cur, before) {
		return false
	}
	s := seq
	p.rows = append(p.rows, logRow{key: k, before: cur, after: before, undoes: &s})
	p.st.set(k, before)
	p.st.latest[k] = logHead{rank: p.rank}
	return true
}

// withArg adds key to an action's args, which are nil or a map.
func withArg(args any, key string, v any) map[string]any {
	m, _ := args.(map[string]any)
	out := map[string]any{key: v}
	for k, x := range m {
		out[k] = x
	}
	return out
}

// AnnotationEntry is one row of a message's annotation history, with
// who wrote it.
type AnnotationEntry struct {
	Seq      int64
	ActionID string
	Attr     string
	Label    string
	Before   *int16
	After    *int16
	Rank     Rank
	// Actor is kind:name of the action's principal; "" if none.
	Actor  string
	Tool   string
	At     time.Time
	Undoes *int64
}

// MessageAnnotations returns the annotation history of message id as v
// sees it, oldest first: where each label, the archived state and the
// class override came from. ErrNotFound if v cannot see the message.
func (db *DB) MessageAnnotations(ctx context.Context, v *auth.Viewer, id string) ([]AnnotationEntry, error) {
	if err := needViewer(v, "annotations"); err != nil {
		return nil, err
	}
	var out []AnnotationEntry
	err := pgx.BeginTxFunc(ctx, db.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		mid, err := db.visibleMessageID(ctx, tx, v, id)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT l.seq, l.action_id, l.attr, l.label, l.before, l.after, l.rank,
				coalesce(p.kind || ':' || p.name, ''), a.tool, a.at, l.undoes
			FROM annotation_log l JOIN actions a ON a.id = l.action_id
			LEFT JOIN principals p ON p.id = a.actor
			WHERE l.message_id = $1 ORDER BY l.seq`, mid)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (AnnotationEntry, error) {
			var e AnnotationEntry
			err := r.Scan(&e.Seq, &e.ActionID, &e.Attr, &e.Label, &e.Before, &e.After, &e.Rank,
				&e.Actor, &e.Tool, &e.At, &e.Undoes)
			return e, err
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("annotations of %s: %w", id, err)
	}
	return out, nil
}

// LabelCount is a label and how many messages v can see carry it.
type LabelCount struct {
	Label    string
	Messages int
}

// Labels lists the labels in use on the messages v can see, by name.
func (db *DB) Labels(ctx context.Context, v *auth.Viewer) ([]LabelCount, error) {
	if err := needViewer(v, "labels"); err != nil {
		return nil, err
	}
	rows, err := db.pool.Query(ctx, `
		SELECT l, count(*) FROM message_state s CROSS JOIN unnest(s.labels) l
		JOIN messages m ON m.id = s.message_id
		WHERE s.labels <> '{}' AND `+db.visibleMessage(v, "m")+`
		GROUP BY l ORDER BY l`)
	if err != nil {
		return nil, fmt.Errorf("labels: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (LabelCount, error) {
		var c LabelCount
		return c, r.Scan(&c.Label, &c.Messages)
	})
	if err != nil {
		return nil, fmt.Errorf("labels: %w", err)
	}
	return out, nil
}
