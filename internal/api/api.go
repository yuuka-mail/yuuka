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

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authn"
	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/classify"
	"github.com/yuuka-mail/yuuka/internal/openapi"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// Store is the slice of the store the API reads. Every method takes the
// viewer; the ACL test wraps the real store to check that it arrives.
type Store interface {
	ListMessages(ctx context.Context, v *auth.Viewer, q store.MessageQuery) ([]store.MessageSummary, error)
	MessageByID(ctx context.Context, v *auth.Viewer, id string) (store.MessageDetail, error)
	ThreadByID(ctx context.Context, v *auth.Viewer, id string) (store.ThreadView, error)
	PrefixStats(ctx context.Context, v *auth.Viewer, q store.PrefixQuery) (store.PrefixSummary, error)
	Search(ctx context.Context, v *auth.Viewer, q store.SearchQuery) (store.SearchResult, error)
	// RecordAction appends to the audit log (attachment downloads).
	RecordAction(ctx context.Context, a store.Action) error
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
	db  Store
	raw RawReader
	log *slog.Logger
}

// New returns the API over db, reading raw messages for the structured
// view through raw.
func New(db Store, raw RawReader, log *slog.Logger) *API {
	if log == nil {
		log = slog.Default()
	}
	return &API{db: db, raw: raw, log: log}
}

var (
	read   = []auth.Scope{auth.MessageRead}
	prefix = []auth.Scope{auth.PrefixRead}
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
	jsonRoute(http.MethodGet, "/api/v1/search", "Search messages, newest first", read, (*API).search),
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
// store.ErrNotFound to not_found, anything else to internal.
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	if p, ok := errors.AsType[*openapi.Problem](err); ok {
		openapi.Write(w, p)
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		openapi.Write(w, openapi.Errorf(http.StatusNotFound, openapi.CodeNotFound, "not found"))
		return
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
		page.Messages[i] = summary(m)
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
	out := Message{
		MessageSummary: summary(d.MessageSummary),
		MessageID:      d.MessageID, InReplyTo: d.InReplyTo, References: nonNil(d.References),
		Date: d.Date, BodyText: d.BodyText, ParseError: d.ParseError,
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
		out.Messages[i] = summary(m)
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
		out.Prefixes[i] = Prefix{
			Zone: p.Zone, Prefix: p.Prefix, Address: p.Prefix + "@" + p.Zone,
			Claimed: p.State == store.PrefixClaimed, Label: p.Label,
			FirstSeen: p.FirstSeen, LastSeen: p.LastSeen, Messages: p.Messages, Downgraded: p.Downgraded,
		}
	}
	return out, nil
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
		}
	}
	if len(res.Hits) == p.Limit {
		last := res.Hits[len(res.Hits)-1]
		out.NextCursor = Cursor{ReceivedAt: last.ReceivedAt, ID: last.ID}.String()
	}
	return out, nil
}

func summary(m store.MessageSummary) MessageSummary {
	return MessageSummary{
		ID: m.ID, ThreadID: m.ThreadID, ReceivedAt: m.ReceivedAt, Subject: m.Subject,
		FromAddr: m.FromAddr, FromName: m.FromName, Snippet: m.Snippet, Class: classOf(m.Class),
		Downgraded: m.Downgraded, Quarantined: m.Quarantined, HasAttach: m.HasAttach,
		Recipients: nonNil(m.Recipients),
	}
}

func classOf(c classify.Class) agentview.Class { return agentview.Class(c.String()) }

// nonNil keeps JSON arrays arrays: the schema promises no null.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
