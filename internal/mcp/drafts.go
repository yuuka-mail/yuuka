package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/submit"
)

// The write tools of M2 (§8.7, §8.9): an agent writes drafts and submits
// them; whether a submission goes out is decided by the grant on its
// From identity, through the same submission core as REST and the SMTP
// port. Being held as a draft is a result, never an error, so the agent
// has no reason to try another way.

// Submitter is the submission core (submit.Service).
type Submitter interface {
	SubmitDraft(ctx context.Context, v *auth.Viewer, draftID string) (submit.Result, error)
}

// IdentitiesInput is the input of list_identities: none.
type IdentitiesInput struct{}

// Identity is one address the caller may send as.
type Identity struct {
	// Address is local@domain, or *@domain: any local part there.
	Address string `json:"address"`
	// Own: the identity belongs to the caller.
	Own bool `json:"own"`
	// Tier is the effective send_policy for the caller (§8.9).
	Tier string `json:"tier"`
	// Sends: a submission goes out without a person, within
	// RecipientScope and the limits. Otherwise every submission is held
	// as a draft for a person, and HoldReason says why.
	Sends      bool   `json:"sends"`
	HoldReason string `json:"hold_reason,omitempty"`
	HoldDetail string `json:"hold_detail,omitempty"`
	// HoldSeconds is the recall window under review (hold_reason
	// review): mail the checks pass is sent this long after submission
	// unless a person recalls it.
	HoldSeconds    *int   `json:"hold_seconds,omitempty"`
	RecipientScope string `json:"recipient_scope"`
}

// IdentitiesOutput is the structured result of list_identities.
type IdentitiesOutput struct {
	Identities []Identity `json:"identities"`
}

func (s *Server) listIdentities(ctx context.Context, v *auth.Viewer, _ IdentitiesInput) (result, error) {
	ss, err := s.db.Standings(ctx, v)
	if err != nil {
		return result{}, err
	}
	out := IdentitiesOutput{Identities: []Identity{}}
	var b strings.Builder
	for _, st := range ss {
		if st.Tier == sendpolicy.None {
			continue
		}
		id := Identity{
			Address: st.Identity.Address(), Own: st.Identity.OwnerID == v.PrincipalID, Tier: string(st.Tier),
			Sends: st.Hold == "", HoldReason: string(st.Hold), HoldDetail: st.HoldDetail,
			RecipientScope: string(st.Grant.RecipientScope),
		}
		if st.Tier == sendpolicy.Review && st.Hold == sendpolicy.HoldReview {
			id.HoldSeconds = &st.HoldSeconds
		}
		out.Identities = append(out.Identities, id)
		fmt.Fprintf(&b, "%s · tier %s · recipients %s · %s\n", id.Address, id.Tier, id.RecipientScope, standing(id.Sends, id.HoldReason, id.HoldDetail))
	}
	text := fmt.Sprintf("%d identities you may write as (identities are the operator's configuration):\n", len(out.Identities)) + b.String()
	if len(out.Identities) == 0 {
		text = "There is no identity you may write as.\n"
	}
	return result{structured: out, text: text, audit: map[string]any{"returned": len(out.Identities)}}, nil
}

func standing(sends bool, reason, detail string) string {
	if sends {
		return "a submission is sent if its recipients are within scope and limits"
	}
	return fmt.Sprintf("every submission is held as a draft for a person (%s: %s)", reason, detail)
}

// Cite is a passage a draft relies on.
type Cite struct {
	URI   string `json:"uri" jsonschema:"yuuka://message/<id>#part/<section>/para/<n>, as get_message gives it"`
	Quote string `json:"quote,omitempty" jsonschema:"the quoted text"`
}

