// Package api serves the REST API under /api/v1/ for the WebUI (session
// cookie) and for agents and services (bearer token). Every route states
// the scopes it needs, and every store read takes the request's viewer, so
// visibility (§13.8) is decided by the store and nowhere here.
//
// The JSON shapes are in types.go; the WebUI's TypeScript types are
// generated from them.
package api

//go:generate go tool tygo generate --config tygo.yaml

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/classify"
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

// route is one endpoint and the scopes it requires. The ACL test walks
// this table, so a new read path cannot skip it.
type route struct {
	pattern string
	scopes  []auth.Scope
	handle  func(*API, http.ResponseWriter, *http.Request, *auth.Viewer)
}

var routes = []route{
	{"GET /api/v1/messages", []auth.Scope{auth.MessageRead}, (*API).listMessages},
	{"GET /api/v1/messages/{id}", []auth.Scope{auth.MessageRead}, (*API).message},
	{"GET /api/v1/messages/{id}/view", []auth.Scope{auth.MessageRead}, (*API).messageView},
	{"GET /api/v1/messages/{id}/attachments/{part}", []auth.Scope{auth.MessageRead}, (*API).attachment},
	{"GET /api/v1/threads/{id}", []auth.Scope{auth.MessageRead}, (*API).thread},
	{"GET /api/v1/prefixes", []auth.Scope{auth.PrefixRead}, (*API).prefixes},
	{"GET /api/v1/search", []auth.Scope{auth.MessageRead}, (*API).search},
}

// Handler serves /api/. Unknown paths under it are JSON 404s, never the
// WebUI shell.
func (a *API) Handler(g Guard) http.Handler {
	mux := http.NewServeMux()
	for _, rt := range routes {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			v, ok := auth.ViewerFrom(r.Context())
			if !ok {
				// Guard let a request through without a viewer: a wiring
				// bug, never "anonymous".
				a.log.Error("api: no viewer after guard", "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			rt.handle(a, w, r, v)
		})
		mux.Handle(rt.pattern, g.Require(h, rt.scopes...))
	}
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint")
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

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, ErrorBody{Error: msg})
}

// fail maps a store error to a response.
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Context().Err() != nil {
		return // the client went away
	}
	a.log.Error("api", "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// badRequest is a query parameter error.
type badRequest string

func (b badRequest) Error() string { return string(b) }

func parseLimit(s string, upper int) (int, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > upper {
		return 0, badRequest("limit: want 1.." + strconv.Itoa(upper))
	}
	return n, nil
}

func parseDowngrade(s string) (store.Downgrade, error) {
	switch s {
	case "", "hide":
		return store.HideDowngraded, nil
	case "include":
		return store.IncludeDowngraded, nil
	case "only":
		return store.OnlyDowngraded, nil
	}
	return 0, badRequest("downgraded: want hide, include or only")
}

// parseClasses reads class=human,unknown: one class or several, comma
// separated.
func parseClasses(s string) ([]classify.Class, error) {
	if s == "" {
		return nil, nil
	}
	var out []classify.Class
	for _, name := range strings.Split(s, ",") {
		switch Class(name) {
		case ClassUnknown:
			out = append(out, classify.Unknown)
		case ClassMachine:
			out = append(out, classify.Machine)
		case ClassHuman:
			out = append(out, classify.Human)
		default:
			return nil, badRequest("class: want unknown, machine or human, comma separated")
		}
	}
	return out, nil
}

// Cursors are opaque to clients: base64url of "<RFC 3339 nano> <id>".
func encodeCursor(c store.MessageCursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(c.ReceivedAt.UTC().Format(time.RFC3339Nano) + " " + c.ID))
}

func decodeCursor(s string) (*store.MessageCursor, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		at, id, ok := strings.Cut(string(raw), " ")
		if t, terr := time.Parse(time.RFC3339Nano, at); ok && terr == nil && id != "" {
			return &store.MessageCursor{ReceivedAt: t, ID: id}, nil
		}
	}
	return nil, badRequest("cursor: malformed")
}

func (a *API) listMessages(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	q, err := messageQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ms, err := a.db.ListMessages(r.Context(), v, q)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	page := MessagePage{Messages: make([]MessageSummary, len(ms))}
	for i, m := range ms {
		page.Messages[i] = summary(m)
	}
	limit := q.Limit
	if limit == 0 {
		limit = defaultPage
	}
	if len(ms) == limit {
		last := ms[len(ms)-1]
		page.NextCursor = encodeCursor(store.MessageCursor{ReceivedAt: last.ReceivedAt, ID: last.ID})
	}
	writeJSON(w, http.StatusOK, page)
}

const defaultPage = 100

func messageQuery(r *http.Request) (store.MessageQuery, error) {
	p := r.URL.Query()
	q := store.MessageQuery{Limit: defaultPage}
	var err error
	if addr := p.Get("prefix"); addr != "" {
		i := strings.LastIndexByte(addr, '@')
		if i <= 0 || i == len(addr)-1 {
			return q, badRequest("prefix: want prefix@zone")
		}
		q.Prefix, q.Zone = strings.ToLower(addr[:i]), strings.ToLower(addr[i+1:])
	}
	if q.Classes, err = parseClasses(p.Get("class")); err != nil {
		return q, err
	}
	if q.Downgraded, err = parseDowngrade(p.Get("downgraded")); err != nil {
		return q, err
	}
	if q.Before, err = decodeCursor(p.Get("cursor")); err != nil {
		return q, err
	}
	if n, err := parseLimit(p.Get("limit"), 500); err != nil {
		return q, err
	} else if n > 0 {
		q.Limit = n
	}
	return q, nil
}

