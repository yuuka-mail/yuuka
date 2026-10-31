package api

import (
	"context"
	"encoding/json"
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
	"github.com/yuuka-mail/yuuka/internal/localraw"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
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

// localRaw reads raw messages the way ingest does without R2.
type localRaw struct{ localraw.Dir }

func (l localRaw) ReadRaw(_ context.Context, ref store.RawRef) ([]byte, error) {
	return l.Get(ref.LocalPath)
}

type harness struct {
	t     *testing.T
	db    *store.DB
	pool  *pgxpool.Pool
	spy   *spy
	svc   *authn.Service
	agent store.Principal
	h     http.Handler
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
	agent, err := db.EnsurePrincipal(t.Context(), store.Principal{ID: ulid.New(time.Now()), Kind: "agent", Name: "arisu", DisplayName: "arisu", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	sp := &spy{DB: db, calls: map[string][]*auth.Viewer{}}
	return &harness{t: t, db: db, pool: pool, spy: sp, svc: svc, agent: agent, h: New(sp, localRaw{localraw.Dir{Root: raws}}, quiet).Handler(svc)}
}

// token issues a bearer token for agent:arisu with exactly scopes.
func (h *harness) token(scopes ...auth.Scope) string {
	h.t.Helper()
	tok, err := h.svc.IssueToken(h.t.Context(), authn.TokenRequest{PrincipalID: h.agent.ID, Label: "test", Scopes: scopes, IssuedBy: store.SystemPrincipalID})
	if err != nil {
		h.t.Fatal(err)
	}
	return tok
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
	r := httptest.NewRequestWithContext(h.t.Context(), http.MethodGet, path, nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	return w
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("%s: %v", w.Body.String(), err)
	}
	return v
}

// TestACL is the table of every read endpoint (AGENTS.md: visibility is
// computed in one place, and new read paths join this table). In M1 it
// checks that an unauthenticated request never reaches the store, that the
// scope is enforced, and that the authenticated viewer is the one the
// store reads for. When account_access arrives (M3.5), each row also gets
// a restricted viewer and asserts nothing leaks.
func TestACL(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	page := decode[MessagePage](t, h.get("/api/v1/messages?limit=1", h.token(auth.MessageRead)))
	msg := page.Messages[0]
	att := h.find("Invoice and logo")
	h.spy.take()

	table := []struct {
		pattern string // as registered in routes
		path    string
		scopes  []auth.Scope
		reads   string // the store method it must call
	}{
		{"GET /api/v1/messages", "/api/v1/messages?prefix=github@example.com", []auth.Scope{auth.MessageRead}, "ListMessages"},
		{"GET /api/v1/messages/{id}", "/api/v1/messages/" + msg.ID, []auth.Scope{auth.MessageRead}, "MessageByID"},
		{"GET /api/v1/messages/{id}/view", "/api/v1/messages/" + msg.ID + "/view?level=sections", []auth.Scope{auth.MessageRead}, "MessageByID"},
		{"GET /api/v1/messages/{id}/attachments/{part}", "/api/v1/messages/" + att + "/attachments/2", []auth.Scope{auth.MessageRead}, "MessageByID"},
		{"GET /api/v1/threads/{id}", "/api/v1/threads/" + msg.ThreadID, []auth.Scope{auth.MessageRead}, "ThreadByID"},
		{"GET /api/v1/prefixes", "/api/v1/prefixes", []auth.Scope{auth.PrefixRead}, "PrefixStats"},
		{"GET /api/v1/search", "/api/v1/search?q=verification", []auth.Scope{auth.MessageRead}, "Search"},
	}
	var covered []string
	for _, row := range table {
		covered = append(covered, row.pattern)
		i := slices.IndexFunc(routes, func(r route) bool { return r.pattern == row.pattern })
		if i < 0 {
			t.Errorf("%s: not a route", row.pattern)
			continue
		}
		if !slices.Equal(routes[i].scopes, row.scopes) {
			t.Errorf("%s: route requires %v, table says %v", row.pattern, routes[i].scopes, row.scopes)
		}

		if w := h.get(row.path, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s anonymous: %d", row.path, w.Code)
		}
		if w := h.get(row.path, "ykt_bogus"); w.Code != http.StatusUnauthorized {
			t.Errorf("%s bad token: %d", row.path, w.Code)
		}
		// Every scope except the required ones is not enough.
		var others []auth.Scope
		for _, s := range auth.AllScopes {
			if !slices.Contains(row.scopes, s) {
				others = append(others, s)
			}
		}
		if w := h.get(row.path, h.token(others...)); w.Code != http.StatusForbidden {
			t.Errorf("%s without %v: %d", row.path, row.scopes, w.Code)
		}
		if calls := h.spy.take(); len(calls) != 0 {
			t.Errorf("%s: refused requests reached the store: %v", row.path, calls)
		}

		w := h.get(row.path, h.token(row.scopes...))
		if w.Code != http.StatusOK {
			t.Errorf("%s: %d %s", row.path, w.Code, w.Body.String())
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s: Cache-Control %q", row.path, cc)
		}
		calls := h.spy.take()
		vs := calls[row.reads]
		if len(calls) != 1 || len(vs) != 1 || vs[0] == nil || vs[0].PrincipalID != h.agent.ID {
			t.Errorf("%s: store calls %v, want one %s for %s", row.path, calls, row.reads, h.agent.ID)
		}
	}
	for _, r := range routes {
		if !slices.Contains(covered, r.pattern) {
			t.Errorf("route %s is missing from the ACL table", r.pattern)
		}
	}
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
	if len(conv.Messages) == 0 || slices.ContainsFunc(conv.Messages, func(m MessageSummary) bool { return m.Class == ClassMachine }) {
		t.Errorf("class=human,unknown: %d messages, machine among them or none", len(conv.Messages))
	}

	for _, bad := range []string{"prefix=nozone", "class=spam", "class=human,", "downgraded=maybe", "cursor=%21%21", "limit=0", "limit=501"} {
		if w := h.get("/api/v1/messages?"+bad, tok); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
	if w := h.get("/api/v1/messages/"+ulid.New(time.Now()), tok); w.Code != http.StatusNotFound {
		t.Errorf("missing message: %d", w.Code)
	}
	if w := h.get("/api/v1/nope", tok); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "error") {
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
	for _, bad := range []string{"state=spam", "offset=-1", "limit=0"} {
		if w := h.get("/api/v1/prefixes?"+bad, h.token(auth.PrefixRead)); w.Code != http.StatusBadRequest {
			t.Errorf("prefixes %s: %d", bad, w.Code)
		}
	}
	read := h.token(auth.MessageRead)
	// Generated variants may reuse the body under another subject.
	res := decode[SearchResult](t, h.get("/api/v1/search?q=usual+place", read))
	i := slices.IndexFunc(res.Hits, func(m MessageSummary) bool { return m.Subject == "Re: Lunch on Friday?" })
	if i < 0 || res.Hits[i].Class != ClassHuman {
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
