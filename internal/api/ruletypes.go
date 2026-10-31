package api

import (
	"time"

	"github.com/yuuka-mail/yuuka/internal/agentview"
)

// The shapes of M3's annotations, rules and proposals (§8.5). Like
// types.go, openapi.json is derived from them; the parts the MCP tools
// share (actions, kinds, outcomes) are agentview's.

// RuleCondition is a leaf of a match tree.
type RuleCondition struct {
	Field string `json:"field" jsonschema:"such as from.addr, from.verified, list.id, to.addr"`
	Op    string `json:"op" jsonschema:"eq, in, contains, regex or has"`
	Value any    `json:"value" jsonschema:"a string, a boolean, or for in a list of strings"`
}

// RuleWarning is something a rule allows but probably does not intend.
// Code is stable (unverified_from: it trusts the From header without
// from.verified, so forged mail can get its labels); Message is for
// people.
type RuleWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// AnnotateBody is the body of POST /api/v1/messages/{id}/annotations.
type AnnotateBody struct {
	Actions []agentview.Annotation  `json:"actions"`
	Scope   agentview.AnnotateScope `json:"scope,omitempty" jsonschema:"message (default) or thread"`
}

// AnnotateParams are the parameters of POST /api/v1/messages/{id}/annotations.
type AnnotateParams struct {
	ID   string       `path:"id"`
	Body AnnotateBody `body:"json"`
}

// AnnotationOutcome is an Annotation and what became of it.
type AnnotationOutcome struct {
	agentview.Annotation
	Outcome agentview.AnnotateOutcome `json:"outcome"`
}

// MessageOutcomes are the outcomes of annotations on one message, in
// the order they were asked.
type MessageOutcomes struct {
	MessageID string              `json:"message_id"`
	Outcomes  []AnnotationOutcome `json:"outcomes"`
}

// AnnotateResult is what an annotation did.
type AnnotateResult struct {
	// ActionID is the undo handle (POST /api/v1/actions/{id}/undo);
	// absent when nothing changed and nothing was recorded.
	ActionID string            `json:"action_id,omitempty"`
	Messages []MessageOutcomes `json:"messages"`
}

// WriterRank is a writer's place in the precedence of annotations: a
// person (or the host CLI) over an agent or service over a rule. A
// writer cannot change what a higher rank set last.
type WriterRank string

func (WriterRank) Enum() []string { return []string{"rule", "agent", "person"} }

// AnnotationEntry is one change in a message's annotation history.
type AnnotationEntry struct {
	Seq      int64                `json:"seq"`
	ActionID string               `json:"action_id"`
	Change   agentview.Annotation `json:"change"`
	Rank     WriterRank           `json:"rank"`
	// By is who made the change; a rule is kind rule named by its id.
	By   Actor     `json:"by"`
	Tool string    `json:"tool"`
	At   time.Time `json:"at"`
	// Undoes is the seq of the change this one reverted, if it is an
	// undo or a rule's revert; UndoneBy the seq of the one that
	// reverted it.
	Undoes   int64 `json:"undoes,omitempty"`
	UndoneBy int64 `json:"undone_by,omitempty"`
}

// AnnotationHistory is GET /api/v1/messages/{id}/annotations: where each
// label, the class override and the archived state came from, oldest
// first. A person sees every change; an agent or service only its own.
type AnnotationHistory struct {
	MessageID string            `json:"message_id"`
	History   []AnnotationEntry `json:"history"`
}

// UndoResult counts what an undo did with the changes of an action.
type UndoResult struct {
	// Reverted changes were set back to their value before the action.
	Reverted int `json:"reverted"`
	// Superseded ones were changed again since, or undone already, and
	// are left as they are now.
	Superseded int `json:"superseded"`
	// Hidden ones are on messages the caller cannot see.
	Hidden int `json:"hidden"`
}

// LabelCount is a label in use and how many messages the caller can see
// carry it.
type LabelCount struct {
	Label    string `json:"label"`
	Messages int    `json:"messages"`
}

// LabelList is GET /api/v1/labels, by name.
type LabelList struct {
	Labels []LabelCount `json:"labels"`
}

// RuleBody is a rule as written. PUT replaces every field: an absent
// disabled enables the rule, an absent priority is 0.
type RuleBody struct {
	Name     string                 `json:"name,omitempty" jsonschema:"for people; at most 200 characters"`
	Priority int                    `json:"priority,omitempty" jsonschema:"evaluation order, ascending, ties by id; the first match sets class, archived and notify, labels add up"`
	Disabled bool                   `json:"disabled,omitempty" jsonschema:"saved but not run"`
	Match    agentview.RuleMatch    `json:"match"`
	Actions  []agentview.RuleAction `json:"actions"`
}

