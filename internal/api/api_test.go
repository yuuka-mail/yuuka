package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authn"
	"github.com/yuuka-mail/yuuka/internal/devseed"
	"github.com/yuuka-mail/yuuka/internal/ingest"
	"github.com/yuuka-mail/yuuka/internal/localraw"
	"github.com/yuuka-mail/yuuka/internal/openapi"
	"github.com/yuuka-mail/yuuka/internal/outbox"
	"github.com/yuuka-mail/yuuka/internal/r2/r2fake"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/submit"
	"github.com/yuuka-mail/yuuka/internal/transport"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// spy is the real store, recording which viewer each read ran for.
type spy struct {
	*store.DB
	mu    sync.Mutex
	calls map[string][]*auth.Viewer
}

func (s *spy) saw(op string, v *auth.Viewer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[op] = append(s.calls[op], v)
}

func (s *spy) take() map[string][]*auth.Viewer {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.calls
	s.calls = map[string][]*auth.Viewer{}
	return c
}

func (s *spy) ListMessages(ctx context.Context, v *auth.Viewer, q store.MessageQuery) ([]store.MessageSummary, error) {
	s.saw("ListMessages", v)
	return s.DB.ListMessages(ctx, v, q)
}

func (s *spy) MessageByID(ctx context.Context, v *auth.Viewer, id string) (store.MessageDetail, error) {
	s.saw("MessageByID", v)
	return s.DB.MessageByID(ctx, v, id)
}

func (s *spy) ThreadByID(ctx context.Context, v *auth.Viewer, id string) (store.ThreadView, error) {
	s.saw("ThreadByID", v)
	return s.DB.ThreadByID(ctx, v, id)
}

func (s *spy) PrefixStats(ctx context.Context, v *auth.Viewer, q store.PrefixQuery) (store.PrefixSummary, error) {
	s.saw("PrefixStats", v)
	return s.DB.PrefixStats(ctx, v, q)
}

func (s *spy) Search(ctx context.Context, v *auth.Viewer, q store.SearchQuery) (store.SearchResult, error) {
	s.saw("Search", v)
	return s.DB.Search(ctx, v, q)
}

func (s *spy) DraftByID(ctx context.Context, v *auth.Viewer, id string) (store.Draft, error) {
	s.saw("DraftByID", v)
	return s.DB.DraftByID(ctx, v, id)
}

func (s *spy) Drafts(ctx context.Context, v *auth.Viewer, q store.DraftQuery) ([]store.Draft, error) {
	s.saw("Drafts", v)
	return s.DB.Drafts(ctx, v, q)
}

func (s *spy) CreateDraft(ctx context.Context, v *auth.Viewer, d store.Draft, at time.Time) (store.Draft, error) {
	s.saw("CreateDraft", v)
	return s.DB.CreateDraft(ctx, v, d, at)
}

func (s *spy) UpdateDraft(ctx context.Context, v *auth.Viewer, d store.Draft, at time.Time) (store.Draft, error) {
	s.saw("UpdateDraft", v)
	return s.DB.UpdateDraft(ctx, v, d, at)
}

func (s *spy) DeleteDraft(ctx context.Context, v *auth.Viewer, id string, at time.Time) error {
	s.saw("DeleteDraft", v)
	return s.DB.DeleteDraft(ctx, v, id, at)
}

func (s *spy) ReplyIdentities(ctx context.Context, v *auth.Viewer, id string) ([]store.ReplyIdentity, error) {
	s.saw("ReplyIdentities", v)
	return s.DB.ReplyIdentities(ctx, v, id)
}

func (s *spy) Standings(ctx context.Context, v *auth.Viewer) ([]store.Standing, error) {
	s.saw("Standings", v)
	return s.DB.Standings(ctx, v)
}

func (s *spy) OutboxList(ctx context.Context, v *auth.Viewer, q store.OutboxQuery) ([]store.OutboxEntry, error) {
	s.saw("OutboxList", v)
	return s.DB.OutboxList(ctx, v, q)
}

func (s *spy) OutboxByID(ctx context.Context, v *auth.Viewer, id string) (store.OutboxEntry, error) {
	s.saw("OutboxByID", v)
	return s.DB.OutboxByID(ctx, v, id)
}

