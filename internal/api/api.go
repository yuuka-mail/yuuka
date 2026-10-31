// Package api serves the REST API under /api/v1/ for the WebUI (session
// cookie) and for agents and services (bearer token). Every route states
// the scopes it needs, and every store read takes the request's viewer, so
// visibility (§13.8) is decided by the store and nowhere here.
//
// The route table is the single source of the contract (§5.1): it serves
// the requests, the ACL test walks it, and openapi.json is generated from
// it and the types in types.go (go generate ./internal/api).
package api

//go:generate go run ./openapigen -o openapi.json

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authn"
	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/classify"
	"github.com/yuuka-mail/yuuka/internal/leak"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/openapi"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/submit"
)

// Store is the slice of the store the API uses. Every read and every
// draft write takes the viewer; the ACL test wraps the real store to
// check that it arrives.
type Store interface {
	ListMessages(ctx context.Context, v *auth.Viewer, q store.MessageQuery) ([]store.MessageSummary, error)
	MessageByID(ctx context.Context, v *auth.Viewer, id string) (store.MessageDetail, error)
	ThreadByID(ctx context.Context, v *auth.Viewer, id string) (store.ThreadView, error)
	PrefixStats(ctx context.Context, v *auth.Viewer, q store.PrefixQuery) (store.PrefixSummary, error)
	PrefixByAddress(ctx context.Context, v *auth.Viewer, zone, prefix string, limit int) (store.PrefixDetail, error)
	Search(ctx context.Context, v *auth.Viewer, q store.SearchQuery) (store.SearchResult, error)
	// RecordAction appends to the audit log (attachment downloads).
	RecordAction(ctx context.Context, a store.Action) error

	DraftByID(ctx context.Context, v *auth.Viewer, id string) (store.Draft, error)
	Drafts(ctx context.Context, v *auth.Viewer, q store.DraftQuery) ([]store.Draft, error)
	CreateDraft(ctx context.Context, v *auth.Viewer, d store.Draft, at time.Time) (store.Draft, error)
	UpdateDraft(ctx context.Context, v *auth.Viewer, d store.Draft, at time.Time) (store.Draft, error)
	DeleteDraft(ctx context.Context, v *auth.Viewer, id string, at time.Time) error
	ReplyIdentities(ctx context.Context, v *auth.Viewer, id string) ([]store.ReplyIdentity, error)
	Standings(ctx context.Context, v *auth.Viewer) ([]store.Standing, error)
	OutboxList(ctx context.Context, v *auth.Viewer, q store.OutboxQuery) ([]store.OutboxEntry, error)
	OutboxByID(ctx context.Context, v *auth.Viewer, id string) (store.OutboxEntry, error)
	// ResolveOutbox records a person's decision on an unknown row; the
	// handler reads the row as the viewer first (OutboxByID).
	ResolveOutbox(ctx context.Context, id, resolution string, a store.Action, p store.RetryPolicy) (string, error)
}

// RawReader loads the raw message of a delivery (ingest.Ingester).
type RawReader interface {
	ReadRaw(ctx context.Context, ref store.RawRef) ([]byte, error)
}

// Guard authenticates a request and checks its scopes before h runs with
// the viewer in the context (authn.Service.Require).
type Guard interface {
	Require(h http.Handler, scopes ...auth.Scope) http.Handler
}

// API is the REST handler.
type API struct {
	db     Store
	raw    RawReader
	submit Submitter
	log    *slog.Logger
}

// New returns the API over db, reading raw messages for the structured
// view through raw.
func New(db Store, raw RawReader, log *slog.Logger) *API {
	if log == nil {
		log = slog.Default()
	}
	return &API{db: db, raw: raw, log: log}
}

// WithSubmitter makes a submit drafts through s. Without one the submit
// route is not served at all: there is no other way into the outbox.
func (a *API) WithSubmitter(s Submitter) *API {
	a.submit = s
	return a
}

var (
	read   = []auth.Scope{auth.MessageRead}
	prefix = []auth.Scope{auth.PrefixRead}
	// draft:create writes drafts and submits them. Submitting needs no
	// more: the core decides, and a credential without submission:send
	// gets a draft held for policy rather than an error (§8.9).
	draftWrite = []auth.Scope{auth.DraftCreate}
	// The outbox holds envelope recipients, Bcc included: delivery data.
	outboxRead = []auth.Scope{auth.MessageRead, auth.DeliveryRead}
	// Resolving resends: a sending decision, and a person's (§4.5).
	outboxResolve = []auth.Scope{auth.SubmissionSend}
)