func (a *API) message(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	d, err := a.db.MessageByID(r.Context(), v, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
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
	writeJSON(w, http.StatusOK, out)
}

// messageView is the structured view of §8.1 that the reading pane
// renders (§12.1), the same tree get_message gives agents. level is
// snippet, sections or part (the default); anchor is a fragment such as
// part/1/para/3. Delivery facts in it need delivery:read, as above.
func (a *API) messageView(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	p := r.URL.Query()
	level := agentview.LevelPart
	if s := p.Get("level"); s != "" {
		var err error
		if level, err = agentview.ParseLevel(s); err != nil {
			writeError(w, http.StatusBadRequest, "level: want snippet, sections or part")
			return
		}
	}
	var anchor agentview.Anchor
	if s := p.Get("anchor"); s != "" {
		var err error
		if anchor, err = agentview.ParseAnchor(s); err != nil {
			writeError(w, http.StatusBadRequest, "anchor: want part/<section>[/para/<n>]")
			return
		}
	}
	d, err := a.db.MessageByID(r.Context(), v, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	raw, err := a.raw.ReadRaw(r.Context(), d.Raw)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	view, err := agentview.Select(agentview.Build(agentview.InputOf(v, d, raw)), level, anchor)
	if errors.Is(err, agentview.ErrNoAnchor) {
		writeError(w, http.StatusNotFound, "anchor: not in this message")
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func authOf(a *authres.Auth) *Auth {
	if a == nil {
		return nil
	}
	out := &Auth{Trusted: a.Trusted, Reason: a.Reason, AuthservID: a.AuthservID, DMARC: a.DMARC, RemoteIP: a.RemoteIP}
	for _, res := range a.Results {
		out.Results = append(out.Results, AuthResult{Method: res.Method, Result: res.Result, Props: res.Props})
	}
	return out
}

func (a *API) thread(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	t, err := a.db.ThreadByID(r.Context(), v, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := Thread{
		ID: t.ID, Subject: t.Subject, Participants: nonNil(t.Participants),
		FirstAt: t.FirstAt, LastAt: t.LastAt, Messages: make([]MessageSummary, len(t.Messages)),
	}
	for i, m := range t.Messages {
		out.Messages[i] = summary(m)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) prefixes(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	q, err := prefixQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sum, err := a.db.PrefixStats(r.Context(), v, q)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := PrefixList{Messages: sum.Messages, Downgraded: sum.Downgraded, Total: sum.Total, Prefixes: make([]Prefix, len(sum.Prefixes))}
	for i, p := range sum.Prefixes {
		out.Prefixes[i] = Prefix{
			Zone: p.Zone, Prefix: p.Prefix, Address: p.Prefix + "@" + p.Zone,
			Claimed: p.State == store.PrefixClaimed, Label: p.Label,
			FirstSeen: p.FirstSeen, LastSeen: p.LastSeen, Messages: p.Messages, Downgraded: p.Downgraded,
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// prefixQuery reads state=claimed|discovered and limit/offset paging.
// Without a limit every matching prefix comes back.
func prefixQuery(r *http.Request) (store.PrefixQuery, error) {
	p := r.URL.Query()
	var q store.PrefixQuery
	switch p.Get("state") {
	case "":
	case "claimed":
		q.State = store.PrefixClaimed
	case "discovered":
		q.State = store.PrefixDiscovered
	default:
		return q, badRequest("state: want claimed or discovered")
	}
	var err error
	if q.Limit, err = parseLimit(p.Get("limit"), 500); err != nil {
		return q, err
	}
	if q.Offset, err = parseOffset(p.Get("offset")); err != nil {
		return q, err
	}
	return q, nil
}

func parseOffset(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, badRequest("offset: want a non-negative integer")
	}
	return n, nil
}

func (a *API) search(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	p := r.URL.Query()
	limit, err := parseLimit(p.Get("limit"), 500)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	offset, err := parseOffset(p.Get("offset"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	dg, err := parseDowngrade(p.Get("downgraded"))
	if err != nil || dg == store.OnlyDowngraded {
		writeError(w, http.StatusBadRequest, "downgraded: want hide or include")
		return
	}
	res, err := a.db.Search(r.Context(), v, store.SearchQuery{
		Text: p.Get("q"), Limit: limit, Offset: offset, IncludeDowngraded: dg == store.IncludeDowngraded,
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := SearchResult{Total: res.Total, Hits: make([]MessageSummary, len(res.Hits))}
	for i, h := range res.Hits {
		out.Hits[i] = MessageSummary{
			ID: h.ID, ThreadID: h.ThreadID, ReceivedAt: h.ReceivedAt, Subject: h.Subject,
			FromAddr: h.FromAddr, FromName: h.FromName, Snippet: h.Snippet, Class: classOf(h.Class),
			Downgraded: h.Downgraded, Quarantined: h.Quarantined, Recipients: []string{},
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func summary(m store.MessageSummary) MessageSummary {
	return MessageSummary{
		ID: m.ID, ThreadID: m.ThreadID, ReceivedAt: m.ReceivedAt, Subject: m.Subject,
		FromAddr: m.FromAddr, FromName: m.FromName, Snippet: m.Snippet, Class: classOf(m.Class),
		Downgraded: m.Downgraded, Quarantined: m.Quarantined, HasAttach: m.HasAttach,
		Recipients: nonNil(m.Recipients),
	}
}

func classOf(c classify.Class) Class { return Class(c.String()) }

// nonNil keeps JSON arrays arrays: the TypeScript types say string[],
// not string[] | null.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
