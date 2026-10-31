package mcp

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/rules"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// The annotation tools of M3 (§8.5, §8.7): an agent labels, classifies
// and archives directly, because an annotation is undoable and a
// person's always wins, and it proposes what it wants a person to
// decide: annotations, or a rule that keeps doing it. It gets no tool to
// manage rules. Nothing here is outbound, and being overruled by a
// person is a result, never an error.
//
// The store records annotate, undo and propose itself (the action is
// the undo handle), so those results carry recorded and tool() does not
// write a second row.

// AnnotateInput is the input of annotate.
type AnnotateInput struct {
	URI     string                  `json:"uri" jsonschema:"yuuka://message/<id> of the message"`
	Actions []agentview.Annotation  `json:"actions" jsonschema:"the changes, in order: label/unlabel (with label), archive/unarchive, classify (with class)/unclassify"`
	Scope   agentview.AnnotateScope `json:"scope,omitempty" jsonschema:"message (default), or thread: every message of its thread you can see"`
}

// AnnotationOutcome is an asked change and what became of it.
type AnnotationOutcome struct {
	agentview.Annotation
	Outcome agentview.AnnotateOutcome `json:"outcome"`
}

// MessageOutcomes are the outcomes on one message.
type MessageOutcomes struct {
	URI      string              `json:"uri"`
	Outcomes []AnnotationOutcome `json:"outcomes"`
}

// AnnotateOutput is the structured result of annotate.
type AnnotateOutput struct {
	// ActionID undoes what changed (undo); absent when nothing did.
	ActionID string            `json:"action_id,omitempty"`
	Messages []MessageOutcomes `json:"messages"`
}

func (s *Server) action(v *auth.Viewer, tool string, args any) store.Action {
	now := s.now()
	return store.Action{ID: ulid.New(now), Actor: v.PrincipalID, CredentialID: v.CredentialID, Tool: "mcp." + tool, Args: args, At: now}
}

func (s *Server) annotate(ctx context.Context, v *auth.Viewer, in AnnotateInput) (result, error) {
	id, _, err := agentview.ParseURI(in.URI)
	if err != nil {
		return result{}, refuse("%v", err)
	}
	ins, err := agentview.StoreAnnotations(in.Actions)
	if err != nil {
		return result{}, refuse("%v", err)
	}
	a := s.action(v, "annotate", in)
	byMessage := map[string][]store.AnnotationResult{}
	var recorded bool
	if in.Scope == "thread" {
		byMessage, recorded, err = s.db.AnnotateThread(ctx, v, id, ins, a)
	} else {
		var res []store.AnnotationResult
		res, recorded, err = s.db.Annotate(ctx, v, id, ins, a)
		byMessage[id] = res
	}
	if err != nil {
		return result{}, annotateErr(err, id)
	}
	out := AnnotateOutput{Messages: []MessageOutcomes{}}
	if recorded {
		out.ActionID = a.ID
	}
	overruled := false
	// Message ids sort in arrival order.
	for _, m := range slices.Sorted(maps.Keys(byMessage)) {
		mo := MessageOutcomes{URI: agentview.MessageURI(m), Outcomes: make([]AnnotationOutcome, len(byMessage[m]))}
		for i, r := range byMessage[m] {
			mo.Outcomes[i] = AnnotationOutcome{Annotation: agentview.AnnotationOf(r.Attr, r.Label, r.To), Outcome: agentview.AnnotateOutcome(r.Outcome)}
			overruled = overruled || r.Outcome == store.Overruled
		}
		out.Messages = append(out.Messages, mo)
	}
	var b strings.Builder
	if out.ActionID != "" {
		fmt.Fprintf(&b, "Done; undo with action_id %s.\n", out.ActionID)
	} else {
		b.WriteString("Nothing changed.\n")
	}
	for _, m := range out.Messages {
		fmt.Fprintf(&b, "%s:", m.URI)
		for _, o := range m.Outcomes {
			fmt.Fprintf(&b, " %s %s", describe(o.Annotation), o.Outcome)
		}
		b.WriteString("\n")
	}
	if overruled {
		b.WriteString("Overruled means a person decided this value: that is final for you and not an error. " +
			"If you think they are wrong, propose instead.\n")
	}
	return result{
		structured: out, text: b.String(), recorded: recorded,
		audit: map[string]any{"action_id": out.ActionID, "messages": len(out.Messages)},
	}, nil
}

// describe is one change as the result text names it. Labels are
// annotations, not mail content.
func describe(a agentview.Annotation) string {
	switch {
	case a.Label != "":
		return fmt.Sprintf("%s %q", a.Kind, a.Label)
	case a.Class != "":
		return fmt.Sprintf("%s %s", a.Kind, a.Class)
	}
	return string(a.Kind)
}

