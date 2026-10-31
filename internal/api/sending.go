package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/openapi"
	"github.com/yuuka-mail/yuuka/internal/outbox"
	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/submit"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// The write side of M2 (§8.9): drafts are written through the store,
// submitted through the submission core and nothing else, and the outbox
// is read here and resolved by a person. Exceeding a grant is a held
// draft, a success with the reason in it, never an error.

// Submitter is the submission core (submit.Service), the only way into
// the outbox. SubmitDraft decides on draft id as v, the author or a
// human confirming it (§8.9), and queues it or holds it as a draft.
type Submitter interface {
	SubmitDraft(ctx context.Context, v *auth.Viewer, draftID string) (submit.Result, error)
}

func (a *API) createDraft(ctx context.Context, v *auth.Viewer, p CreateDraftParams) (Draft, error) {
	d, err := a.draftOf(ctx, v, p.Body, "")
	if err != nil {
		return Draft{}, err
	}
	d, err = a.db.CreateDraft(ctx, v, d, time.Now())
	if err != nil {
		return Draft{}, err
	}
	return draftOut(d), nil
}

func (a *API) updateDraft(ctx context.Context, v *auth.Viewer, p UpdateDraftParams) (Draft, error) {
	from := p.Body.From
	if from == "" {
		cur, err := a.db.DraftByID(ctx, v, p.ID)
		if err != nil {
			return Draft{}, err
		}
		from = cur.FromAddr
	}
	d, err := a.draftOf(ctx, v, p.Body, from)
	if err != nil {
		return Draft{}, err
	}
	d.ID = p.ID
	d, err = a.db.UpdateDraft(ctx, v, d, time.Now())
	if err != nil {
		return Draft{}, err
	}
	return draftOut(d), nil
}

// draftOf turns a body into the store's draft. Without a From, from is
// used; without that either, a reply goes out as the address its
// message came in through (ReplyIdentities, §9.3), and a new thread
// needs one.
func (a *API) draftOf(ctx context.Context, v *auth.Viewer, b DraftBody, from string) (store.Draft, error) {
	cites := b.Cites
	if cites == nil {
		cites = []Cite{}
	}
	// Bounded as a proposal's or a note's: the review reads them.
	if err := agentview.CheckCites(citesIn(cites)); err != nil {
		return store.Draft{}, openapi.Errorf(http.StatusBadRequest, openapi.CodeInvalidBody, "%v", err)
	}
	raw, err := json.Marshal(cites)
	if err != nil {
		return store.Draft{}, err
	}
	d := store.Draft{
		FromAddr: b.From, InReplyToID: b.InReplyTo, To: b.To, Cc: b.Cc, Bcc: b.Bcc,
		Subject: b.Subject, BodyMD: b.BodyMarkdown, Cites: raw,
	}
	if d.FromAddr == "" {
		d.FromAddr = from
	}
	if d.FromAddr == "" {
		if d.InReplyToID == "" {
			return store.Draft{}, openapi.Errorf(http.StatusBadRequest, openapi.CodeInvalidBody, "from: required for a new thread")
		}
		cands, err := a.db.ReplyIdentities(ctx, v, d.InReplyToID)
		if errors.Is(err, store.ErrNotFound) {
			return store.Draft{}, openapi.Errorf(http.StatusBadRequest, openapi.CodeInvalidBody, "in_reply_to: no message %s", d.InReplyToID)
		}
		if err != nil {
			return store.Draft{}, err
		}
		if len(cands) == 0 {
			return store.Draft{}, openapi.Errorf(http.StatusUnprocessableEntity, openapi.CodeNoIdentity,
				"no identity covers the addresses message %s came in through; give from", d.InReplyToID)
		}
		d.FromAddr = cands[0].FromAddr
	}
	return d, nil
}

func (a *API) draft(ctx context.Context, v *auth.Viewer, p IDParams) (Draft, error) {
	d, err := a.db.DraftByID(ctx, v, p.ID)
	if err != nil {
		return Draft{}, err
	}
	return draftOut(d), nil
}

var draftStates = map[DraftState]func(*store.DraftQuery){
	"draft":  func(q *store.DraftQuery) { q.Open = true },
	"held":   func(q *store.DraftQuery) { q.Held = true },
	"outbox": func(q *store.DraftQuery) { q.InFlight = true },
}