// routes is every operation under /api/. A new one is one entry here and
// a handler; the ACL test and openapi.json pick it up from this table.
var routes = []route{
	jsonRoute(http.MethodGet, "/api/v1/messages", "List messages, newest first", read, (*API).listMessages),
	jsonRoute(http.MethodGet, "/api/v1/messages/{id}", "Get a message's parsed fields and plain text body", read, (*API).message),
	jsonRoute(http.MethodGet, "/api/v1/messages/{id}/view", "Get the structured view of a message (§8.1)", read, (*API).messageView),
	rawRoute(http.MethodGet, "/api/v1/messages/{id}/attachments/{part}", "Download an attachment",
		"application/octet-stream", read, (*API).attachment),
	jsonRoute(http.MethodGet, "/api/v1/threads/{id}", "Get a thread", read, (*API).thread),
	jsonRoute(http.MethodGet, "/api/v1/prefixes", "List prefixes with message counts", prefix, (*API).prefixes),
	// The offending mail is listed as messages, so this needs both.
	jsonRoute(http.MethodGet, "/api/v1/prefixes/{address}", "Get a prefix with its leak check and the latest mail that leaked or spoofed",
		[]auth.Scope{auth.PrefixRead, auth.MessageRead}, (*API).prefix),
	jsonRoute(http.MethodGet, "/api/v1/search", "Search messages, newest first", read, (*API).search),
	jsonRoute(http.MethodGet, "/api/v1/messages/{id}/reply-identities", "List the identities a reply defaults to, best first",
		[]auth.Scope{auth.MessageRead, auth.DraftCreate}, (*API).replyIdentities),
	jsonRoute(http.MethodGet, "/api/v1/identities", "List the identities the viewer may send as, and how it would go",
		draftWrite, (*API).identities),
	jsonRoute(http.MethodGet, "/api/v1/drafts", "List drafts, most recently changed first", read, (*API).drafts),
	jsonRoute(http.MethodPost, "/api/v1/drafts", "Create a draft", draftWrite, (*API).createDraft).withStatus(http.StatusCreated),
	jsonRoute(http.MethodGet, "/api/v1/drafts/{id}", "Get a draft", read, (*API).draft),
	jsonRoute(http.MethodPut, "/api/v1/drafts/{id}", "Replace a draft's content", draftWrite, (*API).updateDraft),
	jsonRoute(http.MethodDelete, "/api/v1/drafts/{id}", "Delete a draft", draftWrite, (*API).deleteDraft),
	jsonRoute(http.MethodPost, "/api/v1/drafts/{id}/submit", "Submit a draft: queued, or held as a draft with the reason (§8.9)",
		draftWrite, (*API).submitDraft).submitting(),
	jsonRoute(http.MethodGet, "/api/v1/outbox", "List outgoing mail, newest first", outboxRead, (*API).outboxList),
	jsonRoute(http.MethodGet, "/api/v1/outbox/{id}", "Get one outgoing message's state", outboxRead, (*API).outboxEntry),
	jsonRoute(http.MethodPost, "/api/v1/outbox/{id}/resolve", "Decide on mail whose delivery is unknown (§4.5); people only",
		outboxResolve, (*API).resolveOutbox),
	{Op: openapi.Op{
		Method: http.MethodGet, Path: "/api/v1/openapi.json", Summary: "This document",
		Access: openapi.Public, Media: "application/json",
	}},
}

// OpenAPI is the document of routes and of the /auth/ endpoints.
var OpenAPI = sync.OnceValues(func() ([]byte, error) {
	ops := authn.Ops()
	for _, rt := range routes {
		ops = append(ops, rt.Op)
	}
	return openapi.Document("yuuka", "v1", ops)
})