func annotateErr(err error, id string) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return refuse("no message %s", agentview.MessageURI(id))
	case errors.Is(err, store.ErrInvalidAnnotation), errors.Is(err, rules.ErrInvalid):
		return refuse("%v", err)
	}
	return err
}

// UndoInput is the input of undo.
type UndoInput struct {
	ActionID string `json:"action_id" jsonschema:"the action_id annotate returned"`
}

// UndoOutput is the structured result of undo.
type UndoOutput struct {
	// Reverted changes were set back; Superseded ones were changed again
	// since (often by a person) and are left as they are now.
	Reverted   int `json:"reverted"`
	Superseded int `json:"superseded"`
	Hidden     int `json:"hidden,omitempty"`
}

func (s *Server) undo(ctx context.Context, v *auth.Viewer, in UndoInput) (result, error) {
	res, err := s.db.Undo(ctx, v, in.ActionID, s.action(v, "undo", in))
	if errors.Is(err, store.ErrNotFound) {
		return result{}, refuse("no action %s of yours that changed annotations", in.ActionID)
	}
	if err != nil {
		return result{}, err
	}
	out := UndoOutput{Reverted: res.Reverted, Superseded: res.Superseded, Hidden: res.Hidden}
	text := fmt.Sprintf("Reverted %d changes; %d were changed again since and are left as they are.\n", out.Reverted, out.Superseded)
	return result{structured: out, text: text, recorded: true, audit: out}, nil
}

// ProposeInput is the input of propose.
type ProposeInput struct {
	Kind agentview.ProposalKind `json:"kind" jsonschema:"annotate: changes to one message or its thread; rule: a rule that applies them to arriving mail from now on"`
	// Target is the message: the one to annotate, or a rule's example.
	Target    string         `json:"target,omitempty" jsonschema:"yuuka://message/<id>: the message to annotate (annotate), or an example the rule matches (rule)"`
	Payload   ProposePayload `json:"payload"`
	Rationale string         `json:"rationale" jsonschema:"why, for the person who decides; at most 4000 characters"`
	Cites     []Cite         `json:"cites,omitempty" jsonschema:"the passages it relies on, as get_message anchors them"`
}

// ProposePayload is what is proposed.
type ProposePayload struct {
	Scope   agentview.AnnotateScope `json:"scope,omitempty" jsonschema:"annotate: message (default) or thread"`
	Match   agentview.RuleMatch     `json:"match,omitempty" jsonschema:"rule: the match tree, one node of {\"all\":[…]}, {\"any\":[…]}, {\"not\":{…}} or {\"field\",\"op\",\"value\"}; fields such as from.addr, from.verified, list.id, to.addr; ops eq, in, contains, regex, has"`
	Actions []agentview.RuleAction  `json:"actions" jsonschema:"label, classify, archive, and for a rule notify (off, silent, normal); nothing else exists"`
}

// auditArgs keep the proposal's shape, not the rationale or the quotes,
// which may copy mail.
func (in ProposeInput) auditArgs() map[string]any {
	return map[string]any{"kind": in.Kind, "target": in.Target, "payload": in.Payload, "cites": len(in.Cites)}
}

