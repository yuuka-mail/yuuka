package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/openapi"
	"github.com/yuuka-mail/yuuka/internal/rules"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// M3 (§8.5): annotations, rules and proposals. Agents and services
// annotate directly (an annotation is undoable, and a person's always
// wins) and propose; only a person decides a proposal or manages rules.
// That is the principal's kind, checked here like outbox.resolve, on top
// of the scopes the route table states. Every write goes through the
// store's one annotation core and is recorded in actions.

// isPerson: a human. The host CLI (system) never comes through REST.
func isPerson(v *auth.Viewer) bool { return v.PrincipalKind == "human" }

func peopleOnly(v *auth.Viewer, what string) error {
	if isPerson(v) {
		return nil
	}
	return openapi.Errorf(http.StatusForbidden, openapi.CodeForbidden, "only a person %s", what)
}

// action is the audit record of a write by v through REST.
func action(v *auth.Viewer, tool string, args map[string]any) store.Action {
	now := time.Now()
	if args == nil {
		args = map[string]any{}
	}
	args["via"] = "api"
	return store.Action{ID: ulid.New(now), Actor: v.PrincipalID, CredentialID: v.CredentialID, Tool: tool, Args: args, At: now}
}

func outcomesOf(id string, rs []store.AnnotationResult) MessageOutcomes {
	out := MessageOutcomes{MessageID: id, Outcomes: make([]AnnotationOutcome, len(rs))}
	for i, r := range rs {
		out.Outcomes[i] = AnnotationOutcome{Annotation: agentview.AnnotationOf(r.Attr, r.Label, r.To), Outcome: agentview.AnnotateOutcome(r.Outcome)}
	}
	return out
}

func messagesOutcomes(byMessage map[string][]store.AnnotationResult) []MessageOutcomes {
	out := []MessageOutcomes{}
	for id, rs := range byMessage {
		out = append(out, outcomesOf(id, rs))
	}
	// By message id, which is arrival order.
	slices.SortFunc(out, func(a, b MessageOutcomes) int { return strings.Compare(a.MessageID, b.MessageID) })
	return out
}

func (a *API) annotate(ctx context.Context, v *auth.Viewer, p AnnotateParams) (AnnotateResult, error) {
	ins, err := agentview.StoreAnnotations(p.Body.Actions)
	if err != nil {
		return AnnotateResult{}, openapi.Errorf(http.StatusBadRequest, openapi.CodeInvalidBody, "%v", err)
	}
	act := action(v, "message.annotate", map[string]any{"message": p.ID, "scope": p.Body.Scope, "actions": p.Body.Actions})
	out := AnnotateResult{}
	if p.Body.Scope == "thread" {
		res, recorded, err := a.db.AnnotateThread(ctx, v, p.ID, ins, act)
		if err != nil {
			return AnnotateResult{}, err
		}
		out.Messages = messagesOutcomes(res)
		if recorded {
			out.ActionID = act.ID
		}
		return out, nil
	}
	res, recorded, err := a.db.Annotate(ctx, v, p.ID, ins, act)
	if err != nil {
		return AnnotateResult{}, err
	}
	// The store resolved an alias; the outcomes are about p.ID as asked.
	out.Messages = []MessageOutcomes{outcomesOf(p.ID, res)}
	if recorded {
		out.ActionID = act.ID
	}
	return out, nil
}

var ranks = map[store.Rank]WriterRank{store.RankRule: "rule", store.RankAgent: "agent", store.RankPerson: "person"}

func (a *API) annotations(ctx context.Context, v *auth.Viewer, p IDParams) (AnnotationHistory, error) {
	es, err := a.db.MessageAnnotations(ctx, v, p.ID)
	if err != nil {
		return AnnotationHistory{}, err
	}
	out := AnnotationHistory{MessageID: p.ID, History: make([]AnnotationEntry, len(es))}
	at := map[int64]int{}
	for i, e := range es {
		kind, name, _ := strings.Cut(e.Actor, ":")
		out.History[i] = AnnotationEntry{
			Seq: e.Seq, ActionID: e.ActionID, Change: agentview.AnnotationOf(e.Attr, e.Label, e.After), Rank: ranks[e.Rank],
			By: Actor{ID: e.ActorID, Kind: kind, Name: name}, Tool: e.Tool, At: e.At,
		}
		at[e.Seq] = i
		if e.Undoes != nil {
			out.History[i].Undoes = *e.Undoes
			if j, ok := at[*e.Undoes]; ok {
				out.History[j].UndoneBy = e.Seq
			}
		}
	}
	return out, nil
}

