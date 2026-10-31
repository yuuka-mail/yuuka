package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/rules"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Proposals (§8.5, migration 0009): what an agent suggests and only a
// person decides. Two kinds: annotate (annotations for one message or
// its thread) and rule (a whole rule, with an example message). Either
// carries only annotation actions; nothing outbound can be proposed
// (the table's CHECK says so too). Accepting a rule proposal, or an
// annotate proposal "and from now on", creates the rule in the same
// transaction.

// Proposal kinds and scopes.
const (
	ProposalAnnotate = "annotate"
	ProposalRule     = "rule"

	ScopeMessage = "message"
	ScopeThread  = "thread"
)

// Proposal states. ProposalExpired is never stored: a pending proposal
// past its expires_at reads as expired.
const (
	ProposalPending   = "pending"
	ProposalAccepted  = "accepted"
	ProposalRejected  = "rejected"
	ProposalWithdrawn = "withdrawn"
	ProposalExpired   = "expired"
)

const (
	// ProposalTTL is how long a proposal waits for a decision.
	ProposalTTL = 14 * 24 * time.Hour
	// MaxPendingProposals bounds what one proposer may have waiting, so
	// an agent cannot bury the operator.
	MaxPendingProposals = 50
)

var (
	// ErrTooManyProposals: the proposer has MaxPendingProposals waiting.
	ErrTooManyProposals = errors.New("too many pending proposals")
	// ErrNotPending: the proposal was decided, withdrawn or has expired.
	ErrNotPending = errors.New("proposal is not pending")
)

// Cite is a citation of a proposal (§8.1): a yuuka:// anchor and the
// text it points at, so it can be found again if the anchor drifts.
type Cite struct {
	URI   string `json:"uri"`
	Quote string `json:"quote"`
}

// AnnotatePayload is the payload of an annotate proposal.
type AnnotatePayload struct {
	V       int            `json:"v"`
	Scope   string         `json:"scope"`
	Actions []rules.Action `json:"actions"`
}

// NewProposal is a proposal to make.
type NewProposal struct {
	Kind      string
	MessageID string // required for annotate; the example for rule
	// Payload: AnnotatePayload as JSON for annotate, a rule spec
	// ({"v":1,"match":…,"actions":…}) for rule.
	Payload   []byte
	Rationale string
	Cites     []Cite
}

// Proposal is a row of proposals as a viewer sees it.
type Proposal struct {
	ID         string
	ProposedBy string
	Proposer   string // kind:name
	Kind       string
	MessageID  string
	Payload    json.RawMessage
	Rationale  string
	Cites      []Cite
	State      string // as stored, or ProposalExpired
	CreatedAt  time.Time
	ExpiresAt  time.Time
	DecidedBy  string
	DecidedAt  *time.Time
	ActionID   string
	// RuleID is the rule accepting it created, if any.
	RuleID string
}

// parsePayload validates a proposal's payload for its kind and returns
// the canonical form to store, and the rule's warnings for kind rule.
func parsePayload(kind string, b []byte) ([]byte, []rules.Warning, error) {
	switch kind {
	case ProposalRule:
		s, warns, err := ParseRuleSpec(b)
		if err != nil {
			return nil, nil, err
		}
		out, err := json.Marshal(s)
		return out, warns, err
	case ProposalAnnotate:
		var raw struct {
			V       int             `json:"v"`
			Scope   string          `json:"scope"`
			Actions json.RawMessage `json:"actions"`
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&raw); err != nil {
			return nil, nil, &rules.InvalidError{Path: "payload", Reason: err.Error()}
		}
		if raw.V != 1 || (raw.Scope != ScopeMessage && raw.Scope != ScopeThread) {
			return nil, nil, &rules.InvalidError{Path: "scope", Reason: "want v 1 and scope message or thread"}
		}
		as, err := rules.ParseActions(raw.Actions)
		if err != nil {
			return nil, nil, err
		}
		if err := rules.ValidateActions(as); err != nil {
			return nil, nil, err
		}
		for _, a := range as {
			if a.Kind == rules.KindNotify {
				// Notification is decided when mail arrives; there is
				// nothing to notify about mail already here.
				return nil, nil, &rules.InvalidError{Path: "actions", Reason: "an annotate proposal cannot notify"}
			}
		}
		out, err := json.Marshal(AnnotatePayload{V: 1, Scope: raw.Scope, Actions: as})
		return out, nil, err
	}
	return nil, nil, &rules.InvalidError{Path: "kind", Reason: fmt.Sprintf("proposal kind %q", kind)}
}