// Proposal is a proposal as the caller sees it: only its own.
type Proposal struct {
	ID        string                  `json:"id"`
	Kind      agentview.ProposalKind  `json:"kind"`
	State     agentview.ProposalState `json:"state"`
	Target    string                  `json:"target,omitempty"`
	Payload   ProposePayload          `json:"payload"`
	Rationale string                  `json:"rationale"`
	Cites     []Cite                  `json:"cites"`
	CreatedAt time.Time               `json:"created_at"`
	ExpiresAt time.Time               `json:"expires_at"`
	DecidedAt *time.Time              `json:"decided_at,omitempty"`
	// RuleID is the rule the person created from it.
	RuleID   string   `json:"rule_id,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

func proposalOf(p store.Proposal) Proposal {
	out := Proposal{
		ID: p.ID, Kind: agentview.ProposalKind(p.Kind), State: agentview.ProposalState(p.State),
		Rationale: p.Rationale, Cites: make([]Cite, len(p.Cites)), RuleID: p.RuleID,
		CreatedAt: p.CreatedAt.UTC(), ExpiresAt: p.ExpiresAt.UTC(), DecidedAt: utc(p.DecidedAt),
	}
	if p.MessageID != "" {
		out.Target = agentview.MessageURI(p.MessageID)
	}
	out.Payload.Scope, out.Payload.Match, out.Payload.Actions = agentview.ProposalContent(p.Payload)
	for i, c := range p.Cites {
		out.Cites[i] = Cite{URI: c.URI, Quote: c.Quote}
	}
	return out
}

func (s *Server) propose(ctx context.Context, v *auth.Viewer, in ProposeInput) (result, error) {
	var message string
	if in.Target != "" {
		id, _, err := agentview.ParseURI(in.Target)
		if err != nil {
			return result{}, refuse("target: %v", err)
		}
		message = id
	}
	for _, c := range in.Cites {
		if _, _, err := agentview.ParseURI(c.URI); err != nil {
			return result{}, refuse("cites: %v", err)
		}
	}
	payload, err := agentview.ProposalPayload(in.Kind, in.Payload.Scope, in.Payload.Match, in.Payload.Actions)
	if err != nil {
		return result{}, refuse("payload: %v", err)
	}
	cites := make([]store.Cite, len(in.Cites))
	for i, c := range in.Cites {
		cites[i] = store.Cite{URI: c.URI, Quote: c.Quote}
	}
	p, warns, err := s.db.CreateProposal(ctx, v, store.NewProposal{
		Kind: string(in.Kind), MessageID: message, Payload: payload, Rationale: in.Rationale, Cites: cites,
	}, s.action(v, "propose", in.auditArgs()))
	if e, ok := errors.AsType[*rules.InvalidError](err); ok {
		return result{}, refuse("payload.%s: %s", e.Path, e.Reason)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return result{}, refuse("no message %s", in.Target)
	case errors.Is(err, store.ErrDuplicate):
		return result{}, refuse("you have proposed exactly this already; it is pending")
	case errors.Is(err, store.ErrTooManyProposals):
		return result{}, refuse("you have %d proposals pending; withdraw some or wait for a person to decide", store.MaxPendingProposals)
	case errors.Is(err, rules.ErrInvalid):
		return result{}, refuse("%v", err)
	case err != nil:
		return result{}, err
	}
	out := proposalOf(p)
	var b strings.Builder
	fmt.Fprintf(&b, "Proposal %s is pending: a person accepts or rejects it; it expires %s. Nothing is applied until then.\n", p.ID, out.ExpiresAt.Format(time.RFC3339))
	for _, w := range warns {
		out.Warnings = append(out.Warnings, w.Message)
		fmt.Fprintf(&b, "Warning (%s): %s\n", w.Code, w.Message)
	}
	return result{structured: out, text: b.String(), recorded: true, audit: map[string]any{"proposal": p.ID}}, nil
}

// ListProposalsInput is the input of list_proposals.
type ListProposalsInput struct {
	State agentview.ProposalState `json:"state,omitempty" jsonschema:"only proposals in this state; absent is all"`
	Limit int                     `json:"limit,omitempty" jsonschema:"at most this many, newest first; default 50, at most 500"`
}

// ProposalsOutput is the structured result of list_proposals.
type ProposalsOutput struct {
	Proposals []Proposal `json:"proposals"`
}

func (s *Server) listProposals(ctx context.Context, v *auth.Viewer, in ListProposalsInput) (result, error) {
	limit := 50
	if in.Limit > 0 {
		limit = min(in.Limit, 500)
	}
	ps, err := s.db.Proposals(ctx, v, store.ProposalQuery{State: string(in.State), Limit: limit})
	if err != nil {
		return result{}, err
	}
	out := ProposalsOutput{Proposals: make([]Proposal, len(ps))}
	var facts, said strings.Builder
	fmt.Fprintf(&facts, "%d of your proposals, newest first. accepted and rejected are a person's decisions; expired ones nobody decided.\n", len(ps))
	for i, p := range ps {
		out.Proposals[i] = proposalOf(p)
		pr := out.Proposals[i]
		fmt.Fprintf(&facts, "%d. %s · %s · %s", i+1, pr.ID, pr.Kind, pr.State)
		if pr.Target != "" {
			fmt.Fprintf(&facts, " · %s", pr.Target)
		}
		if pr.RuleID != "" {
			fmt.Fprintf(&facts, " · became rule %s", pr.RuleID)
		}
		facts.WriteString("\n")
		// Your own words, but they may quote mail.
		fmt.Fprintf(&said, "%d. Rationale: %s\n", i+1, pr.Rationale)
		for _, c := range pr.Cites {
			fmt.Fprintf(&said, "   Cites %s: %s\n", c.URI, c.Quote)
		}
	}
	text := facts.String()
	if len(ps) > 0 {
		text += "\n" + agentview.Fence("proposals", said.String())
	}
	return result{structured: out, text: text, audit: map[string]any{"returned": len(ps)}}, nil
}