func (a *API) drafts(ctx context.Context, v *auth.Viewer, p DraftsParams) (DraftPage, error) {
	q := store.DraftQuery{Limit: p.Limit}
	for _, s := range p.State {
		draftStates[s](&q)
	}
	if p.Cursor != nil {
		q.Before = &store.DraftCursor{UpdatedAt: p.Cursor.ReceivedAt, ID: p.Cursor.ID}
	}
	ds, err := a.db.Drafts(ctx, v, q)
	if err != nil {
		return DraftPage{}, err
	}
	page := DraftPage{Drafts: make([]Draft, len(ds))}
	for i, d := range ds {
		page.Drafts[i] = draftOut(d)
	}
	if len(ds) == p.Limit {
		last := ds[len(ds)-1]
		page.NextCursor = Cursor{ReceivedAt: last.UpdatedAt, ID: last.ID}.String()
	}
	return page, nil
}

func (a *API) deleteDraft(ctx context.Context, v *auth.Viewer, p IDParams) (struct{}, error) {
	return struct{}{}, a.db.DeleteDraft(ctx, v, p.ID, time.Now())
}

func draftOut(d store.Draft) Draft {
	out := Draft{
		ID: d.ID, Author: Actor{ID: d.PrincipalID, Kind: d.AuthorKind, Name: d.AuthorName},
		State: "draft", IdentityID: d.IdentityID, From: d.FromAddr,
		IdentityOwner: Actor{ID: d.IdentityOwner.ID, Kind: d.IdentityOwner.Kind, Name: d.IdentityOwner.Name},
		To:            nonNil(d.To), Cc: nonNil(d.Cc), Bcc: nonNil(d.Bcc), Subject: d.Subject,
		BodyMarkdown: d.BodyMD, SMTP: d.RawID != "", InReplyTo: d.InReplyToID, Cites: []Cite{},
		CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
	}
	// Cites are written through draftOf, or [] by the store; anything
	// else is left out rather than failing the read.
	var cites []Cite
	if json.Unmarshal(d.Cites, &cites) == nil && cites != nil {
		out.Cites = cites
	}
	switch {
	case d.OutboxID != "":
		out.State, out.Outbox = "outbox", &Outgoing{OutboxID: d.OutboxID, State: OutboxState(d.OutboxState)}
	case d.HoldReason != "":
		out.State, out.HoldReason, out.HoldDetail = "held", HoldReason(d.HoldReason), d.HoldDetail
		out.Review = reviewOut(d.Review)
	}
	return out
}

// reviewOut is a stored verdict (sendpolicy.Verdict as JSON), nil for
// none. The verdict holds where a code or link of incoming mail was
// found, never the value, and Review has no field for one either.
func reviewOut(raw json.RawMessage) *Review {
	var v sendpolicy.Verdict
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return nil
	}
	out := &Review{Checks: make([]ReviewCheck, len(v.Checks))}
	for i, c := range v.Checks {
		rc := ReviewCheck{
			Name: CheckName(c.Name), Pass: c.Pass, Advisory: c.Advisory, Detail: c.Detail, Recipients: c.Recipients, Unreadable: c.Unreadable,
		}
		for _, x := range c.Extracted {
			rc.Extracted = append(rc.Extracted, ReviewExtracted{
				Kind: agentview.ExtractionKind(x.Kind), MessageID: x.MessageID, Source: agentview.ExtractionSource(x.Source), Where: x.Where,
			})
		}
		for _, r := range c.Refs {
			rc.Refs = append(rc.Refs, ReviewRef{As: r.As, Where: r.Where, MessageID: r.MessageID})
		}
		for _, l := range c.Links {
			rc.Links = append(rc.Links, ReviewLink{Host: l.Host, Where: l.Where, Domain: l.Domain})
		}
		out.Checks[i] = rc
	}
	return out
}

// submitDraft submits draft id through the core. Queued or held, it is a
// success: the result says which and why (§8.9).
func (a *API) submitDraft(ctx context.Context, v *auth.Viewer, p IDParams) (SubmitResult, error) {
	res, err := a.submit.SubmitDraft(ctx, v, p.ID)
	if err != nil {
		return SubmitResult{}, err
	}
	out := SubmitResult{
		State: "draft", Queued: res.Queued, DraftID: p.ID, OutboxID: res.OutboxID,
		HoldReason: HoldReason(res.HoldReason), HoldDetail: res.Detail,
	}
	switch {
	case res.Queued:
		out.State = "queued"
	case res.Held:
		// The window was set by the database's clock: read it back
		// rather than add HoldSeconds to this one's. The submission has
		// committed, so a row the viewer cannot read back (an account it
		// may not read) leaves hold_until out rather than failing it.
		out.State = "held"
		e, err := a.db.OutboxByID(ctx, v, res.OutboxID)
		switch {
		case err == nil:
			out.HoldUntil = e.HoldUntil
		case !errors.Is(err, store.ErrNotFound):
			return SubmitResult{}, err
		}
	}
	return out, nil
}