// CreateProposal stores p as proposed by v and records a. The message,
// if any, must be visible to v. The same pending proposal twice is
// ErrDuplicate; one that expired is withdrawn on the proposer's behalf
// and the new one takes its place. It returns the warnings of a
// proposed rule.
func (db *DB) CreateProposal(ctx context.Context, v *auth.Viewer, p NewProposal, a Action) (Proposal, []rules.Warning, error) {
	if err := needViewer(v, "propose"); err != nil {
		return Proposal{}, nil, err
	}
	if p.Kind == ProposalAnnotate && p.MessageID == "" {
		return Proposal{}, nil, &rules.InvalidError{Path: "message_id", Reason: "an annotate proposal names its message"}
	}
	payload, warns, err := parsePayload(p.Kind, p.Payload)
	if err != nil {
		return Proposal{}, nil, err
	}
	cites := p.Cites
	if cites == nil {
		cites = []Cite{}
	}
	var out Proposal
	err = pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		// Writers of changes serialize on contentLock (changes.seq is
		// commit order); it also covers the per-proposer cap.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, contentLock); err != nil {
			return err
		}
		var message *string
		if p.MessageID != "" {
			id, err := db.visibleMessageID(ctx, tx, v, p.MessageID)
			if err != nil {
				return err
			}
			message = &id
		}
		// An identical expired proposal still holds the dedupe index.
		if _, err := tx.Exec(ctx, `
			UPDATE proposals SET state = 'withdrawn', decided_by = proposed_by, decided_at = $5
			WHERE state = 'pending' AND expires_at <= $5 AND proposed_by = $1 AND kind = $2
				AND coalesce(message_id, '') = coalesce($3, '') AND md5(payload::text) = md5($4::jsonb::text)`,
			v.PrincipalID, p.Kind, message, payload, a.At); err != nil {
			return err
		}
		var pending int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM proposals WHERE proposed_by = $1 AND state = 'pending' AND expires_at > $2`,
			v.PrincipalID, a.At).Scan(&pending); err != nil {
			return err
		}
		if pending >= MaxPendingProposals {
			return ErrTooManyProposals
		}
		id := ulid.New(a.At)
		_, err := tx.Exec(ctx, `
			INSERT INTO proposals (id, proposed_by, kind, message_id, payload, rationale, cites, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			id, v.PrincipalID, p.Kind, message, payload, p.Rationale, cites, a.At, a.At.Add(ProposalTTL))
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
			return ErrDuplicate
		}
		if err != nil {
			return fmt.Errorf("insert proposal: %w", err)
		}
		a.Args = withArg(a.Args, "proposal", id)
		if err := insertAction(ctx, tx, a); err != nil {
			return err
		}
		w := writer{tx: tx}
		w.change("proposal", id)
		if err := w.flush(ctx); err != nil {
			return err
		}
		out, err = db.proposal(ctx, tx, v, id, a.At)
		return err
	})
	if err != nil {
		return Proposal{}, nil, fmt.Errorf("create proposal: %w", err)
	}
	return out, warns, nil
}

// proposalVisible is the condition on proposals pr that v may see
// (§13.8). A person sees every proposal, an agent or service only its
// own: a rationale and its cites quote mail, maybe mail the reader may
// not see. Either way one about a message only if v may see the
// message; a rule proposal without an example message is not about
// any.
func (db *DB) proposalVisible(v *auth.Viewer) string {
	own := "true"
	if !isPerson(v) {
		// Inlined like readableAccount: only a well-formed id is.
		own = "false"
		if ulid.Valid(v.PrincipalID) {
			own = `pr.proposed_by = '` + v.PrincipalID + `'`
		}
	}
	return `(` + own + ` AND (pr.message_id IS NULL OR EXISTS (SELECT 1 FROM messages m WHERE m.id = pr.message_id AND ` +
		db.visibleMessage(v, "m") + `)))`
}

const proposalCols = `pr.id, pr.proposed_by, p.kind || ':' || p.name, pr.kind, coalesce(pr.message_id, ''), pr.payload,
	pr.rationale, pr.cites, CASE WHEN pr.state = 'pending' AND pr.expires_at <= $1 THEN 'expired' ELSE pr.state END,
	pr.created_at, pr.expires_at, coalesce(pr.decided_by, ''), pr.decided_at, coalesce(pr.action_id, ''),
	coalesce((SELECT r.id FROM rules r WHERE r.proposal_id = pr.id ORDER BY r.id LIMIT 1), '') AS rule_id`

const proposalFrom = ` FROM proposals pr JOIN principals p ON p.id = pr.proposed_by`