// DraftInput is the content of a draft.
type DraftInput struct {
	From         string   `json:"from,omitempty" jsonschema:"the address to send from, one of list_identities; without it a reply goes out as the address the replied-to mail came in through"`
	To           []string `json:"to,omitempty" jsonschema:"bare addresses"`
	Cc           []string `json:"cc,omitempty" jsonschema:"bare addresses"`
	Bcc          []string `json:"bcc,omitempty" jsonschema:"bare addresses"`
	Subject      string   `json:"subject,omitempty"`
	BodyMarkdown string   `json:"body_markdown,omitempty" jsonschema:"the body as Markdown; the identity's signature is added when it is sent"`
	InReplyTo    string   `json:"in_reply_to,omitempty" jsonschema:"yuuka://message/<id> of the message this replies to"`
	Cites        []Cite   `json:"cites,omitempty" jsonschema:"the passages the draft relies on; whoever reviews the draft checks them"`
}

// auditArgs names the draft's addressing, never its text: the audit log
// is not a copy of the mail.
func (in DraftInput) auditArgs() map[string]any {
	return map[string]any{
		"from": in.From, "in_reply_to": in.InReplyTo, "recipients": len(in.To) + len(in.Cc) + len(in.Bcc), "cites": len(in.Cites),
	}
}

// UpdateDraftInput is the input of update_draft.
type UpdateDraftInput struct {
	DraftID string `json:"draft_id" jsonschema:"the draft_id create_draft returned"`
	DraftInput
}

func (in UpdateDraftInput) auditArgs() map[string]any {
	a := in.DraftInput.auditArgs()
	a["draft_id"] = in.DraftID
	return a
}

// DraftOutput is the structured result of create_draft and update_draft.
type DraftOutput struct {
	DraftID   string   `json:"draft_id"`
	From      string   `json:"from"`
	To        []string `json:"to"`
	Cc        []string `json:"cc"`
	Bcc       []string `json:"bcc"`
	Subject   string   `json:"subject"`
	InReplyTo string   `json:"in_reply_to,omitempty"`
	// Sends and HoldReason say how submitting it as From would go now,
	// before its recipients are looked at (list_identities).
	Sends      bool   `json:"sends"`
	HoldReason string `json:"hold_reason,omitempty"`
	HoldDetail string `json:"hold_detail,omitempty"`
}

func (s *Server) createDraft(ctx context.Context, v *auth.Viewer, in DraftInput) (result, error) {
	d, err := s.draftOf(ctx, v, in, "")
	if err != nil {
		return result{}, err
	}
	d, err = s.db.CreateDraft(ctx, v, d, s.now())
	if err != nil {
		return result{}, draftErr(err)
	}
	return s.draftResult(ctx, v, d, "created")
}

func (s *Server) updateDraft(ctx context.Context, v *auth.Viewer, in UpdateDraftInput) (result, error) {
	from := in.From
	if from == "" {
		cur, err := s.db.DraftByID(ctx, v, in.DraftID)
		if err != nil {
			return result{}, draftErr(err)
		}
		from = cur.FromAddr
	}
	d, err := s.draftOf(ctx, v, in.DraftInput, from)
	if err != nil {
		return result{}, err
	}
	d.ID = in.DraftID
	d, err = s.db.UpdateDraft(ctx, v, d, s.now())
	if err != nil {
		return result{}, draftErr(err)
	}
	return s.draftResult(ctx, v, d, "updated")
}