func (a *API) identities(ctx context.Context, v *auth.Viewer, _ struct{}) (IdentityList, error) {
	ss, err := a.db.Standings(ctx, v)
	if err != nil {
		return IdentityList{}, err
	}
	out := IdentityList{Identities: []SendingIdentity{}}
	for _, s := range ss {
		if s.Tier != sendpolicy.None {
			out.Identities = append(out.Identities, sendingIdentity(s))
		}
	}
	return out, nil
}

func (a *API) replyIdentities(ctx context.Context, v *auth.Viewer, p IDParams) (ReplyIdentities, error) {
	cands, err := a.db.ReplyIdentities(ctx, v, p.ID)
	if err != nil {
		return ReplyIdentities{}, err
	}
	ss, err := a.db.Standings(ctx, v)
	if err != nil {
		return ReplyIdentities{}, err
	}
	byID := map[string]store.Standing{}
	for _, s := range ss {
		byID[s.Identity.ID] = s
	}
	out := ReplyIdentities{Candidates: []ReplyIdentity{}}
	for _, c := range cands {
		s, ok := byID[c.Identity.ID]
		if !ok { // disabled between the two reads
			continue
		}
		out.Candidates = append(out.Candidates, ReplyIdentity{From: c.FromAddr, Identity: sendingIdentity(s)})
	}
	return out, nil
}

func sendingIdentity(s store.Standing) SendingIdentity {
	i, g := s.Identity, s.Grant
	var hold *int
	if s.Tier == sendpolicy.Review && s.Hold == sendpolicy.HoldReview {
		hold = &s.HoldSeconds
	}
	return SendingIdentity{
		HoldSeconds: hold,
		ID:          i.ID, Address: i.Address(), DisplayName: i.DisplayName,
		Owner: Actor{ID: i.OwnerID, Kind: i.OwnerKind, Name: i.OwnerName},
		Tier:  Tier(s.Tier), Granted: s.Granted, OwnerCapped: s.Capped,
		RecipientScope: RecipientScope(g.RecipientScope), Allowlist: nonNil(g.Allowlist),
		Limits: Limits{
			MaxPerHour: g.Limits.MaxPerHour, MaxPerDay: g.Limits.MaxPerDay,
			MaxNewRecipientsPerDay: g.Limits.MaxNewRecipientsPerDay,
		},
		Suspended: g.Suspended, Sends: s.Hold == "", HoldReason: HoldReason(s.Hold), HoldDetail: s.HoldDetail,
	}
}

func (a *API) outboxList(ctx context.Context, v *auth.Viewer, p OutboxParams) (OutboxPage, error) {
	q := store.OutboxQuery{Before: p.Cursor, Limit: p.Limit}
	for _, s := range p.State {
		q.States = append(q.States, string(s))
	}
	rows, err := a.db.OutboxList(ctx, v, q)
	if err != nil {
		return OutboxPage{}, err
	}
	page := OutboxPage{Entries: make([]OutboxEntry, len(rows))}
	for i, r := range rows {
		page.Entries[i] = outboxOut(r)
	}
	if len(rows) == p.Limit {
		page.NextCursor = rows[len(rows)-1].ID
	}
	return page, nil
}

func (a *API) outboxEntry(ctx context.Context, v *auth.Viewer, p IDParams) (OutboxEntry, error) {
	e, err := a.db.OutboxByID(ctx, v, p.ID)
	if err != nil {
		return OutboxEntry{}, err
	}
	return outboxOut(e), nil
}