// Handler serves /api/. Unknown paths under it are problem+json 404s,
// never the WebUI shell.
func (a *API) Handler(g Guard) http.Handler {
	doc, err := OpenAPI()
	if err != nil {
		panic(err) // the types cannot describe themselves: TestOpenAPI fails first
	}
	mux := http.NewServeMux()
	for _, rt := range routes {
		pattern := rt.Method + " " + rt.Path
		if rt.needsSubmitter && a.submit == nil {
			continue // answered by the 404 below
		}
		if rt.serve == nil {
			// The contract is public: the repository is, and an agent
			// with a token can read what it may call.
			mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(doc)
			})
			continue
		}
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			v, ok := auth.ViewerFrom(r.Context())
			if !ok {
				// Guard let a request through without a viewer: a wiring
				// bug, never "anonymous".
				a.fail(w, r, errors.New("api: no viewer after guard"))
				return
			}
			res, err := rt.serve(a, w, r, v)
			switch {
			case err != nil:
				a.fail(w, r, err)
			case rt.Response != nil:
				writeJSON(w, openapi.StatusOf(rt.Op), res)
			case rt.Media == "":
				w.WriteHeader(openapi.StatusOf(rt.Op)) // 204
			}
		})
		mux.Handle(pattern, g.Require(h, rt.Scopes...))
	}
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		openapi.Write(w, openapi.Errorf(http.StatusNotFound, openapi.CodeNotFound, "no endpoint %s %s", r.Method, r.URL.Path))
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Mail is private; no shared or browser cache keeps a copy.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// fail maps an error to a problem: one a handler or bind returned as is,
// the store's and the submission core's sentinels to their codes,
// anything else to internal.
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	if p, ok := errors.AsType[*openapi.Problem](err); ok {
		openapi.Write(w, p)
		return
	}
	for _, m := range []struct {
		err    error
		status int
		code   openapi.Code
		detail string // "" for the error's own text, which names no mail content
	}{
		{store.ErrNotFound, http.StatusNotFound, openapi.CodeNotFound, "not found"},
		{store.ErrInvalidDraft, http.StatusBadRequest, openapi.CodeInvalidBody, ""},
		{store.ErrInFlight, http.StatusConflict, openapi.CodeConflict, "the draft has gone to the outbox"},
		{store.ErrRawDraft, http.StatusConflict, openapi.CodeConflict, ""},
		{store.ErrOutboxState, http.StatusConflict, openapi.CodeConflict, ""},
		{store.ErrNoIdentity, http.StatusUnprocessableEntity, openapi.CodeNoIdentity, ""},
		{submit.ErrNoIdentity, http.StatusUnprocessableEntity, openapi.CodeNoIdentity, ""},
		{submit.ErrRejected, http.StatusForbidden, openapi.CodeSubmissionRejected, ""},
	} {
		if errors.Is(err, m.err) {
			detail := m.detail
			if detail == "" {
				detail = err.Error()
			}
			openapi.Write(w, openapi.Errorf(m.status, m.code, "%s", detail))
			return
		}
	}
	if r.Context().Err() != nil {
		return // the client went away
	}
	a.log.Error("api", "path", r.URL.Path, "err", err)
	openapi.Write(w, openapi.Errorf(http.StatusInternalServerError, openapi.CodeInternal, "internal error"))
}

var downgrades = map[Downgraded]store.Downgrade{
	"hide": store.HideDowngraded, "include": store.IncludeDowngraded, "only": store.OnlyDowngraded,
}

var classes = map[agentview.Class]classify.Class{
	agentview.ClassUnknown: classify.Unknown, agentview.ClassMachine: classify.Machine, agentview.ClassHuman: classify.Human,
}

func (a *API) listMessages(ctx context.Context, v *auth.Viewer, p MessagesParams) (MessagePage, error) {
	q := store.MessageQuery{Limit: p.Limit, Before: (*store.MessageCursor)(p.Cursor)}
	q.Downgraded = downgrades[p.Downgraded]
	if p.Prefix != nil {
		q.Prefix, q.Zone = p.Prefix.Prefix, p.Prefix.Zone
	}
	for _, c := range p.Class {
		q.Classes = append(q.Classes, classes[c])
	}
	ms, err := a.db.ListMessages(ctx, v, q)
	if err != nil {
		return MessagePage{}, err
	}
	page := MessagePage{Messages: make([]MessageSummary, len(ms))}
	for i, m := range ms {
		page.Messages[i] = summary(v, m)
	}
	if len(ms) == p.Limit {
		last := ms[len(ms)-1]
		page.NextCursor = Cursor{ReceivedAt: last.ReceivedAt, ID: last.ID}.String()
	}
	return page, nil
}