// draftOf turns the input into the store's draft, as the REST API does:
// From, else from, else the reply identity of the replied-to message.
func (s *Server) draftOf(ctx context.Context, v *auth.Viewer, in DraftInput, from string) (store.Draft, error) {
	cites := in.Cites
	if cites == nil {
		cites = []Cite{}
	}
	for _, c := range cites {
		if _, _, err := agentview.ParseURI(c.URI); err != nil {
			return store.Draft{}, refuse("cites: %v", err)
		}
	}
	raw, err := json.Marshal(cites)
	if err != nil {
		return store.Draft{}, err
	}
	d := store.Draft{
		FromAddr: in.From, To: in.To, Cc: in.Cc, Bcc: in.Bcc, Subject: in.Subject, BodyMD: in.BodyMarkdown, Cites: raw,
	}
	if in.InReplyTo != "" {
		id, _, err := agentview.ParseURI(in.InReplyTo)
		if err != nil {
			return store.Draft{}, refuse("in_reply_to: %v", err)
		}
		d.InReplyToID = id
	}
	if d.FromAddr == "" {
		d.FromAddr = from
	}
	if d.FromAddr == "" {
		if d.InReplyToID == "" {
			return store.Draft{}, refuse("from is required for a new thread; list_identities says which addresses you may use")
		}
		cands, err := s.db.ReplyIdentities(ctx, v, d.InReplyToID)
		if errors.Is(err, store.ErrNotFound) {
			return store.Draft{}, refuse("no message %s", in.InReplyTo)
		}
		if err != nil {
			return store.Draft{}, err
		}
		if len(cands) == 0 {
			return store.Draft{}, refuse("no identity covers the address %s came in through; give from", in.InReplyTo)
		}
		d.FromAddr = cands[0].FromAddr
	}
	return d, nil
}

