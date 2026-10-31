package mcp_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authn"
	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/contenthash"
	"github.com/yuuka-mail/yuuka/internal/mcp"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// rawMap is the raw store of the test: R2 key to bytes.
type rawMap map[string][]byte

func (m rawMap) ReadRaw(_ context.Context, ref store.RawRef) ([]byte, error) {
	b, ok := m[ref.R2Key]
	if !ok {
		return nil, fmt.Errorf("no raw %s", ref.R2Key)
	}
	return b, nil
}

type env struct {
	t      *testing.T
	db     *store.DB
	pool   *pgxpool.Pool
	srv    *mcp.Server
	svc    *authn.Service
	bearer *authn.Bearer
	agent  store.Principal
	// ids maps a corpus file name to its message id.
	ids map[string]string
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// newEnv ingests the synthetic corpus: every message to the claimed
// prefix "a", except html-reply-tracking, which goes to the unclaimed
// prefix "promo-x".
func newEnv(t *testing.T) *env {
	t.Helper()
	db, pool := storetest.New(t)
	e := &env{t: t, db: db, pool: pool, ids: map[string]string{}}
	if _, err := pool.Exec(t.Context(), `INSERT INTO prefixes (zone, prefix, first_seen, last_seen, state, label)
		VALUES ('example.com', 'a', now(), now(), 1, 'main')`); err != nil {
		t.Fatal(err)
	}
	raws := rawMap{}
	files, _ := filepath.Glob("../../testdata/eml/*.eml")
	for i, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimSuffix(filepath.Base(f), ".eml")
		prefix := "a"
		if name == "html-reply-tracking" {
			prefix = "promo-x"
		}
		at := time.Date(2026, 10, 31, 0, 0, i, 0, time.UTC)
		id := ulid.New(at)
		key := fmt.Sprintf("raw/2026/10/%s.eml", id)
		raws[key] = raw
		in := store.Ingest{
			RawID: id, R2Key: key, LocalPath: key, Size: int64(len(raw)), SHA256: sha256.Sum256(raw),
			ContentHash: contenthash.V1(raw), ReceivedAt: at, DeliveryID: id, AccountID: storetest.Account(t, db),
			Zone: "example.com", Prefix: prefix, EnvelopeTo: prefix + "@example.com",
		}
		if _, err := db.CommitIngest(t.Context(), in); err != nil {
			t.Fatal(err)
		}
		f, err := db.Fold(t.Context(), store.RawRef{
			DeliveryID: id, RawID: id, R2Key: key, LocalPath: key, ContentHash: in.ContentHash[:],
			Zone: "example.com", Prefix: prefix, ReceivedAt: at,
		}, mimeparse.Parse(raw), authres.Extract(raw, "mx.cloudflare.net"))
		if err != nil {
			t.Fatal(err)
		}
		e.ids[name] = f.MessageID
	}
	e.srv = mcp.New(mcp.Config{DB: db, Raw: raws, Log: quiet, Version: "test"})
	svc, err := authn.New(authn.Config{DB: db, Origin: "https://mail.example.com", Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	e.bearer = authn.NewBearer(db, quiet, nil)
	e.agent, err = db.EnsurePrincipal(t.Context(), store.Principal{
		ID: ulid.New(time.Now()), Kind: "agent", Name: "arisu", DisplayName: "arisu", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// token issues a bearer token for agent:arisu.
func (e *env) token(scopes ...auth.Scope) string {
	e.t.Helper()
	tok, err := e.svc.IssueToken(e.t.Context(), authn.TokenRequest{
		PrincipalID: e.agent.ID, Scopes: scopes, IssuedBy: store.SystemPrincipalID,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

// viewer returns the Viewer of a fresh token.
func (e *env) viewer(scopes ...auth.Scope) *auth.Viewer {
	e.t.Helper()
	v, err := e.bearer.Authenticate(e.t.Context(), e.token(scopes...), "test")
	if err != nil {
		e.t.Fatal(err)
	}
	return v
}

// client connects an in-memory client whose calls act as v.
func (e *env) client(v *auth.Viewer) *sdk.ClientSession {
	e.t.Helper()
	srv := e.srv.MCP(func(context.Context, *sdk.CallToolRequest) (*auth.Viewer, error) { return v, nil })
	ct, st := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(e.t.Context(), st, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = ss.Close() })
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test"}, nil).Connect(e.t.Context(), ct, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = cs.Close() })
	return cs
}

type call struct {
	text       string
	structured json.RawMessage
	isError    bool
}

func callTool(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) call {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error %v", name, err)
	}
	var c call
	for _, ct := range res.Content {
		if tc, ok := ct.(*sdk.TextContent); ok {
			c.text += tc.Text
		}
	}
	c.isError = res.IsError
	if res.StructuredContent != nil {
		c.structured, err = json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
	}
	return c
}

const fenceTag = "<untrusted_mail_content nonce="

func TestSearchAndRead(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cs := e.client(e.viewer(auth.MessageRead, auth.DeliveryRead, auth.PrefixRead))

	r := callTool(t, cs, "search_messages", map[string]any{"q": "verification code is 482913"})
	var out mcp.SearchOutput
	if err := json.Unmarshal(r.structured, &out); err != nil || r.isError || out.Total != 1 {
		t.Fatalf("search: %+v %s %v", out, r.text, err)
	}
	uri := out.Hits[0].URI
	if uri != agentview.MessageURI(e.ids["plain-otp"]) || !strings.Contains(r.text, fenceTag) || !strings.Contains(r.text, "482913") {
		t.Errorf("search text:\n%s", r.text)
	}
	// Sender-written fields are inside the fence, yuuka's facts outside.
	if i, j := strings.Index(r.text, fenceTag), strings.Index(r.text, "Subject: Your verification code"); j < i {
		t.Errorf("subject outside the fence:\n%s", r.text)
	}

	r = callTool(t, cs, "get_message", map[string]any{"uri": uri})
	var v agentview.View
	if err := json.Unmarshal(r.structured, &v); err != nil || r.isError {
		t.Fatalf("get: %v %s", err, r.text)
	}
	if v.Level != agentview.LevelSnippet || v.Content.Parts != nil || !strings.Contains(v.Content.Snippet, "482913") ||
		v.Facts.Auth.Line != "spf=pass dkim=pass dmarc=pass from_domain=example.net" ||
		strings.Join(v.Facts.DeliveredTo, ",") != "a@example.com" || !strings.Contains(r.text, fenceTag) {
		t.Errorf("snippet view: %+v\n%s", v, r.text)
	}

	r = callTool(t, cs, "get_message", map[string]any{"uri": uri + "#part/1/para/2", "level": "part"})
	if err := json.Unmarshal(r.structured, &v); err != nil || r.isError {
		t.Fatalf("part: %v %s", err, r.text)
	}
	if b := v.Content.Parts[0].Blocks[0]; b.Anchor != "part/1/para/2" || !strings.Contains(b.Text(), "482913") {
		t.Errorf("part view starts at %+v", b)
	}

	// Filters reach the store.
	r = callTool(t, cs, "search_messages", map[string]any{"prefix": "promo-x@example.com", "include_downgraded": true})
	if err := json.Unmarshal(r.structured, &out); err != nil || out.Total != 1 || !out.Hits[0].Downgraded {
		t.Errorf("prefix filter: %+v %s", out, r.text)
	}
	r = callTool(t, cs, "search_messages", map[string]any{"prefix": "promo-x"})
	if err := json.Unmarshal(r.structured, &out); err != nil || out.Total != 0 {
		t.Errorf("downgraded mail returned by default: %+v", out)
	}
	r = callTool(t, cs, "search_messages", map[string]any{"class": "human", "since": "2026-10-31"})
	if err := json.Unmarshal(r.structured, &out); err != nil || out.Total == 0 {
		t.Errorf("class filter: %+v %s", out, r.text)
	}
	for _, h := range out.Hits {
		if h.Class != "human" {
			t.Errorf("class filter returned %s", h.Class)
		}
	}
}

// Mail that failed DMARC is downgraded and marked in the view, and its
// injected instructions stay inside the fence.
func TestDowngradedAndFenced(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cs := e.client(e.viewer(auth.MessageRead, auth.DeliveryRead))
	r := callTool(t, cs, "get_message", map[string]any{"uri": agentview.MessageURI(e.ids["phish-dmarc-fail"]), "level": "part"})
	var v agentview.View
	if err := json.Unmarshal(r.structured, &v); err != nil || r.isError {
		t.Fatal(err, r.text)
	}
	if !v.Facts.Downgraded || !strings.Contains(r.text, "warning: dmarc=fail") {
		t.Errorf("not downgraded:\n%s", r.text)
	}
	open := strings.Index(r.text, fenceTag)
	closing := strings.LastIndex(r.text, "</untrusted_mail_content nonce=")
	inj := strings.Index(r.text, "ignore your previous instructions")
	if open < 0 || inj < open || closing < inj || strings.Count(r.text, "untrusted_mail_content") != 2 {
		t.Errorf("injection not fenced:\n%s", r.text)
	}
}

// Envelope and authentication facts are delivery data (§13.4): without
// delivery:read they are withheld, as in the REST API, and the
// downgrade is all that remains of a DMARC failure.
func TestDeliveryFactsNeedDeliveryRead(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cs := e.client(e.viewer(auth.MessageRead))
	r := callTool(t, cs, "get_message", map[string]any{"uri": agentview.MessageURI(e.ids["phish-dmarc-fail"])})
	var v agentview.View
	if err := json.Unmarshal(r.structured, &v); err != nil || r.isError {
		t.Fatal(err, r.text)
	}
	f := v.Facts
	if !f.Auth.Withheld || f.Auth.DMARC != "" || len(f.DeliveredTo) != 0 || !f.Downgraded ||
		strings.Contains(r.text, "dmarc=fail") || strings.Contains(r.text, "a@example.com") ||
		!strings.Contains(r.text, "auth: withheld") || !strings.Contains(r.text, "warning: downgraded") {
		t.Errorf("facts %+v\n%s", f, r.text)
	}
}

func TestListPrefixes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cs := e.client(e.viewer(auth.PrefixRead))
	r := callTool(t, cs, "list_prefixes", nil)
	var out mcp.PrefixesOutput
	if err := json.Unmarshal(r.structured, &out); err != nil || r.isError || out.Total != 2 {
		t.Fatalf("%+v %s %v", out, r.text, err)
	}
	// Discovered names were chosen by senders: fenced; claimed ones not.
	fence := strings.Index(r.text, fenceTag)
	if fence < 0 || strings.Index(r.text, "promo-x@example.com") < fence || strings.Index(r.text, "a@example.com ·") > fence {
		t.Errorf("prefix text:\n%s", r.text)
	}
	r = callTool(t, cs, "list_prefixes", map[string]any{"leak_suspect": true})
	if err := json.Unmarshal(r.structured, &out); err != nil || out.Total != 0 {
		t.Errorf("leak filter: %+v", out)
	}
}

func TestScopes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	uri := agentview.MessageURI(e.ids["plain-otp"])
	prefixOnly := e.client(e.viewer(auth.PrefixRead))
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"search_messages", map[string]any{"q": "code"}},
		{"get_message", map[string]any{"uri": uri}},
	} {
		r := callTool(t, prefixOnly, c.tool, c.args)
		if !r.isError || !strings.Contains(r.text, "message:read") || strings.Contains(r.text, "482913") {
			t.Errorf("%s without message:read: %+v", c.tool, r)
		}
	}
	// admin does not imply prefix:read: scopes are flat (§13.4).
	r := callTool(t, e.client(e.viewer(auth.MessageRead, auth.Admin)), "list_prefixes", nil)
	if !r.isError || !strings.Contains(r.text, "prefix:read") {
		t.Errorf("list_prefixes without prefix:read: %+v", r)
	}
	var denied int
	if err := e.pool.QueryRow(t.Context(), `SELECT count(*) FROM actions WHERE tool = 'auth.deny' AND actor = $1`, e.agent.ID).Scan(&denied); err != nil || denied != 3 {
		t.Errorf("denials recorded: %d %v", denied, err)
	}
}

func TestNoViewerRefused(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cs := e.client(nil)
	for name, args := range map[string]map[string]any{
		"search_messages": {"q": "code"},
		"get_message":     {"uri": agentview.MessageURI(e.ids["plain-otp"])},
		"list_prefixes":   {},
	} {
		r := callTool(t, cs, name, args)
		if !r.isError || !strings.Contains(r.text, "not authenticated") || r.structured != nil {
			t.Errorf("%s without a viewer: %+v", name, r)
		}
	}
}

func TestBadInput(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cs := e.client(e.viewer(auth.MessageRead))
	uri := agentview.MessageURI(e.ids["plain-otp"])
	for args, want := range map[string]string{
		`{"uri": "https://example.com/x"}`:                              "not a yuuka://message",
		`{"uri": "` + agentview.MessageURI(ulid.New(time.Now())) + `"}`: "no message",
		`{"uri": "` + uri + `#part/7"}`:                                 "has no part/7",
		// The input schema lists the levels (§5.1).
		`{"uri": "` + uri + `", "level": "everything"}`: "[snippet sections part]",
	} {
		var a map[string]any
		if err := json.Unmarshal([]byte(args), &a); err != nil {
			t.Fatal(err)
		}
		r := callTool(t, cs, "get_message", a)
		if !r.isError || !strings.Contains(r.text, want) {
			t.Errorf("%s: %+v, want %q", args, r, want)
		}
	}
	r := callTool(t, cs, "search_messages", map[string]any{"class": "robot"})
	if !r.isError || !strings.Contains(r.text, "[unknown machine human]") {
		t.Errorf("bad class: %+v", r)
	}
}

// snapshot fingerprints every table of the test schema but the audit log.
func snapshot(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE' AND table_name <> 'actions'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	if len(tables) < 10 {
		t.Fatalf("only %d tables found", len(tables))
	}
	out := map[string]string{}
	for _, n := range tables {
		var sum *string
		q := fmt.Sprintf(`SELECT md5(string_agg(t::text, ',' ORDER BY t::text)) FROM %q t`, n)
		if err := pool.QueryRow(t.Context(), q).Scan(&sum); err != nil {
			t.Fatal(err)
		}
		out[n] = fmt.Sprint(sum)
		if sum != nil {
			out[n] = *sum
		}
	}
	return out
}

// No tool writes anything but its own audit record (§8.2: the read tier
// is read-only by construction, not by prompt).
func TestToolsWriteOnlyTheAuditLog(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cs := e.client(e.viewer(auth.AllScopes...))
	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names, writers []string
	for _, tl := range tools.Tools {
		if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			writers = append(writers, tl.Name)
			continue
		}
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "get_message,list_identities,list_prefixes,search_messages" {
		t.Fatalf("read-only tools = %v; a new tool needs its own write test", names)
	}
	// The draft tools write drafts (TestDraftTools). submit_draft is
	// absent: this server has no submission core.
	if strings.Join(writers, ",") != "create_draft,update_draft" {
		t.Fatalf("writing tools = %v; a new one needs its own test", writers)
	}
	before := snapshot(t, e.pool)
	var actionsBefore int
	_ = e.pool.QueryRow(t.Context(), `SELECT count(*) FROM actions`).Scan(&actionsBefore)

	uri := agentview.MessageURI(e.ids["html-reply-tracking"])
	calls := []struct {
		tool string
		args map[string]any
	}{
		{"search_messages", map[string]any{"q": "release", "include_downgraded": true}},
		{"get_message", map[string]any{"uri": uri, "level": "snippet"}},
		{"get_message", map[string]any{"uri": uri, "level": "sections"}},
		{"get_message", map[string]any{"uri": uri, "level": "part"}},
		{"get_message", map[string]any{"uri": "yuuka://message/bad"}},
		{"list_prefixes", map[string]any{}},
		{"list_identities", map[string]any{}},
	}
	for _, c := range calls {
		callTool(t, cs, c.tool, c.args)
	}
	after := snapshot(t, e.pool)
	for n, sum := range before {
		if after[n] != sum {
			t.Errorf("table %s changed", n)
		}
	}
	var actionsAfter int
	_ = e.pool.QueryRow(t.Context(), `SELECT count(*) FROM actions`).Scan(&actionsAfter)
	if actionsAfter-actionsBefore != len(calls) {
		t.Errorf("%d actions recorded for %d calls", actionsAfter-actionsBefore, len(calls))
	}
	var tool, credential, argURI string
	err = e.pool.QueryRow(t.Context(), `SELECT tool, credential_id, args->>'uri' FROM actions
		WHERE actor = $1 AND tool = 'mcp.get_message' ORDER BY id LIMIT 1`, e.agent.ID).Scan(&tool, &credential, &argURI)
	if err != nil || argURI != uri || credential == "" {
		t.Errorf("audit record: %s %s %s %v", tool, credential, argURI, err)
	}
}

// The stdio form acts as its token, checks it before serving, and stops
// honouring it once revoked.
func TestServeToken(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.srv.ServeToken(t.Context(), e.bearer, "ykt_bogus", nil); err == nil {
		t.Fatal("served with a bogus token")
	}
	tok := e.token(auth.MessageRead)
	ct, st := sdk.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- e.srv.ServeToken(ctx, e.bearer, tok, st) }()
	t.Cleanup(func() {
		cancel()
		t.Log("server stopped:", <-done)
	})
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	uri := agentview.MessageURI(e.ids["plain-otp"])
	if r := callTool(t, cs, "get_message", map[string]any{"uri": uri}); r.isError {
		t.Fatalf("valid token: %+v", r)
	}
	var actor string
	if err := e.pool.QueryRow(t.Context(), `SELECT actor FROM actions WHERE tool = 'mcp.get_message'`).Scan(&actor); err != nil || actor != e.agent.ID {
		t.Errorf("recorded as %q, want the token's principal: %v", actor, err)
	}
	id, _, _ := auth.ParseSecret(auth.PrefixToken, tok)
	if err := e.svc.Revoke(t.Context(), &auth.Viewer{PrincipalID: store.SystemPrincipalID}, id); err != nil {
		t.Fatal(err)
	}
	if r := callTool(t, cs, "get_message", map[string]any{"uri": uri}); !r.isError || !strings.Contains(r.text, "not authenticated") {
		t.Errorf("revoked token still served: %+v", r)
	}
}