func (a *API) message(ctx context.Context, v *auth.Viewer, p IDParams) (Message, error) {
	d, err := a.db.MessageByID(ctx, v, p.ID)
	if err != nil {
		return Message{}, err
	}
	// The recipients with their names come from the raw, like the
	// structured view: the messages columns keep bare addresses only
	// (§4). d.Raw is a raw this viewer may read (§13.8), so a viewer who
	// cannot read the message never gets here, and one who reads it only
	// through its sent copy gets the sent copy's header.
	raw, err := a.raw.ReadRaw(ctx, d.Raw)
	if err != nil {
		return Message{}, err
	}
	rc := mimeparse.ParseRecipients(raw)
	out := Message{
		MessageSummary: summary(v, d.MessageSummary),
		Extractions:    make([]agentview.Extraction, len(d.Extractions)),
		MessageID:      d.MessageID, InReplyTo: d.InReplyTo, References: nonNil(d.References),
		Date: d.Date, BodyText: d.BodyText, ParseError: d.ParseError,
		To: addressesOf(rc.To), Cc: addressesOf(rc.Cc), ReplyTo: addressesOf(rc.ReplyTo),
	}
	for i, x := range d.Extractions {
		out.Extractions[i] = agentview.ExtractionOf(x)
	}
	// Envelope and authentication facts are delivery data (§13.4).
	if v.Has(auth.DeliveryRead) {
		out.Deliveries = make([]Delivery, len(d.Deliveries))
		for i, f := range d.Deliveries {
			out.Deliveries[i] = Delivery{
				ID: f.ID, Zone: f.Zone, Prefix: f.Prefix, PrefixClaimed: f.PrefixState == store.PrefixClaimed,
				EnvelopeFrom: f.EnvelopeFrom, EnvelopeTo: f.EnvelopeTo, ReceivedAt: f.ReceivedAt,
				Auth: authOf(f.Auth),
			}
			if f.Leak.Verdict != leak.None {
				out.Deliveries[i].Leak = &agentview.LeakCheck{
					To: f.Prefix + "@" + f.Zone, Verdict: agentview.LeakVerdict(f.Leak.Verdict), Domain: f.Leak.Domain,
				}
			}
		}
	}
	return out, nil
}

// messageView is the structured view of §8.1 that the reading pane
// renders (§12.1), the same tree get_message gives agents. Delivery facts
// in it need delivery:read, as above.
func (a *API) messageView(ctx context.Context, v *auth.Viewer, p ViewParams) (agentview.View, error) {
	d, err := a.db.MessageByID(ctx, v, p.ID)
	if err != nil {
		return agentview.View{}, err
	}
	raw, err := a.raw.ReadRaw(ctx, d.Raw)
	if err != nil {
		return agentview.View{}, err
	}
	view, err := agentview.Select(agentview.Build(agentview.InputOf(v, d, raw)), p.Level, p.Anchor)
	if errors.Is(err, agentview.ErrNoAnchor) {
		return view, openapi.Errorf(http.StatusNotFound, openapi.CodeNotFound, "anchor %s is not in this message", p.Anchor)
	}
	return view, err
}

func addressesOf(ms []mimeparse.Mailbox) []agentview.Address {
	out := make([]agentview.Address, len(ms))
	for i, m := range ms {
		out[i] = agentview.Address{Name: m.Name, Addr: m.Addr}
	}
	return out
}

func authOf(a *authres.Auth) *DeliveryAuth {
	if a == nil {
		return nil
	}
	out := &DeliveryAuth{Trusted: a.Trusted, Reason: a.Reason, AuthservID: a.AuthservID, DMARC: a.DMARC, RemoteIP: a.RemoteIP}
	for _, res := range a.Results {
		out.Results = append(out.Results, DeliveryAuthResult{Method: res.Method, Result: res.Result, Props: res.Props})
	}
	return out
}

func (a *API) thread(ctx context.Context, v *auth.Viewer, p IDParams) (Thread, error) {
	t, err := a.db.ThreadByID(ctx, v, p.ID)
	if err != nil {
		return Thread{}, err
	}
	out := Thread{
		ID: t.ID, Subject: t.Subject, Participants: nonNil(t.Participants),
		FirstAt: t.FirstAt, LastAt: t.LastAt, Messages: make([]MessageSummary, len(t.Messages)),
	}
	for i, m := range t.Messages {
		out.Messages[i] = summary(v, m)
	}
	return out, nil
}

func (a *API) prefixes(ctx context.Context, v *auth.Viewer, p PrefixesParams) (PrefixList, error) {
	q := store.PrefixQuery{Limit: p.Limit, Offset: p.Offset}
	q.State = map[PrefixState]int{"claimed": store.PrefixClaimed, "discovered": store.PrefixDiscovered}[p.State]
	sum, err := a.db.PrefixStats(ctx, v, q)
	if err != nil {
		return PrefixList{}, err
	}
	out := PrefixList{Messages: sum.Messages, Downgraded: sum.Downgraded, Total: sum.Total, Prefixes: make([]Prefix, len(sum.Prefixes))}
	for i, p := range sum.Prefixes {
		out.Prefixes[i] = prefixOut(p)
	}
	return out, nil
}

// offenseLimit is how many offending messages the prefix detail lists:
// enough to see a pattern, the rest is the prefix's message list.
const offenseLimit = 20