func (s *Server) draftResult(ctx context.Context, v *auth.Viewer, d store.Draft, verb string) (result, error) {
	out := DraftOutput{
		DraftID: d.ID, From: d.FromAddr, To: nonNil(d.To), Cc: nonNil(d.Cc), Bcc: nonNil(d.Bcc), Subject: d.Subject,
	}
	if d.InReplyToID != "" {
		out.InReplyTo = agentview.MessageURI(d.InReplyToID)
	}
	ss, err := s.db.Standings(ctx, v)
	if err != nil {
		return result{}, err
	}
	out.HoldReason, out.HoldDetail = string(sendpolicy.HoldPolicy), "no grant to send as this identity"
	for _, st := range ss {
		if st.Identity.ID == d.IdentityID && st.Tier != sendpolicy.None {
			out.Sends, out.HoldReason, out.HoldDetail = st.Hold == "", string(st.Hold), st.HoldDetail
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Draft %s %s. Nothing is sent until submit_draft; as its From, %s.\n\n", d.ID, verb,
		standing(out.Sends, out.HoldReason, out.HoldDetail))
	// The addressing may be copied from mail, and a default From names
	// a local part a sender chose: it is fenced like mail content.
	var addr strings.Builder
	fmt.Fprintf(&addr, "From: %s\nTo: %s\n", out.From, strings.Join(out.To, ", "))
	if len(out.Cc) > 0 {
		fmt.Fprintf(&addr, "Cc: %s\n", strings.Join(out.Cc, ", "))
	}
	if len(out.Bcc) > 0 {
		fmt.Fprintf(&addr, "Bcc: %s\n", strings.Join(out.Bcc, ", "))
	}
	fmt.Fprintf(&addr, "Subject: %s\n", out.Subject)
	if out.InReplyTo != "" {
		fmt.Fprintf(&addr, "In reply to: %s\n", out.InReplyTo)
	}
	b.WriteString(agentview.Fence(d.ID, addr.String()))
	return result{structured: out, text: b.String(), audit: map[string]any{"draft_id": d.ID, "identity_id": d.IdentityID}}, nil
}

// SubmitInput is the input of submit_draft.
type SubmitInput struct {
	DraftID string `json:"draft_id" jsonschema:"the draft_id create_draft returned"`
}

// SubmitOutput is where a submission went.
type SubmitOutput struct {
	// State is queued (it will be sent), held (it passed the review and
	// is sent at hold_until unless a person recalls it first) or draft
	// (stopped for a person, with hold_reason).
	State string `json:"state"`
	// Queued: state queued.
	Queued   bool   `json:"queued"`
	DraftID  string `json:"draft_id"`
	OutboxID string `json:"outbox_id,omitempty"`
	// HoldUntil ends the recall window, in state held.
	HoldUntil *time.Time `json:"hold_until,omitempty"`
	// HoldReason and HoldDetail say why it was not sent straight away.
	HoldReason string `json:"hold_reason,omitempty"`
	HoldDetail string `json:"hold_detail,omitempty"`
	// FailedChecks are the review checks that escalated it to a person
	// (hold_reason review, state draft).
	FailedChecks []string `json:"failed_checks,omitempty"`
}

func (s *Server) submitDraft(ctx context.Context, v *auth.Viewer, in SubmitInput) (result, error) {
	res, err := s.submit.SubmitDraft(ctx, v, in.DraftID)
	if err != nil {
		return result{}, draftErr(err)
	}
	out := SubmitOutput{
		State: "draft", Queued: res.Queued, DraftID: in.DraftID, OutboxID: res.OutboxID, HoldReason: res.HoldReason, HoldDetail: res.Detail,
	}
	var text string
	switch {
	case res.Queued:
		out.State = "queued"
		text = fmt.Sprintf("Queued for sending as outbox %s. Nothing more to do.\n", out.OutboxID)
	case res.Held:
		out.State = "held"
		// The window was set by the database's clock: read it back.
		if e, err := s.db.OutboxByID(ctx, v, res.OutboxID); err == nil {
			out.HoldUntil = e.HoldUntil
		} else if !errors.Is(err, store.ErrNotFound) {
			return result{}, err
		}
		when := "the end of its recall window"
		if out.HoldUntil != nil {
			when = out.HoldUntil.UTC().Format(time.RFC3339)
		}
		text = fmt.Sprintf("Passed review; goes out at %s unless recalled. It waits as outbox %s until then. Nothing more to do.\n",
			when, out.OutboxID)
	default:
		if res.HoldReason == string(sendpolicy.HoldReview) {
			if out.FailedChecks, err = s.failedChecks(ctx, v, in.DraftID); err != nil {
				return result{}, err
			}
		}
		var failed string
		if len(out.FailedChecks) > 0 {
			failed = "Review checks that did not pass: " + strings.Join(out.FailedChecks, ", ") + ".\n"
		}
		text = fmt.Sprintf("Not sent: held as draft %s (%s): %s\n%sThis is a normal result, not an error: a person reviews held drafts. "+
			"Do not try to send it another way.\n", out.DraftID, out.HoldReason, out.HoldDetail, failed)
	}
	audit := map[string]any{"queued": out.Queued, "state": out.State, "outbox_id": out.OutboxID, "hold_reason": out.HoldReason}
	return result{structured: out, text: text, audit: audit}, nil
}

// failedChecks names the checks of the verdict stored on draft id that
// did not pass, in their order.
func (s *Server) failedChecks(ctx context.Context, v *auth.Viewer, id string) ([]string, error) {
	d, err := s.db.DraftByID(ctx, v, id)
	if err != nil {
		return nil, err
	}
	var verdict sendpolicy.Verdict
	if len(d.Review) == 0 || json.Unmarshal(d.Review, &verdict) != nil {
		return nil, nil
	}
	var out []string
	for _, c := range verdict.Checks {
		if !c.Pass {
			out = append(out, c.Name)
		}
	}
	return out, nil
}

// draftErr turns the store's and the core's refusals into answers the
// caller can act on; anything else stays internal.
func draftErr(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return refuse("no such draft, or not one of yours")
	case errors.Is(err, store.ErrInFlight):
		return refuse("the draft has already gone to the outbox")
	case errors.Is(err, store.ErrRawDraft):
		return refuse("this draft is a held SMTP submission; it cannot be edited")
	case errors.Is(err, store.ErrNoIdentity), errors.Is(err, submit.ErrNoIdentity):
		return refuse("no identity covers this From address; list_identities says which you may use")
	case errors.Is(err, store.ErrInvalidDraft), errors.Is(err, submit.ErrRejected):
		return refuse("%v", err)
	}
	return err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