// resolveOutbox is the human decision on mail whose delivery is unknown
// (§4.5), the REST form of `yuuka outbox resolve`. A person decides: the
// remote may hold the message already, so resending risks a duplicate
// that no policy can weigh. It needs submission:send, since resend
// sends, and is recorded as outbox.resolve like the CLI's.
func (a *API) resolveOutbox(ctx context.Context, v *auth.Viewer, p ResolveParams) (OutboxEntry, error) {
	if v.PrincipalKind != sendpolicy.KindHuman {
		return OutboxEntry{}, openapi.Errorf(http.StatusForbidden, openapi.CodeForbidden,
			"only a person decides on mail whose delivery is unknown")
	}
	if p.Body.Resolution == "" {
		return OutboxEntry{}, openapi.Errorf(http.StatusBadRequest, openapi.CodeInvalidBody, "resolution: required")
	}
	if _, err := a.db.OutboxByID(ctx, v, p.ID); err != nil {
		return OutboxEntry{}, err
	}
	now := time.Now()
	_, err := a.db.ResolveOutbox(ctx, p.ID, string(p.Body.Resolution), store.Action{
		ID: ulid.New(now), Actor: v.PrincipalID, CredentialID: v.CredentialID, Tool: "outbox.resolve", At: now,
		Args: map[string]any{"via": "api"},
	}, outbox.DefaultRetry)
	if err != nil {
		return OutboxEntry{}, err
	}
	e, err := a.db.OutboxByID(ctx, v, p.ID)
	if err != nil {
		return OutboxEntry{}, err
	}
	return outboxOut(e), nil
}

// withdrawOutbox recalls held mail during its recall window (§8.9): it
// will not be sent, and its draft can be edited and submitted again.
// People may recall any row they can see, agents and services only what
// they submitted. It is the only thing this API does to held mail:
// nothing here releases it or shortens its window.
func (a *API) withdrawOutbox(ctx context.Context, v *auth.Viewer, p IDParams) (Withdrawal, error) {
	w, err := a.db.WithdrawOutbox(ctx, v, p.ID, map[string]any{"via": "rest"}, time.Now())
	if errors.Is(err, store.ErrOutboxState) {
		e, err := a.db.OutboxByID(ctx, v, p.ID)
		if err != nil {
			return Withdrawal{}, err
		}
		if e.State == store.OutboxWithdrawn {
			return Withdrawal{}, openapi.Errorf(http.StatusConflict, openapi.CodeConflict, "already withdrawn")
		}
		return Withdrawal{}, openapi.Errorf(http.StatusConflict, openapi.CodeConflict,
			"already released (it is %s): only held mail can be withdrawn", e.State)
	}
	if err != nil {
		return Withdrawal{}, err
	}
	out := Withdrawal{OutboxID: w.OutboxID, DraftID: w.DraftID}
	if s := w.Suspended; s != nil {
		out.Suspended = &Suspension{IdentityID: s.IdentityID, PrincipalID: s.PrincipalID, Recalls: s.Recalls, Withdrawn: nonNil(s.Withdrawn)}
		if a.suspended != nil {
			a.suspended.Suspended(ctx, *s)
		}
	}
	return out, nil
}

func outboxOut(e store.OutboxEntry) OutboxEntry {
	out := OutboxEntry{
		ID: e.ID, State: OutboxState(e.State), Identity: e.Identity, IdentityID: e.IdentityID, From: e.FromAddr,
		DraftID: e.DraftID, MessageID: e.MessageID, Subject: e.Subject,
		SubmittedBy: Actor{ID: e.SubmittedBy.ID, Kind: e.SubmittedBy.Kind, Name: e.SubmittedBy.Name},
		CreatedAt:   e.CreatedAt, NextRetryAt: e.NextRetryAt, Error: e.Error,
		Recipients: make([]OutboxRecipient, len(e.Recipients)), Attempts: e.Attempts,
		LastOutcome: AttemptOutcome(e.LastOutcome),
	}
	if e.ApprovedBy.ID != "" {
		out.ApprovedBy = &Actor{ID: e.ApprovedBy.ID, Kind: e.ApprovedBy.Kind, Name: e.ApprovedBy.Name}
	}
	if e.WithdrawnBy.ID != "" {
		out.WithdrawnBy = &Actor{ID: e.WithdrawnBy.ID, Kind: e.WithdrawnBy.Kind, Name: e.WithdrawnBy.Name}
	}
	out.HoldUntil, out.Review = e.HoldUntil, reviewOut(e.Review)
	for i, r := range e.Recipients {
		out.Recipients[i] = OutboxRecipient{Addr: r.Addr, Kind: RecipientKind(r.Kind), State: RecipientState(r.State)}
	}
	return out
}

func outgoingOut(o *store.Outgoing) *Outgoing {
	if o == nil {
		return nil
	}
	return &Outgoing{OutboxID: o.OutboxID, State: OutboxState(o.State)}
}