// localRaw reads raw messages the way ingest does without R2.
type localRaw struct{ localraw.Dir }

func (l localRaw) ReadRaw(_ context.Context, ref store.RawRef) ([]byte, error) {
	return l.Get(ref.LocalPath)
}

type harness struct {
	t    *testing.T
	db   *store.DB
	pool *pgxpool.Pool
	spy  *spy
	svc  *authn.Service
	// agent is agent:arisu, owner of arisu@example.com; human is
	// human:operator, owner of *@example.com, where devseed's mail
	// arrives.
	agent, human store.Principal
	sub          *submit.Service
	h            http.Handler
}

// fakeTransport only answers Caps: nothing here is ever sent.
type fakeTransport struct{}

func (fakeTransport) Caps() transport.Caps { return transport.CloudflareCaps() }

func (fakeTransport) Send(context.Context, transport.Envelope, []byte, func(context.Context) error) (*transport.SendResult, error) {
	return nil, errors.New("not in these tests")
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db, pool := storetest.New(t)
	raws := t.TempDir()
	if _, err := devseed.Load(t.Context(), db, pool, devseed.Options{
		Dir: "../../testdata/eml", Variants: 20, Now: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC), RawDir: raws,
	}); err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := authn.New(authn.Config{DB: db, Origin: "https://mail.example.com", Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	principal := func(kind, name string) store.Principal {
		p, err := db.EnsurePrincipal(ctx, store.Principal{ID: ulid.New(time.Now()), Kind: kind, Name: name, DisplayName: name, CreatedAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	agent, human := principal("agent", "arisu"), principal("human", "operator")
	account, err := db.EnsureCloudflareAccount(ctx, "devseed")
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []struct{ addr, owner string }{{"*@example.com", human.ID}, {"arisu@example.com", agent.ID}} {
		zone, prefix, _ := store.SplitIdentityAddress(i.addr)
		at := time.Now()
		if err := db.AddIdentity(ctx, store.Identity{
			ID: ulid.New(at), AccountID: account, Zone: zone, Prefix: prefix, Transport: "cf", OwnerID: i.owner, CreatedAt: at,
		}, store.Action{ID: ulid.New(at), Actor: store.SystemPrincipalID, Tool: "identity.add", At: at}); err != nil {
			t.Fatal(err)
		}
	}
	local := localraw.Dir{Root: raws}
	bucket := r2fake.New()
	in := ingest.New(ingest.Config{Raw: bucket, Local: local, DB: db, Log: quiet})
	out := outbox.New(outbox.Config{
		DB: db, Raw: bucket, Local: local, Transports: map[string]outbox.Transport{"cf": fakeTransport{}},
		IndexSent: in.IndexSent, Log: quiet,
	})
	sub := submit.New(submit.Config{DB: db, Outbox: out, Log: quiet})
	sp := &spy{DB: db, calls: map[string][]*auth.Viewer{}}
	return &harness{
		t: t, db: db, pool: pool, spy: sp, svc: svc, agent: agent, human: human, sub: sub,
		h: New(sp, localRaw{local}, quiet).WithSubmitter(sub).Handler(svc),
	}
}

// tokenFor issues a credential for p with exactly scopes: a bearer token,
// or for a human a session as a passkey login stores it (do sends it as
// the cookie).
func (h *harness) tokenFor(p store.Principal, scopes ...auth.Scope) string {
	h.t.Helper()
	if p.Kind == "human" {
		now := time.Now()
		sec := auth.NewSecret(auth.PrefixSession, ulid.New(now))
		expires := now.Add(time.Hour)
		if err := h.db.CreateCredential(h.t.Context(), store.Credential{
			ID: sec.ID, PrincipalID: p.ID, Kind: auth.KindSession, Label: "test", SecretHash: sec.Hash[:],
			Scopes: auth.Strings(scopes), CreatedAt: now, ExpiresAt: &expires,
		}, store.Action{ID: ulid.New(now), Actor: p.ID, Tool: "auth.login", At: now}); err != nil {
			h.t.Fatal(err)
		}
		return sec.Text
	}
	tok, err := h.svc.IssueToken(h.t.Context(), authn.TokenRequest{PrincipalID: p.ID, Label: "test", Scopes: scopes, IssuedBy: store.SystemPrincipalID})
	if err != nil {
		h.t.Fatal(err)
	}
	return tok
}

// viewer is p as the store sees it, with every scope.
func viewer(p store.Principal) *auth.Viewer {
	return &auth.Viewer{PrincipalID: p.ID, PrincipalKind: p.Kind, PrincipalName: p.Name, Scopes: auth.AllScopes}
}

// do sends a request with a JSON body, if any.
func (h *harness) do(method, path, body, bearer string) *httptest.ResponseRecorder {
	h.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequestWithContext(h.t.Context(), method, path, rd)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	switch {
	case strings.HasPrefix(bearer, auth.PrefixSession+"_"):
		r.AddCookie(&http.Cookie{Name: authn.SessionCookie, Value: bearer})
	case bearer != "":
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	return w
}

// token issues a bearer token for agent:arisu with exactly scopes.
func (h *harness) token(scopes ...auth.Scope) string {
	h.t.Helper()
	return h.tokenFor(h.agent, scopes...)
}

// find returns the id of the message with exactly this subject.
func (h *harness) find(subject string) string {
	h.t.Helper()
	res := decode[SearchResult](h.t, h.get("/api/v1/search?downgraded=include&limit=500&q="+url.QueryEscape(subject), h.token(auth.MessageRead)))
	for _, m := range res.Hits {
		if m.Subject == subject {
			return m.ID
		}
	}
	h.t.Fatalf("no message %q", subject)
	return ""
}

func (h *harness) get(path, bearer string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(http.MethodGet, path, "", bearer)
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("%s: %v", w.Body.String(), err)
	}
	return v
}

// draft stores a draft written by p, as the store's own callers do.
func (h *harness) draft(p store.Principal, d store.Draft) store.Draft {
	h.t.Helper()
	if d.BodyMD == "" {
		d.BodyMD = "Hello"
	}
	if d.To == nil {
		d.To = []string{"bob@example.net"}
	}
	out, err := h.db.CreateDraft(h.t.Context(), viewer(p), d, time.Now())
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

// queued is an outbox row the operator queued through the core.
func (h *harness) queued() string {
	h.t.Helper()
	d := h.draft(h.human, store.Draft{FromAddr: "me@example.com", Subject: "Queued"})
	res, err := h.sub.SubmitDraft(h.t.Context(), viewer(h.human), d.ID)
	if err != nil || !res.Queued {
		h.t.Fatalf("submit: %+v, %v", res, err)
	}
	return res.OutboxID
}

// unknown is an outbox row whose fate is unknown (§4.5): queued, then
// set as an attempt past its commit point would leave it.
func (h *harness) unknown() string {
	h.t.Helper()
	id := h.queued()
	if _, err := h.pool.Exec(h.t.Context(), `UPDATE outbox SET state = 'unknown', next_retry_at = NULL WHERE id = $1`, id); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.pool.Exec(h.t.Context(), `UPDATE outbox_recipients SET state = 'unknown' WHERE outbox_id = $1`, id); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// TestACL walks the route table (AGENTS.md: visibility is computed in
// one place, and new read paths join this table). For every route it
// checks that an unauthenticated request never reaches the store, that
// the scope is enforced, and that the authenticated viewer is the one the
// store reads and writes for. Drafts are visible to their author and to
// humans only (§13.8), so another agent with every scope gets 404 for
// one, never its content. Restricted accounts (account_access, M3.5) are
// checked per read path in the store's TestVisibility.
func TestACL(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	page := decode[MessagePage](t, h.get("/api/v1/messages?limit=1", h.token(auth.MessageRead)))
	msg := page.Messages[0]
	att := h.find("Invoice and logo")
	mine := func() string { return h.draft(h.agent, store.Draft{FromAddr: "arisu@example.com"}).ID }
	get, upd, del, sub := mine(), mine(), mine(), mine()
	sent, unknown := h.queued(), h.unknown()
	other := principalNamed(t, h, "agent", "momoi")
	h.spy.take()

	// How to call each route, and the store methods it must go through.
	type call struct {
		method, path, body string
		as                 store.Principal // zero: agent:arisu
		reads              []string
		// private: the route acts on arisu's draft, which another agent
		// must not be able to tell from a missing one.
		private bool
	}
	calls := map[string]call{
		"GET /api/v1/messages":                         {path: "/api/v1/messages?prefix=github@example.com", reads: []string{"ListMessages"}},
		"GET /api/v1/messages/{id}":                    {path: "/api/v1/messages/" + msg.ID, reads: []string{"MessageByID"}},
		"GET /api/v1/messages/{id}/view":               {path: "/api/v1/messages/" + msg.ID + "/view?level=sections", reads: []string{"MessageByID"}},
		"GET /api/v1/messages/{id}/attachments/{part}": {path: "/api/v1/messages/" + att + "/attachments/2", reads: []string{"MessageByID"}},
		"GET /api/v1/threads/{id}":                     {path: "/api/v1/threads/" + msg.ThreadID, reads: []string{"ThreadByID"}},
		"GET /api/v1/prefixes":                         {path: "/api/v1/prefixes", reads: []string{"PrefixStats"}},
		"GET /api/v1/search":                           {path: "/api/v1/search?q=verification", reads: []string{"Search"}},
		"GET /api/v1/messages/{id}/reply-identities": {
			path: "/api/v1/messages/" + msg.ID + "/reply-identities", reads: []string{"ReplyIdentities", "Standings"},
		},
		"GET /api/v1/identities": {path: "/api/v1/identities", reads: []string{"Standings"}},
		"GET /api/v1/drafts":     {path: "/api/v1/drafts?state=draft,held,outbox", reads: []string{"Drafts"}},
		"POST /api/v1/drafts": {
			method: http.MethodPost, path: "/api/v1/drafts",
			body:  `{"in_reply_to":"` + msg.ID + `","to":["bob@example.net"],"body_markdown":"Hi"}`,
			reads: []string{"ReplyIdentities", "CreateDraft"},
		},
		"GET /api/v1/drafts/{id}": {path: "/api/v1/drafts/" + get, reads: []string{"DraftByID"}, private: true},
		"PUT /api/v1/drafts/{id}": {
			method: http.MethodPut, path: "/api/v1/drafts/" + upd, body: `{"from":"arisu@example.com","subject":"Edited"}`,
			reads: []string{"UpdateDraft"}, private: true,
		},
		"DELETE /api/v1/drafts/{id}": {method: http.MethodDelete, path: "/api/v1/drafts/" + del, reads: []string{"DeleteDraft"}, private: true},
		// The submission core reads the store itself, as the viewer.
		"POST /api/v1/drafts/{id}/submit": {method: http.MethodPost, path: "/api/v1/drafts/" + sub + "/submit", private: true},
		"GET /api/v1/outbox":              {path: "/api/v1/outbox?state=queued,unknown", reads: []string{"OutboxList"}},
		"GET /api/v1/outbox/{id}":         {path: "/api/v1/outbox/" + sent, reads: []string{"OutboxByID"}},
		"POST /api/v1/outbox/{id}/resolve": {
			method: http.MethodPost, path: "/api/v1/outbox/" + unknown + "/resolve", body: `{"resolution":"abandon"}`,
			as: h.human, reads: []string{"OutboxByID"},
		},
	}
	for _, rt := range routes {
		pattern := rt.Method + " " + rt.Path
		if rt.Access == openapi.Public {
			if pattern != "GET /api/v1/openapi.json" {
				t.Errorf("%s is public", pattern)
			}
			continue
		}
		row, ok := calls[pattern]
		if !ok {
			t.Errorf("route %s is missing from the ACL table", pattern)
			continue
		}
		if rt.Access != openapi.Authenticated || len(rt.Scopes) == 0 {
			t.Errorf("%s: access %v, scopes %v", pattern, rt.Access, rt.Scopes)
		}
		if row.method == "" {
			row.method = http.MethodGet
		}
		if row.as.ID == "" {
			row.as = h.agent
		}
		send := func(bearer string) *httptest.ResponseRecorder {
			return h.do(row.method, row.path, row.body, bearer)
		}

		if w := send(""); w.Code != http.StatusUnauthorized || problemOf(t, w).Code != openapi.CodeUnauthenticated {
			t.Errorf("%s anonymous: %d %s", pattern, w.Code, w.Body.String())
		}
		if w := send("ykt_bogus"); w.Code != http.StatusUnauthorized {
			t.Errorf("%s bad token: %d", pattern, w.Code)
		}
		// Every scope except the required ones is not enough.
		var others []auth.Scope
		for _, s := range auth.AllScopes {
			if !slices.Contains(rt.Scopes, s) {
				others = append(others, s)
			}
		}
		if w := send(h.tokenFor(row.as, others...)); w.Code != http.StatusForbidden || problemOf(t, w).Code != openapi.CodeScopeRequired {
			t.Errorf("%s without %v: %d %s", pattern, rt.Scopes, w.Code, w.Body.String())
		}
		if calls := h.spy.take(); len(calls) != 0 {
			t.Errorf("%s: refused requests reached the store: %v", pattern, calls)
		}
		if row.private {
			if w := send(h.tokenFor(other, auth.AllScopes...)); w.Code != http.StatusNotFound || problemOf(t, w).Code != openapi.CodeNotFound {
				t.Errorf("%s as another agent: %d %s", pattern, w.Code, w.Body.String())
			}
			h.spy.take()
		}

		w := send(h.tokenFor(row.as, rt.Scopes...))
		if w.Code != openapi.StatusOf(rt.Op) {
			t.Errorf("%s: %d %s", pattern, w.Code, w.Body.String())
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" && w.Code != http.StatusNoContent {
			t.Errorf("%s: Cache-Control %q", pattern, cc)
		}
		if rt.Response != nil {
			noNull(t, pattern, decode[any](t, w))
		}
		calls := h.spy.take()
		var ops []string
		for op, vs := range calls {
			ops = append(ops, op)
			for _, v := range vs {
				if v == nil || v.PrincipalID != row.as.ID {
					t.Errorf("%s: %s for %v, want %s", pattern, op, v, row.as.ID)
				}
			}
		}
		slices.Sort(ops)
		want := slices.Sorted(slices.Values(row.reads))
		if !slices.Equal(ops, want) {
			t.Errorf("%s: store calls %v, want %v", pattern, ops, want)
		}
	}
}

func principalNamed(t *testing.T, h *harness, kind, name string) store.Principal {
	t.Helper()
	p, err := h.db.EnsurePrincipal(t.Context(), store.Principal{ID: ulid.New(time.Now()), Kind: kind, Name: name, DisplayName: name, CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// noNull fails on any null in a response: the schema promises arrays are
// arrays and absent values are left out.
func noNull(t *testing.T, at string, v any) {
	t.Helper()
	switch v := v.(type) {
	case nil:
		t.Errorf("%s: null", at)
	case map[string]any:
		for k, e := range v {
			noNull(t, at+"."+k, e)
		}
	case []any:
		for i, e := range v {
			noNull(t, fmt.Sprintf("%s[%d]", at, i), e)
		}
	}
}

func problemOf(t *testing.T, w *httptest.ResponseRecorder) openapi.Problem {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type %q: %s", ct, w.Body.String())
	}
	p := decode[openapi.Problem](t, w)
	if p.Status != w.Code || p.Title != http.StatusText(w.Code) || p.Detail == "" {
		t.Errorf("problem %+v for %d", p, w.Code)
	}
	return p
}

func TestDeliveriesNeedDeliveryRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	read := h.token(auth.MessageRead)
	page := decode[MessagePage](t, h.get("/api/v1/messages?limit=1", read))
	path := "/api/v1/messages/" + page.Messages[0].ID
	if m := decode[Message](t, h.get(path, read)); m.Deliveries != nil || m.BodyText == "" {
		t.Errorf("without delivery:read: %+v", m.Deliveries)
	}
	if m := decode[Message](t, h.get(path, h.token(auth.MessageRead, auth.DeliveryRead))); len(m.Deliveries) == 0 {
		t.Error("with delivery:read: no deliveries")
	}
}

// The structured view is the tree get_message gives agents, with the same
// rule for delivery facts as the message endpoint.
func TestMessageView(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	read := h.token(auth.MessageRead)
	res := decode[SearchResult](t, h.get("/api/v1/search?downgraded=include&q=confirm+your+account", read))
	i := slices.IndexFunc(res.Hits, func(m MessageSummary) bool { return m.Subject == "Urgent: confirm your account" })
	if i < 0 {
		t.Fatalf("no DMARC-fail phish in %+v", res)
	}
	path := "/api/v1/messages/" + res.Hits[i].ID + "/view"

	v := decode[agentview.View](t, h.get(path, h.token(auth.MessageRead, auth.DeliveryRead)))
	f := v.Facts
	if v.Level != agentview.LevelPart || len(v.Content.Parts) == 0 || f.Auth.DMARC != "fail" ||
		len(f.DeliveredTo) != 1 || !f.Downgraded || len(f.Warnings) == 0 {
		t.Errorf("with delivery:read: %+v", v)
	}
	v = decode[agentview.View](t, h.get(path, read))
	if f := v.Facts; !f.Auth.Withheld || f.Auth.DMARC != "" || len(f.DeliveredTo) != 0 || !f.Downgraded {
		t.Errorf("without delivery:read: %+v", f)
	}
	anchor := v.Content.Parts[0].Blocks[1].Anchor
	from := decode[agentview.View](t, h.get(path+"?anchor="+anchor, read))
	if from.Content.Parts[0].Blocks[0].Anchor != anchor {
		t.Errorf("from %s: %+v", anchor, from.Content.Parts[0].Blocks[0])
	}
	sec := decode[agentview.View](t, h.get(path+"?level=sections", read))
	if sec.Content.Parts != nil || len(sec.Content.Sections) == 0 {
		t.Errorf("sections: %+v", sec.Content)
	}
	for q, code := range map[string]int{
		"?level=all":              http.StatusBadRequest,
		"?anchor=para/1":          http.StatusBadRequest,
		"?anchor=part/9":          http.StatusNotFound,
		"?anchor=part/1/para/999": http.StatusNotFound,
	} {
		if w := h.get(path+q, read); w.Code != code {
			t.Errorf("%s: %d %s", q, w.Code, w.Body.String())
		}
	}
	if w := h.get("/api/v1/messages/"+ulid.New(time.Now())+"/view", read); w.Code != http.StatusNotFound {
		t.Errorf("missing message: %d", w.Code)
	}
}

func TestMessageListPaging(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tok := h.token(auth.MessageRead)
	all := decode[MessagePage](t, h.get("/api/v1/messages?downgraded=include&limit=500", tok))
	if all.NextCursor != "" || len(all.Messages) < 30 {
		t.Fatalf("all: %d messages, cursor %q", len(all.Messages), all.NextCursor)
	}
	var ids []string
	next := "/api/v1/messages?downgraded=include&limit=7"
	for next != "" {
		p := decode[MessagePage](t, h.get(next, tok))
		for _, m := range p.Messages {
			ids = append(ids, m.ID)
		}
		next = ""
		if p.NextCursor != "" {
			next = "/api/v1/messages?downgraded=include&limit=7&cursor=" + p.NextCursor
		}
	}
	if len(ids) != len(all.Messages) || ids[0] != all.Messages[0].ID || ids[len(ids)-1] != all.Messages[len(all.Messages)-1].ID {
		t.Errorf("paged %d, want %d", len(ids), len(all.Messages))
	}

	conv := decode[MessagePage](t, h.get("/api/v1/messages?downgraded=include&limit=500&class=human,unknown", tok))
	if len(conv.Messages) == 0 || slices.ContainsFunc(conv.Messages, func(m MessageSummary) bool { return m.Class == agentview.ClassMachine }) {
		t.Errorf("class=human,unknown: %d messages, machine among them or none", len(conv.Messages))
	}

	for _, bad := range []string{"prefix=nozone", "class=spam", "class=human,", "downgraded=maybe", "cursor=%21%21", "limit=0", "limit=501", "limit=x"} {
		if w := h.get("/api/v1/messages?"+bad, tok); w.Code != http.StatusBadRequest || problemOf(t, w).Code != openapi.CodeInvalidParam {
			t.Errorf("%s: %d %s", bad, w.Code, w.Body.String())
		}
	}
	if w := h.get("/api/v1/messages/"+ulid.New(time.Now()), tok); w.Code != http.StatusNotFound || problemOf(t, w).Code != openapi.CodeNotFound {
		t.Errorf("missing message: %d", w.Code)
	}
	if w := h.get("/api/v1/nope", tok); w.Code != http.StatusNotFound || problemOf(t, w).Code != openapi.CodeNotFound {
		t.Errorf("unknown endpoint: %d %s", w.Code, w.Body.String())
	}
}

func TestPrefixesAndSearch(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ps := decode[PrefixList](t, h.get("/api/v1/prefixes", h.token(auth.PrefixRead)))
	byAddr := map[string]Prefix{}
	for _, p := range ps.Prefixes {
		byAddr[p.Address] = p
	}
	if p := byAddr["github@example.com"]; !p.Claimed || p.Messages == 0 {
		t.Errorf("github: %+v", p)
	}
	if p := byAddr["random456@example.com"]; p.Claimed || p.Downgraded != p.Messages || p.Messages == 0 {
		t.Errorf("random456: %+v", p)
	}
	// The unclaimed list pages; the totals stay those of all mail.
	page := decode[PrefixList](t, h.get("/api/v1/prefixes?state=discovered&limit=2&offset=1", h.token(auth.PrefixRead)))
	discovered := slices.DeleteFunc(slices.Clone(ps.Prefixes), func(p Prefix) bool { return p.Claimed })
	if page.Total != len(discovered) || len(page.Prefixes) != 2 || page.Prefixes[0].Address != discovered[1].Address || page.Messages != ps.Messages {
		t.Errorf("discovered page: %+v", page)
	}
	for _, bad := range []string{"state=spam", "offset=-1", "limit=0", "limit=501"} {
		if w := h.get("/api/v1/prefixes?"+bad, h.token(auth.PrefixRead)); w.Code != http.StatusBadRequest {
			t.Errorf("prefixes %s: %d", bad, w.Code)
		}
	}
	read := h.token(auth.MessageRead)
	// Search pages by cursor; the total is that of all matches.
	all := decode[SearchResult](t, h.get("/api/v1/search?downgraded=include&limit=500", read))
	var ids []string
	next := "/api/v1/search?downgraded=include&limit=9"
	for next != "" {
		p := decode[SearchResult](t, h.get(next, read))
		if p.Total != all.Total {
			t.Fatalf("total %d, want %d", p.Total, all.Total)
		}
		for _, m := range p.Hits {
			ids = append(ids, m.ID)
		}
		next = ""
		if p.NextCursor != "" {
			next = "/api/v1/search?downgraded=include&limit=9&cursor=" + p.NextCursor
		}
	}
	if all.NextCursor != "" || len(ids) != all.Total || ids[len(ids)-1] != all.Hits[len(all.Hits)-1].ID {
		t.Errorf("paged %d search hits, want %d", len(ids), all.Total)
	}
	// Search has no review list: only is not one of its values.
	if w := h.get("/api/v1/search?downgraded=only", read); w.Code != http.StatusBadRequest {
		t.Errorf("search downgraded=only: %d", w.Code)
	}
	// Generated variants may reuse the body under another subject.
	res := decode[SearchResult](t, h.get("/api/v1/search?q=usual+place", read))
	i := slices.IndexFunc(res.Hits, func(m MessageSummary) bool { return m.Subject == "Re: Lunch on Friday?" })
	if i < 0 || res.Hits[i].Class != agentview.ClassHuman {
		t.Fatalf("search: %+v", res)
	}
	// devseed adds the question and an answer around the corpus reply.
	th := decode[Thread](t, h.get("/api/v1/threads/"+res.Hits[i].ThreadID, read))
	if len(th.Messages) != 3 || th.Subject != "Lunch on Friday?" || len(th.Participants) != 2 {
		t.Errorf("thread: %+v", th)
	}
}

// Attachments are downloads, never rendered: mail is untrusted (§8.3).
func TestAttachmentDownload(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	read := h.token(auth.MessageRead)
	id := h.find("Invoice and logo")
	view := decode[agentview.View](t, h.get("/api/v1/messages/"+id+"/view", read))
	if len(view.Content.Attachments) != 1 || view.Content.Attachments[0].Section != "2" {
		t.Fatalf("attachments %+v", view.Content.Attachments)
	}
	w := h.get("/api/v1/messages/"+id+"/attachments/2", read)
	if w.Code != http.StatusOK || w.Body.String() != "%PDF-1.4\n% synthetic\n" {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
	for k, v := range map[string]string{
		"Content-Type":                 "application/pdf",
		"Content-Disposition":          `attachment; filename="invoice.pdf"; filename*=UTF-8''invoice.pdf`,
		"X-Content-Type-Options":       "nosniff",
		"Content-Security-Policy":      "default-src 'none'; sandbox",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Cache-Control":                "no-store",
		"Content-Length":               "21",
	} {
		if got := w.Header().Get(k); got != v {
			t.Errorf("%s: %q, want %q", k, got, v)
		}
	}
	var args, result map[string]any
	var actor string
	if err := h.pool.QueryRow(t.Context(), `SELECT actor, args, result FROM actions WHERE tool = 'api.attachment.download'`).Scan(&actor, &args, &result); err != nil {
		t.Fatal(err)
	}
	if actor != h.agent.ID || args["message_id"] != id || args["part"] != "2" || len(args) != 2 || result["bytes"] != float64(21) {
		t.Errorf("recorded %s %v %v", actor, args, result)
	}

	for path, code := range map[string]int{
		"/attachments/1":   http.StatusNotFound, // the multipart/related container
		"/attachments/1.1": http.StatusNotFound, // the alternative with the text
		"/attachments/1.2": http.StatusNotFound, // the inline logo is part of the HTML
		"/attachments/3":   http.StatusNotFound,
		"/attachments/x":   http.StatusBadRequest,
		"/attachments/02":  http.StatusBadRequest,
		"/attachments/2.":  http.StatusBadRequest,
	} {
		if w := h.get("/api/v1/messages/"+id+path, read); w.Code != code {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	if w := h.get("/api/v1/messages/"+ulid.New(time.Now())+"/attachments/2", read); w.Code != http.StatusNotFound {
		t.Errorf("missing message: %d", w.Code)
	}
}

func TestContentDisposition(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"invoice.pdf":      `attachment; filename="invoice.pdf"; filename*=UTF-8''invoice.pdf`,
		"../../etc/passwd": `attachment; filename="passwd"; filename*=UTF-8''passwd`,
		`C:\\x\\a "b".txt`: `attachment; filename="a _b_.txt"; filename*=UTF-8''a%20%22b%22.txt`,
		"报告.pdf":           `attachment; filename="__.pdf"; filename*=UTF-8''%E6%8A%A5%E5%91%8A.pdf`,
		"inv\u202eexe.txt": `attachment; filename="invexe.txt"; filename*=UTF-8''invexe.txt`,
		"a\r\nb.txt":       `attachment; filename="ab.txt"; filename*=UTF-8''ab.txt`,
		".hidden":          `attachment; filename="hidden"; filename*=UTF-8''hidden`,
		"100%.txt":         `attachment; filename="100_.txt"; filename*=UTF-8''100%25.txt`,
		"":                 `attachment; filename="attachment-1-2"; filename*=UTF-8''attachment-1-2`,
		"..":               `attachment; filename="attachment-1-2"; filename*=UTF-8''attachment-1-2`,
	} {
		if got := contentDisposition(name, "1.2"); got != want {
			t.Errorf("%q: %s, want %s", name, got, want)
		}
	}
	long := strings.Repeat("长", 100) + ".pdf"
	got := safeFilename(long, "2")
	if len(got) > maxFilename || !strings.HasSuffix(got, ".pdf") || !utf8.ValidString(got) {
		t.Errorf("long name: %d bytes %q", len(got), got)
	}
}