// CreateRuleParams are the parameters of POST /api/v1/rules.
type CreateRuleParams struct {
	Body RuleBody `body:"json"`
}

// UpdateRuleParams are the parameters of PUT /api/v1/rules/{id}.
type UpdateRuleParams struct {
	ID   string   `path:"id"`
	Body RuleBody `body:"json"`
}

// RulesParams are the parameters of GET /api/v1/rules.
type RulesParams struct {
	Deleted bool `query:"deleted" jsonschema:"also list deleted rules"`
}

// Rule is a stored rule. It runs on mail as it arrives; changing it does
// not change what it did to mail already here (apply and revert do).
type Rule struct {
	ID       string                 `json:"id"`
	Name     string                 `json:"name"`
	Priority int                    `json:"priority"`
	Enabled  bool                   `json:"enabled"`
	Revision int                    `json:"revision"`
	Match    agentview.RuleMatch    `json:"match"`
	Actions  []agentview.RuleAction `json:"actions"`
	// ProposalID is the proposal it was accepted from, if any.
	ProposalID string `json:"proposal_id,omitempty"`
	// CreatedBy is the principal id of whoever created it.
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	// Unsupported says why this version cannot evaluate the stored rule;
	// such a rule never matches.
	Unsupported []string `json:"unsupported,omitempty"`
}

// SavedRule is a rule as saved, with the warnings of its validation.
type SavedRule struct {
	Rule
	Warnings []RuleWarning `json:"warnings"`
}

// RuleList is GET /api/v1/rules, in evaluation order.
type RuleList struct {
	Rules []Rule `json:"rules"`
}

// RetroBody is the body of a retroactive run over past mail.
type RetroBody struct {
	Since  *time.Time `json:"since,omitempty" jsonschema:"only mail received at or after this time; absent is all mail"`
	DryRun bool       `json:"dry_run,omitempty" jsonschema:"apply only: count, write nothing"`
}

// RetroParams are the parameters of POST /api/v1/rules/{id}/apply and
// /revert.
type RetroParams struct {
	ID   string    `path:"id"`
	Body RetroBody `body:"json"`
}

// RetroStats counts what running a rule over past mail did, or with
// dry_run or in a preview, would do. It respects precedence and never
// notifies.
type RetroStats struct {
	Messages int `json:"messages"`
	Matched  int `json:"matched"`
	// Changed messages got a new value; Overruled ones kept something a
	// person or an agent decided.
	Changed   int `json:"changed"`
	Overruled int `json:"overruled"`
	// Failed evaluations ran out of time and did not match.
	Failed int `json:"failed"`
}

// RevertStats counts what taking back a rule's changes did. Superseded
// changes were changed again since and are left as they are now.
type RevertStats struct {
	Messages   int `json:"messages"`
	Reverted   int `json:"reverted"`
	Superseded int `json:"superseded"`
}

// PreviewBody is a rule to try on the caller's mail without saving it.
type PreviewBody struct {
	Match   agentview.RuleMatch    `json:"match"`
	Actions []agentview.RuleAction `json:"actions"`
	Since   *time.Time             `json:"since,omitempty" jsonschema:"only mail received at or after this time; absent is all mail"`
	Samples int                    `json:"samples,omitempty" minimum:"1" maximum:"50" jsonschema:"how many matching messages to return, default 10"`
}

// PreviewParams are the parameters of POST /api/v1/rules/preview.
type PreviewParams struct {
	Body PreviewBody `body:"json"`
}

// RulePreview is what a rule would do to the mail the caller can see:
// the same evaluator as arriving mail runs, read only.
type RulePreview struct {
	RetroStats
	Warnings []RuleWarning `json:"warnings"`
	// Samples are matching messages, newest first.
	Samples []MessageSummary `json:"samples"`
}

// RuleDraftBody names the example message of a draft rule.
type RuleDraftBody struct {
	MessageID string     `json:"message_id"`
	Since     *time.Time `json:"since,omitempty" jsonschema:"count matches among mail received at or after this time; absent is all mail"`
}

// RuleDraftParams are the parameters of POST /api/v1/rules/draft.
type RuleDraftParams struct {
	Body RuleDraftBody `body:"json"`
}

// RuleCandidate is one condition offered for a draft rule, whether it is
// ticked by default, and how many messages it alone matches.
type RuleCandidate struct {
	Condition RuleCondition `json:"condition"`
	Selected  bool          `json:"selected"`
	Matches   int           `json:"matches"`
}