func scanProposal(r pgx.Row) (Proposal, error) {
	var p Proposal
	err := r.Scan(&p.ID, &p.ProposedBy, &p.Proposer, &p.Kind, &p.MessageID, &p.Payload, &p.Rationale, &p.Cites,
		&p.State, &p.CreatedAt, &p.ExpiresAt, &p.DecidedBy, &p.DecidedAt, &p.ActionID, &p.RuleID)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

func (db *DB) proposal(ctx context.Context, q querier, v *auth.Viewer, id string, now time.Time) (Proposal, error) {
	return scanProposal(q.QueryRow(ctx, `SELECT `+proposalCols+proposalFrom+`
		WHERE pr.id = $2 AND `+db.proposalVisible(v), now, id))
}

// ProposalByID returns proposal id as v sees it; ErrNotFound if it does
// not exist or is about a message v cannot see.
func (db *DB) ProposalByID(ctx context.Context, v *auth.Viewer, id string) (Proposal, error) {
	if err := needViewer(v, "proposal"); err != nil {
		return Proposal{}, err
	}
	p, err := db.proposal(ctx, db.pool, v, id, time.Now())
	if err != nil {
		return p, fmt.Errorf("proposal %s: %w", id, err)
	}
	return p, nil
}

// ProposalQuery narrows Proposals.
type ProposalQuery struct {
	// State keeps one state (ProposalExpired included); "" keeps all.
	State string
	// MessageID keeps the proposals about one message.
	MessageID string
	// ProposedBy keeps one proposer's.
	ProposedBy string
	// Before continues a list after the proposal with this id.
	Before string
	Limit  int // default 100, at most 500
}

// Proposals lists the proposals v may see, newest first.
func (db *DB) Proposals(ctx context.Context, v *auth.Viewer, q ProposalQuery) ([]Proposal, error) {
	if err := needViewer(v, "proposals"); err != nil {
		return nil, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 500)
	rows, err := db.pool.Query(ctx, `SELECT * FROM (SELECT `+proposalCols+proposalFrom+`
			WHERE `+db.proposalVisible(v)+` AND ($2 = '' OR pr.message_id = $2) AND ($3 = '' OR pr.proposed_by = $3)) x
		WHERE ($4 = '' OR x.state = $4) AND ($6 = '' OR x.id < $6)
		ORDER BY x.id DESC LIMIT $5`, time.Now(), q.MessageID, q.ProposedBy, q.State, limit, q.Before)
	if err != nil {
		return nil, fmt.Errorf("proposals: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Proposal, error) { return scanProposal(r) })
	if err != nil {
		return nil, fmt.Errorf("proposals: %w", err)
	}
	return out, nil
}

// decidable loads proposal id for a decision by v, under contentLock:
// it must be visible, pending and not expired.
func (db *DB) decidable(ctx context.Context, tx pgx.Tx, v *auth.Viewer, id string, now time.Time) (Proposal, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, contentLock); err != nil {
		return Proposal{}, err
	}
	p, err := db.proposal(ctx, tx, v, id, now)
	if err != nil {
		return p, err
	}
	if p.State != ProposalPending {
		return p, ErrNotPending
	}
	return p, nil
}

func (db *DB) decide(ctx context.Context, tx pgx.Tx, w *writer, id, state string, v *auth.Viewer, a Action) error {
	if _, err := tx.Exec(ctx, `
		UPDATE proposals SET state = $2, decided_by = $3, decided_at = $4, action_id = $5 WHERE id = $1`,
		id, state, v.PrincipalID, a.At, a.ID); err != nil {
		return fmt.Errorf("decide proposal: %w", err)
	}
	w.change("proposal", id)
	return nil
}

// isPerson reports whether v is a person or the host, who alone decide
// proposals (§8.5).
func isPerson(v *auth.Viewer) bool { return v.PrincipalKind == "human" || v.PrincipalKind == "system" }

// Accepted says what accepting a proposal did.
type Accepted struct {
	// Annotations are the outcomes on each message of an annotate
	// proposal, by message id.
	Annotations map[string][]AnnotationResult
	// Rule is the rule created, if any.
	Rule *Rule
	// Warnings are the rule's validation warnings.
	Warnings []rules.Warning
}

// AcceptProposal accepts proposal id as v (a person; ErrNotPermitted
// otherwise) and records a. An annotate proposal's annotations are
// written at v's rank on its message, or on every message of the thread
// v can see. A rule proposal creates its rule. rule, when set, is the
// rule to create instead: the proposal's rule as the person edited it,
// or for an annotate proposal the "from now on" rule. Either way the
// rule and its principal are created in the same transaction, linked by
// proposal_id. The person asked for it, so no retroactive run happens.
func (db *DB) AcceptProposal(ctx context.Context, v *auth.Viewer, id string, rule *NewRule, a Action) (Accepted, error) {
	var out Accepted
	if err := needViewer(v, "accept proposal"); err != nil {
		return out, err
	}
	if !isPerson(v) {
		return out, fmt.Errorf("accept proposal as %s: %w", v, ErrNotPermitted)
	}
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		p, err := db.decidable(ctx, tx, v, id, a.At)
		if err != nil {
			return err
		}
		a.Args = withArg(a.Args, "proposal", id)
		w := writer{tx: tx}
		var newRule NewRule
		switch {
		case rule != nil:
			newRule = *rule
		case p.Kind == ProposalRule:
			newRule = NewRule{Spec: p.Payload}
		}
		var ruleID string
		if newRule.Spec != nil {
			spec, warns, err := ParseRuleSpec(newRule.Spec)
			if err != nil {
				return err
			}
			out.Warnings = warns
			if ruleID, err = createRule(ctx, tx, &w, newRule, spec, id, a); err != nil {
				return err
			}
			a.Args = withArg(a.Args, "rule", ruleID)
		}
		var targets []string
		var ins []Annotation
		if p.Kind == ProposalAnnotate {
			var pl AnnotatePayload
			if err := json.Unmarshal(p.Payload, &pl); err != nil {
				return fmt.Errorf("stored payload: %w", err)
			}
			ins, _ = annotationsOf(pl.Actions)
			targets = []string{p.MessageID}
			if pl.Scope == ScopeThread {
				if targets, err = db.visibleThreadOf(ctx, tx, v, p.MessageID); err != nil {
					return err
				}
			}
		}
		if len(targets) > 0 {
			a.UndoToken = a.ID
		}
		if err := insertAction(ctx, tx, a); err != nil {
			return err
		}
		if err := db.decide(ctx, tx, &w, id, ProposalAccepted, v, a); err != nil {
			return err
		}
		rank, err := RankOf(v.PrincipalKind)
		if err != nil {
			return err
		}
		out.Annotations = map[string][]AnnotationResult{}
		for _, m := range targets {
			st, err := loadAnnState(ctx, tx, m)
			if err != nil {
				return err
			}
			plan := annPlan{st: st, rank: rank}
			out.Annotations[m] = plan.plan(ins)
			if err := plan.write(ctx, tx, &w, a.ID); err != nil {
				return err
			}
		}
		if ruleID != "" {
			r, err := scanRule(tx.QueryRow(ctx, `SELECT `+ruleCols+` FROM rules WHERE id = $1`, ruleID))
			if err != nil {
				return err
			}
			out.Rule = &r
		}
		return w.flush(ctx)
	})
	if err != nil {
		return Accepted{}, fmt.Errorf("accept proposal %s: %w", id, err)
	}
	return out, nil
}