func (a *API) undo(ctx context.Context, v *auth.Viewer, p IDParams) (UndoResult, error) {
	res, err := a.db.Undo(ctx, v, p.ID, action(v, "annotation.undo", nil))
	if err != nil {
		return UndoResult{}, err
	}
	return UndoResult{Reverted: res.Reverted, Superseded: res.Superseded, Hidden: res.Hidden}, nil
}

func (a *API) labels(ctx context.Context, v *auth.Viewer, _ struct{}) (LabelList, error) {
	ls, err := a.db.Labels(ctx, v)
	if err != nil {
		return LabelList{}, err
	}
	out := LabelList{Labels: make([]LabelCount, len(ls))}
	for i, l := range ls {
		out.Labels[i] = LabelCount{Label: l.Label, Messages: l.Messages}
	}
	return out, nil
}

func actionsOut(as []rules.Action) []agentview.RuleAction {
	out := make([]agentview.RuleAction, len(as))
	for i, x := range as {
		out[i] = agentview.RuleAction{Kind: agentview.RuleActionKind(x.Kind), Label: x.Label, Class: agentview.RuleClass(x.Class), Level: agentview.NotifyLevel(x.Level)}
	}
	return out
}

func warningsOut(ws []rules.Warning) []RuleWarning {
	out := make([]RuleWarning, len(ws))
	for i, w := range ws {
		out[i] = RuleWarning{Code: w.Code, Message: w.Message}
	}
	return out
}

func ruleOut(r store.Rule) Rule {
	return Rule{
		ID: r.ID, Name: r.Name, Priority: r.Priority, Enabled: r.Enabled, Revision: r.Revision,
		Match: agentview.MatchOf(r.Spec.Match), Actions: actionsOut(r.Spec.Actions), ProposalID: r.ProposalID,
		CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, DeletedAt: r.DeletedAt,
		Unsupported: r.Unsupported,
	}
}

func newRule(b RuleBody) store.NewRule {
	return store.NewRule{Name: b.Name, Priority: b.Priority, Spec: agentview.RuleSpec(b.Match, b.Actions), Disabled: b.Disabled}
}

const manageRules = "manages rules"

func (a *API) rules(ctx context.Context, v *auth.Viewer, p RulesParams) (RuleList, error) {
	if err := peopleOnly(v, manageRules); err != nil {
		return RuleList{}, err
	}
	rs, err := a.db.Rules(ctx, p.Deleted)
	if err != nil {
		return RuleList{}, err
	}
	out := RuleList{Rules: make([]Rule, len(rs))}
	for i, r := range rs {
		out.Rules[i] = ruleOut(r)
	}
	return out, nil
}

func (a *API) rule(ctx context.Context, v *auth.Viewer, p IDParams) (Rule, error) {
	if err := peopleOnly(v, manageRules); err != nil {
		return Rule{}, err
	}
	r, err := a.db.RuleByID(ctx, p.ID)
	if err != nil {
		return Rule{}, err
	}
	return ruleOut(r), nil
}

func (a *API) createRule(ctx context.Context, v *auth.Viewer, p CreateRuleParams) (SavedRule, error) {
	if err := peopleOnly(v, manageRules); err != nil {
		return SavedRule{}, err
	}
	b := p.Body
	r, warns, err := a.db.CreateRule(ctx, newRule(b),
		action(v, "rule.add", map[string]any{"name": b.Name, "priority": b.Priority, "disabled": b.Disabled}))
	if err != nil {
		return SavedRule{}, err
	}
	return SavedRule{Rule: ruleOut(r), Warnings: warningsOut(warns)}, nil
}

func (a *API) updateRule(ctx context.Context, v *auth.Viewer, p UpdateRuleParams) (SavedRule, error) {
	if err := peopleOnly(v, manageRules); err != nil {
		return SavedRule{}, err
	}
	b := p.Body
	enabled := !b.Disabled
	r, warns, err := a.db.UpdateRule(ctx, p.ID, store.RuleUpdate{
		Name: &b.Name, Priority: &b.Priority, Spec: agentview.RuleSpec(b.Match, b.Actions), Enabled: &enabled,
	}, action(v, "rule.set", map[string]any{"name": b.Name, "priority": b.Priority, "disabled": b.Disabled}))
	if err != nil {
		return SavedRule{}, err
	}
	return SavedRule{Rule: ruleOut(r), Warnings: warningsOut(warns)}, nil
}

func (a *API) deleteRule(ctx context.Context, v *auth.Viewer, p IDParams) (struct{}, error) {
	if err := peopleOnly(v, manageRules); err != nil {
		return struct{}{}, err
	}
	return struct{}{}, a.db.DeleteRule(ctx, p.ID, action(v, "rule.rm", nil))
}