// RuleDraft is "handle mail like this from now on" (§8.5): conditions
// drawn from the example's sender, list and recipient, never from its
// subject or body, which the sender writes. Match is the ticked ones
// joined with all, absent when none is ticked; Matches counts what it
// matches.
type RuleDraft struct {
	MessageID  string              `json:"message_id"`
	Candidates []RuleCandidate     `json:"candidates"`
	Match      agentview.RuleMatch `json:"match,omitempty"`
	Matches    int                 `json:"matches"`
}

// ProposalBody is a proposal as made. annotate: message_id, scope and
// actions (no notify: there is nothing to notify about mail already
// here). rule: match and actions, with message_id as its example.
type ProposalBody struct {
	Kind      agentview.ProposalKind  `json:"kind"`
	MessageID string                  `json:"message_id,omitempty" jsonschema:"annotate: the message; rule: an example it matches"`
	Scope     agentview.AnnotateScope `json:"scope,omitempty" jsonschema:"annotate: message (default) or thread"`
	Match     agentview.RuleMatch     `json:"match,omitempty" jsonschema:"rule: the match tree"`
	Actions   []agentview.RuleAction  `json:"actions"`
	Rationale string                  `json:"rationale,omitempty" jsonschema:"why, for the person who decides; at most 4000 characters"`
	Cites     []Cite                  `json:"cites,omitempty" jsonschema:"the passages it relies on; at most 32, each quote at most 1000 characters"`
}

// CreateProposalParams are the parameters of POST /api/v1/proposals.
type CreateProposalParams struct {
	Body ProposalBody `body:"json"`
}

// ProposalsParams are the parameters of GET /api/v1/proposals.
type ProposalsParams struct {
	State     agentview.ProposalState `query:"state" jsonschema:"only proposals in this state"`
	MessageID string                  `query:"message_id" jsonschema:"only proposals about this message"`
	Cursor    string                  `query:"cursor" jsonschema:"next_cursor of the previous page"`
	Limit     int                     `query:"limit" minimum:"1" maximum:"500" default:"100"`
}

// Proposal is a suggestion only a person decides (§8.5). A person sees
// every proposal; an agent or service only its own. Rationale and cites
// are the proposer's words, and cites quote mail: neither is yuuka's.
type Proposal struct {
	ID        string                  `json:"id"`
	Kind      agentview.ProposalKind  `json:"kind"`
	State     agentview.ProposalState `json:"state"`
	Proposer  Actor                   `json:"proposer"`
	MessageID string                  `json:"message_id,omitempty"`
	Scope     agentview.AnnotateScope `json:"scope,omitempty"`
	Match     agentview.RuleMatch     `json:"match,omitempty"`
	Actions   []agentview.RuleAction  `json:"actions"`
	Rationale string                  `json:"rationale"`
	Cites     []Cite                  `json:"cites"`
	CreatedAt time.Time               `json:"created_at"`
	ExpiresAt time.Time               `json:"expires_at"`
	// DecidedBy is the principal id that decided or withdrew it.
	DecidedBy string     `json:"decided_by,omitempty"`
	DecidedAt *time.Time `json:"decided_at,omitempty"`
	// ActionID is the decision's action: for an accepted annotate
	// proposal, the undo handle of what it wrote.
	ActionID string `json:"action_id,omitempty"`
	// RuleID is the rule accepting it created.
	RuleID string `json:"rule_id,omitempty"`
}

// ProposalCreated is a new proposal with its rule's warnings.
type ProposalCreated struct {
	Proposal
	Warnings []RuleWarning `json:"warnings"`
}

// ProposalPage is one page of GET /api/v1/proposals, newest first.
type ProposalPage struct {
	Proposals  []Proposal `json:"proposals"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

// AcceptBody is the body of POST /api/v1/proposals/{id}/accept: {} to
// accept as proposed.
type AcceptBody struct {
	// Rule, when set, is the rule to create: a rule proposal as the
	// person edited it, or for an annotate proposal "and from now on"
	// (POST /api/v1/rules/draft drafts one). It needs rule:manage, as
	// does accepting any rule proposal. It is created with the decision,
	// linked to the proposal, and not run over past mail.
	Rule *RuleBody `json:"rule,omitempty"`
}

// AcceptParams are the parameters of POST /api/v1/proposals/{id}/accept.
type AcceptParams struct {
	ID   string     `path:"id"`
	Body AcceptBody `body:"json"`
}

// AcceptResult is what accepting a proposal did.
type AcceptResult struct {
	Proposal Proposal `json:"proposal"`
	// Messages are the outcomes of an annotate proposal's annotations,
	// written at the person's rank.
	Messages []MessageOutcomes `json:"messages"`
	Rule     *SavedRule        `json:"rule,omitempty"`
}