// RejectProposal rejects proposal id as v (a person) and records a.
func (db *DB) RejectProposal(ctx context.Context, v *auth.Viewer, id string, a Action) error {
	if err := needViewer(v, "reject proposal"); err != nil {
		return err
	}
	if !isPerson(v) {
		return fmt.Errorf("reject proposal as %s: %w", v, ErrNotPermitted)
	}
	return db.closeProposal(ctx, v, id, ProposalRejected, a)
}

// WithdrawProposal withdraws proposal id as v, who must have made it,
// and records a. An expired proposal can be withdrawn too.
func (db *DB) WithdrawProposal(ctx context.Context, v *auth.Viewer, id string, a Action) error {
	if err := needViewer(v, "withdraw proposal"); err != nil {
		return err
	}
	return db.closeProposal(ctx, v, id, ProposalWithdrawn, a)
}

func (db *DB) closeProposal(ctx context.Context, v *auth.Viewer, id, state string, a Action) error {
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		p, err := db.decidable(ctx, tx, v, id, a.At)
		if errors.Is(err, ErrNotPending) && state == ProposalWithdrawn && p.State == ProposalExpired {
			err = nil
		}
		if err != nil {
			return err
		}
		if state == ProposalWithdrawn && p.ProposedBy != v.PrincipalID {
			return ErrNotPermitted
		}
		a.Args = withArg(a.Args, "proposal", id)
		if err := insertAction(ctx, tx, a); err != nil {
			return err
		}
		w := writer{tx: tx}
		if err := db.decide(ctx, tx, &w, id, state, v, a); err != nil {
			return err
		}
		return w.flush(ctx)
	})
	if err != nil {
		return fmt.Errorf("%s proposal %s: %w", state, id, err)
	}
	return nil
}