func since(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func (a *API) applyRule(ctx context.Context, v *auth.Viewer, p RetroParams) (RetroStats, error) {
	if err := peopleOnly(v, manageRules); err != nil {
		return RetroStats{}, err
	}
	st, err := a.db.ApplyRule(ctx, p.ID, since(p.Body.Since), p.Body.DryRun, action(v, "rule.apply", nil))
	if err != nil {
		return RetroStats{}, err
	}
	return RetroStats(st), nil
}

func (a *API) revertRule(ctx context.Context, v *auth.Viewer, p RetroParams) (RevertStats, error) {
	if err := peopleOnly(v, manageRules); err != nil {
		return RevertStats{}, err
	}
	if p.Body.DryRun {
		return RevertStats{}, openapi.Errorf(http.StatusBadRequest, openapi.CodeInvalidBody, "dry_run: apply only")
	}
	st, err := a.db.RevertRule(ctx, p.ID, since(p.Body.Since), action(v, "rule.revert", nil))
	if err != nil {
		return RevertStats{}, err
	}
	return RevertStats(st), nil
}

func (a *API) previewRule(ctx context.Context, v *auth.Viewer, p PreviewParams) (RulePreview, error) {
	if err := peopleOnly(v, manageRules); err != nil {
		return RulePreview{}, err
	}
	b := p.Body
	pv, err := a.db.PreviewRule(ctx, v, agentview.RuleSpec(b.Match, b.Actions), store.PreviewQuery{Since: since(b.Since), Samples: b.Samples})
	if err != nil {
		return RulePreview{}, err
	}
	out := RulePreview{RetroStats: RetroStats(pv.RetroStats), Warnings: warningsOut(pv.Warnings), Samples: make([]MessageSummary, len(pv.Samples))}
	for i, m := range pv.Samples {
		out.Samples[i] = summary(v, m)
	}
	return out, nil
}

// draftRule offers the conditions of "mail like this" (rules.Generalize)
// with how many messages each matches, and the default selection.
func (a *API) draftRule(ctx context.Context, v *auth.Viewer, p RuleDraftParams) (RuleDraft, error) {
	if err := peopleOnly(v, manageRules); err != nil {
		return RuleDraft{}, err
	}
	if p.Body.MessageID == "" {
		return RuleDraft{}, openapi.Errorf(http.StatusBadRequest, openapi.CodeInvalidBody, "message_id: required")
	}
	fs, err := a.db.FactsFor(ctx, v, p.Body.MessageID)
	if err != nil {
		return RuleDraft{}, err
	}
	if len(fs) == 0 {
		return RuleDraft{}, openapi.Errorf(http.StatusUnprocessableEntity, openapi.CodeInvalidBody,
			"message_id: no delivery of it reached you; rules match arriving mail only")
	}
	cands := rules.Generalize(&fs[0])
	ms := make([]rules.Match, len(cands), len(cands)+1)
	for i, c := range cands {
		ms[i] = c.Condition
	}
	draft, ok := rules.Draft(cands)
	if ok {
		ms = append(ms, draft)
	}
	counts, err := a.db.CountMatches(ctx, v, ms, since(p.Body.Since))
	if err != nil {
		return RuleDraft{}, err
	}
	out := RuleDraft{MessageID: p.Body.MessageID, Candidates: make([]RuleCandidate, len(cands))}
	for i, c := range cands {
		out.Candidates[i] = RuleCandidate{
			Condition: RuleCondition{Field: c.Condition.Field, Op: c.Condition.Op, Value: c.Condition.Value},
			Selected:  c.Selected, Matches: counts[i],
		}
	}
	if ok {
		out.Match, out.Matches = agentview.MatchOf(draft), counts[len(cands)]
	}
	return out, nil
}

// proposalOut is a stored proposal as the API shows it.
func proposalOut(p store.Proposal) Proposal {
	kind, name, _ := strings.Cut(p.Proposer, ":")
	out := Proposal{
		ID: p.ID, Kind: agentview.ProposalKind(p.Kind), State: agentview.ProposalState(p.State),
		Proposer: Actor{ID: p.ProposedBy, Kind: kind, Name: name}, MessageID: p.MessageID,
		Rationale: p.Rationale, Cites: make([]Cite, len(p.Cites)),
		CreatedAt: p.CreatedAt, ExpiresAt: p.ExpiresAt, DecidedBy: p.DecidedBy, DecidedAt: p.DecidedAt,
		ActionID: p.ActionID, RuleID: p.RuleID,
	}
	for i, c := range p.Cites {
		out.Cites[i] = Cite{URI: c.URI, Quote: c.Quote}
	}
	out.Scope, out.Match, out.Actions = agentview.ProposalContent(p.Payload)
	return out
}

func (a *API) proposals(ctx context.Context, v *auth.Viewer, p ProposalsParams) (ProposalPage, error) {
	ps, err := a.db.Proposals(ctx, v, store.ProposalQuery{State: string(p.State), MessageID: p.MessageID, Before: p.Cursor, Limit: p.Limit})
	if err != nil {
		return ProposalPage{}, err
	}
	out := ProposalPage{Proposals: make([]Proposal, len(ps))}
	for i, pr := range ps {
		out.Proposals[i] = proposalOut(pr)
	}
	if len(ps) == p.Limit {
		out.NextCursor = ps[len(ps)-1].ID
	}
	return out, nil
}

func (a *API) proposal(ctx context.Context, v *auth.Viewer, p IDParams) (Proposal, error) {
	pr, err := a.db.ProposalByID(ctx, v, p.ID)
	if err != nil {
		return Proposal{}, err
	}
	return proposalOut(pr), nil
}

func (a *API) createProposal(ctx context.Context, v *auth.Viewer, p CreateProposalParams) (ProposalCreated, error) {
	b := p.Body
	cites := citesIn(b.Cites)
	if err := agentview.CheckCites(cites); err != nil {
		return ProposalCreated{}, openapi.Errorf(http.StatusBadRequest, openapi.CodeInvalidBody, "%v", err)
	}
	payload, err := agentview.ProposalPayload(b.Kind, b.Scope, b.Match, b.Actions)
	if err != nil {
		return ProposalCreated{}, openapi.Errorf(http.StatusBadRequest, openapi.CodeInvalidBody, "%v", err)
	}
	pr, warns, err := a.db.CreateProposal(ctx, v, store.NewProposal{
		Kind: string(b.Kind), MessageID: b.MessageID, Payload: payload, Rationale: b.Rationale, Cites: cites,
	}, action(v, "proposal.create", map[string]any{"kind": b.Kind, "message": b.MessageID, "cites": len(b.Cites)}))
	if err != nil {
		return ProposalCreated{}, err
	}
	return ProposalCreated{Proposal: proposalOut(pr), Warnings: warningsOut(warns)}, nil
}

const decideProposals = "decides proposals"

func (a *API) acceptProposal(ctx context.Context, v *auth.Viewer, p AcceptParams) (AcceptResult, error) {
	if err := peopleOnly(v, decideProposals); err != nil {
		return AcceptResult{}, err
	}
	pr, err := a.db.ProposalByID(ctx, v, p.ID)
	if err != nil {
		return AcceptResult{}, err
	}
	// A rule is a standing capability: creating one needs rule:manage,
	// whichever way it comes.
	if (p.Body.Rule != nil || pr.Kind == store.ProposalRule) && !v.Has(auth.RuleManage) {
		return AcceptResult{}, openapi.Errorf(http.StatusForbidden, openapi.CodeScopeRequired,
			"creating a rule needs %s", auth.RuleManage)
	}
	var rule *store.NewRule
	if b := p.Body.Rule; b != nil {
		n := newRule(*b)
		rule = &n
	}
	acc, err := a.db.AcceptProposal(ctx, v, p.ID, rule, action(v, "proposal.accept", map[string]any{"automate": rule != nil}))
	if err != nil {
		return AcceptResult{}, err
	}
	if pr, err = a.db.ProposalByID(ctx, v, p.ID); err != nil {
		return AcceptResult{}, err
	}
	out := AcceptResult{Proposal: proposalOut(pr), Messages: messagesOutcomes(acc.Annotations)}
	if acc.Rule != nil {
		out.Rule = &SavedRule{Rule: ruleOut(*acc.Rule), Warnings: warningsOut(acc.Warnings)}
	}
	return out, nil
}

func (a *API) rejectProposal(ctx context.Context, v *auth.Viewer, p IDParams) (Proposal, error) {
	if err := peopleOnly(v, decideProposals); err != nil {
		return Proposal{}, err
	}
	if err := a.db.RejectProposal(ctx, v, p.ID, action(v, "proposal.reject", nil)); err != nil {
		return Proposal{}, err
	}
	return a.proposal(ctx, v, p)
}

func (a *API) withdrawProposal(ctx context.Context, v *auth.Viewer, p IDParams) (Proposal, error) {
	if err := a.db.WithdrawProposal(ctx, v, p.ID, action(v, "proposal.withdraw", nil)); err != nil {
		return Proposal{}, err
	}
	return a.proposal(ctx, v, p)
}