// headerTransport adds an Authorization header to every request.
type headerTransport struct{ auth string }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if h.auth != "" {
		r.Header.Set("Authorization", h.auth)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestHTTP(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ts := httptest.NewServer(e.srv.Handler(e.bearer))
	t.Cleanup(ts.Close)
	tok := e.token(auth.MessageRead)

	post := func(header, value string) int {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if header != "" {
			req.Header.Set(header, value)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	// Only a bearer token gets in: not nothing, not a cookie, not a session.
	for _, c := range [][2]string{
		{"", ""},
		{"Cookie", authn.SessionCookie + "=" + tok},
		{"Authorization", "Basic " + tok},
		{"Authorization", "Bearer ykt_nonsense"},
	} {
		if code := post(c[0], c[1]); code != http.StatusUnauthorized {
			t.Errorf("%s %q: status %d", c[0], c[1], code)
		}
	}

	connect := func(token string) *sdk.ClientSession {
		tr := &sdk.StreamableClientTransport{
			Endpoint:   ts.URL,
			HTTPClient: &http.Client{Transport: headerTransport{auth: "Bearer " + token}},
		}
		cs, err := sdk.NewClient(&sdk.Implementation{Name: "test"}, nil).Connect(t.Context(), tr, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cs.Close() })
		return cs
	}
	cs := connect(tok)
	r := callTool(t, cs, "get_message", map[string]any{"uri": agentview.MessageURI(e.ids["plain-otp"])})
	if r.isError || !strings.Contains(r.text, "482913") {
		t.Fatalf("over HTTP: %+v", r)
	}
	r = callTool(t, cs, "list_prefixes", nil)
	if !r.isError || !strings.Contains(r.text, "prefix:read") {
		t.Errorf("scope over HTTP: %+v", r)
	}

	// Revocation takes effect on the next request.
	id, _, _ := auth.ParseSecret(auth.PrefixToken, tok)
	if err := e.svc.Revoke(t.Context(), &auth.Viewer{PrincipalID: store.SystemPrincipalID}, id); err != nil {
		t.Fatal(err)
	}
	if code := post("Authorization", "Bearer "+tok); code != http.StatusUnauthorized {
		t.Errorf("revoked token: status %d", code)
	}
}