func (a *API) prefix(ctx context.Context, v *auth.Viewer, p PrefixParams) (PrefixDetail, error) {
	d, err := a.db.PrefixByAddress(ctx, v, p.Address.Zone, p.Address.Prefix, offenseLimit)
	if err != nil {
		return PrefixDetail{}, err
	}
	out := PrefixDetail{Prefix: prefixOut(d.PrefixStat), Offenses: make([]Offense, len(d.Offenses))}
	for i, o := range d.Offenses {
		out.Offenses[i] = Offense{Message: summary(v, o.Message), Verdict: agentview.LeakVerdict(o.Verdict), Domain: o.Domain, DeliveredAt: o.DeliveredAt}
	}
	return out, nil
}

func prefixOut(p store.PrefixStat) Prefix {
	out := Prefix{
		Zone: p.Zone, Prefix: p.Prefix, Address: p.Prefix + "@" + p.Zone,
		Claimed: p.State == store.PrefixClaimed, Label: p.Label,
		FirstSeen: p.FirstSeen, LastSeen: p.LastSeen, Messages: p.Messages, Downgraded: p.Downgraded,
		ExpectedDomains: nonNil(p.ExpectedDomains), Observed: make([]ObservedDomain, len(p.Observed)),
		Leaks: p.Leaks, Unverified: p.Unverified, Spoofs: p.Spoofs, LastOffense: p.LastOffense,
	}
	for i, o := range p.Observed {
		out.Observed[i] = ObservedDomain{Domain: o.Domain, Messages: o.Messages, LastSeen: o.LastSeen, Expected: o.Expected}
	}
	return out
}

func (a *API) search(ctx context.Context, v *auth.Viewer, p SearchParams) (SearchResult, error) {
	res, err := a.db.Search(ctx, v, store.SearchQuery{
		Text: p.Q, Limit: p.Limit, Before: (*store.MessageCursor)(p.Cursor), IncludeDowngraded: p.Downgraded == "include",
	})
	if err != nil {
		return SearchResult{}, err
	}
	out := SearchResult{Total: res.Total, Hits: make([]MessageSummary, len(res.Hits))}
	for i, h := range res.Hits {
		out.Hits[i] = MessageSummary{
			ID: h.ID, ThreadID: h.ThreadID, ReceivedAt: h.ReceivedAt, Subject: h.Subject,
			FromAddr: h.FromAddr, FromName: h.FromName, Snippet: h.Snippet, Class: classOf(h.Class),
			Downgraded: h.Downgraded, Quarantined: h.Quarantined, Recipients: []string{},
			Outgoing: outgoingOut(h.Outgoing),
		}
		// A hit from the notification zone shows its code there too
		// (§12.9); other mail is found by its words, not its numbers.
		if h.Class == classify.Machine {
			out.Hits[i].Extracted = extracted(h.Extracted, time.Now())
			if v.Has(auth.DeliveryRead) {
				out.Hits[i].SenderVerified = h.SenderVerified
			}
		}
	}
	if len(res.Hits) == p.Limit {
		last := res.Hits[len(res.Hits)-1]
		out.NextCursor = Cursor{ReceivedAt: last.ReceivedAt, ID: last.ID}.String()
	}
	return out, nil
}

func summary(v *auth.Viewer, m store.MessageSummary) MessageSummary {
	out := MessageSummary{
		ID: m.ID, ThreadID: m.ThreadID, ReceivedAt: m.ReceivedAt, Subject: m.Subject,
		FromAddr: m.FromAddr, FromName: m.FromName, Snippet: m.Snippet, Class: classOf(m.Class),
		Downgraded: m.Downgraded, Quarantined: m.Quarantined, HasAttach: m.HasAttach,
		Recipients: nonNil(m.Recipients), Outgoing: outgoingOut(m.Outgoing),
		Extracted: extracted(m.Extracted, time.Now()),
	}
	if v.Has(auth.DeliveryRead) {
		out.SenderVerified = m.SenderVerified
	}
	return out
}

// extracted is the summary's Extracted from the best extraction of each
// kind, as the store lists them; nil when there is no code or link.
func extracted(xs []store.Extraction, now time.Time) *Extracted {
	var out Extracted
	for _, x := range xs {
		e := agentview.ExtractionOf(x)
		switch e.Kind {
		case "otp":
			out.Code = &e
		case "link":
			out.Link = &e
		case "expiry":
			out.ExpiresAt = e.ExpiresAt
		}
	}
	if out.Code == nil && out.Link == nil {
		return nil
	}
	out.Expired = out.ExpiresAt != nil && !now.Before(*out.ExpiresAt)
	return &out
}

func classOf(c classify.Class) agentview.Class { return agentview.Class(c.String()) }

// nonNil keeps JSON arrays arrays: the schema promises no null.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
