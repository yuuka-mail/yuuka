package authn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/clientip"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

const origin = "https://mail.example.com"

type alerts struct {
	mu   sync.Mutex
	msgs []string
}

func (a *alerts) Notify(_ context.Context, text string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.msgs = append(a.msgs, text)
	return nil
}

func (a *alerts) got() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.msgs...)
}

type harness struct {
	t      *testing.T
	svc    *Service
	db     *store.DB
	pool   *pgxpool.Pool
	now    time.Time
	alerts *alerts
	mux    *http.ServeMux
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db, pool := storetest.New(t)
	h := &harness{t: t, db: db, pool: pool, now: time.Now(), alerts: &alerts{}}
	svc, err := New(Config{
		DB: db, Origin: origin, Alerter: h.alerts,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return h.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	h.svc = svc
	h.mux = http.NewServeMux()
	h.mux.Handle("/auth/", svc.Handler())
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, _ := auth.ViewerFrom(r.Context())
		_, _ = io.WriteString(w, v.String())
	})
	h.mux.Handle("GET /test/read", svc.Require(ok, auth.MessageRead))
	h.mux.Handle("POST /test/admin", svc.Require(ok, auth.Admin))
	return h
}

type resp struct {
	code    int
	body    []byte
	cookies map[string]*http.Cookie
}

// do sends a request; body may be nil, []byte or anything JSON-encodable.
// Each opt may set a header or cookie on the request.
func (h *harness) do(method, path string, body any, opts ...func(*http.Request)) resp {
	h.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			h.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	r := httptest.NewRequestWithContext(h.t.Context(), method, path, rd)
	r.RemoteAddr = "192.0.2.1:4000"
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, r)
	out := resp{code: w.Code, body: w.Body.Bytes(), cookies: map[string]*http.Cookie{}}
	for _, c := range w.Result().Cookies() {
		out.cookies[c.Name] = c
	}
	return out
}

func withCookie(c *http.Cookie) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value}) }
}

func withHeader(k, v string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(k, v) }
}

func (h *harness) want(r resp, code int) {
	h.t.Helper()
	if r.code != code {
		h.t.Fatalf("status %d, want %d: %s", r.code, code, r.body)
	}
}

func (h *harness) count(query string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *harness) enroll() Enrollment {
	h.t.Helper()
	e, err := h.svc.Enroll(h.t.Context(), "", DefaultEnrollTTL)
	if err != nil {
		h.t.Fatal(err)
	}
	return e
}

func tokenOf(e Enrollment) string {
	_, tok, _ := strings.Cut(e.URL, "#")
	return tok
}

// beginRegister returns the ceremony cookie and the creation options.
func (h *harness) beginRegister(token string, opts ...func(*http.Request)) (*http.Cookie, []byte) {
	h.t.Helper()
	r := h.do(http.MethodPost, "/auth/passkey/register/begin", map[string]string{"enrollment_token": token}, opts...)
	h.want(r, http.StatusOK)
	return r.cookies[ceremonyCookie], r.body
}

func (h *harness) register(key *softKey, token string) resp {
	h.t.Helper()
	c, opts := h.beginRegister(token)
	return h.do(http.MethodPost, "/auth/passkey/register/finish", key.create(opts), withCookie(c))
}

func (h *harness) login(key *softKey) resp {
	h.t.Helper()
	r := h.do(http.MethodPost, "/auth/passkey/login/begin", nil)
	h.want(r, http.StatusOK)
	key.counter++
	return h.do(http.MethodPost, "/auth/passkey/login/finish", key.get(r.body), withCookie(r.cookies[ceremonyCookie]))
}

// enrolled returns a harness with one registered passkey and a session.
func enrolled(t *testing.T) (*harness, *softKey, *http.Cookie) {
	t.Helper()
	h := newHarness(t)
	key := newSoftKey(t, origin)
	h.want(h.register(key, tokenOf(h.enroll())), http.StatusCreated)
	r := h.login(key)
	h.want(r, http.StatusOK)
	s := r.cookies[SessionCookie]
	if s == nil || !s.HttpOnly || !s.Secure || s.SameSite != http.SameSiteStrictMode || s.Path != "/" {
		t.Fatalf("session cookie %+v", s)
	}
	return h, key, s
}

func TestEnrollRegisterLogin(t *testing.T) {
	t.Parallel()
	h, _, session := enrolled(t)

	r := h.do(http.MethodGet, "/auth/session", nil, withCookie(session))
	h.want(r, http.StatusOK)
	var who struct {
		Principal      string   `json:"principal"`
		CredentialKind string   `json:"credential_kind"`
		Scopes         []string `json:"scopes"`
	}
	if err := json.Unmarshal(r.body, &who); err != nil {
		t.Fatal(err)
	}
	if who.Principal != "human:operator" || who.CredentialKind != "session" || len(who.Scopes) != len(auth.AllScopes) {
		t.Fatalf("viewer %+v", who)
	}
	for _, tool := range []string{"auth.enroll.issue", "auth.passkey.register", "auth.login"} {
		if h.count(`SELECT count(*) FROM actions WHERE tool = $1`, tool) != 1 {
			t.Errorf("no %s action", tool)
		}
	}
	if h.count(`SELECT count(*) FROM actions WHERE tool = 'auth.login' AND actor IS NOT NULL AND credential_id IS NOT NULL`) != 1 {
		t.Error("login action lacks actor or credential")
	}
	if got := h.alerts.got(); len(got) != 1 || !strings.Contains(got[0], "new passkey") {
		t.Errorf("alerts %q", got)
	}
	// Only a hash of the session secret is stored.
	if h.count(`SELECT count(*) FROM credentials WHERE kind = 'session' AND octet_length(secret_hash) = 32`) != 1 {
		t.Error("session not stored as a hash")
	}
}

func TestRegistrationNeverCreatesSession(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.register(newSoftKey(t, origin), tokenOf(h.enroll()))
	h.want(r, http.StatusCreated)
	if _, ok := r.cookies[SessionCookie]; ok {
		t.Fatal("registration set a session cookie")
	}
	if h.count(`SELECT count(*) FROM credentials`) != 0 {
		t.Fatal("registration created a credential")
	}
}

func TestEnrollmentTokenReplay(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tok := tokenOf(h.enroll())
	h.want(h.register(newSoftKey(t, origin), tok), http.StatusCreated)

	r := h.do(http.MethodPost, "/auth/passkey/register/begin", map[string]string{"enrollment_token": tok})
	h.want(r, http.StatusUnauthorized)
	if h.count(`SELECT count(*) FROM actions WHERE tool = 'auth.enroll.reject' AND args->>'reason' = 'used'`) != 1 {
		t.Error("replay not recorded")
	}
}

func TestEnrollmentTokenRaceRegistersOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tok := tokenOf(h.enroll())
	a, b := newSoftKey(t, origin), newSoftKey(t, origin)
	ca, oa := h.beginRegister(tok)
	cb, ob := h.beginRegister(tok)
	h.want(h.do(http.MethodPost, "/auth/passkey/register/finish", a.create(oa), withCookie(ca)), http.StatusCreated)
	h.want(h.do(http.MethodPost, "/auth/passkey/register/finish", b.create(ob), withCookie(cb)), http.StatusUnauthorized)
	if n := h.count(`SELECT count(*) FROM passkeys`); n != 1 {
		t.Fatalf("%d passkeys from one token", n)
	}
}

func TestEnrollmentTokenExpired(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	e, err := h.svc.Enroll(t.Context(), "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// Begun in time, finished late: the token check at finish wins.
	c, opts := h.beginRegister(tokenOf(e))
	h.now = h.now.Add(2 * time.Minute)
	h.want(h.do(http.MethodPost, "/auth/passkey/register/finish", newSoftKey(t, origin).create(opts), withCookie(c)), http.StatusUnauthorized)
	// And begun late.
	r := h.do(http.MethodPost, "/auth/passkey/register/begin", map[string]string{"enrollment_token": tokenOf(e)})
	h.want(r, http.StatusUnauthorized)
	if h.count(`SELECT count(*) FROM passkeys`) != 0 {
		t.Fatal("expired token registered a passkey")
	}
}

func TestEnrollmentTokenTampered(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tok := tokenOf(h.enroll())
	last := tok[len(tok)-2]
	flip := byte('A')
	if last == 'A' {
		flip = 'B'
	}
	bad := tok[:len(tok)-2] + string(flip) + tok[len(tok)-1:]
	r := h.do(http.MethodPost, "/auth/passkey/register/begin", map[string]string{"enrollment_token": bad})
	h.want(r, http.StatusUnauthorized)
}

func TestCeremonyIsSingleUse(t *testing.T) {
	t.Parallel()
	h, key, _ := enrolled(t)
	r := h.do(http.MethodPost, "/auth/passkey/login/begin", nil)
	h.want(r, http.StatusOK)
	key.counter++
	answer := key.get(r.body)
	c := withCookie(r.cookies[ceremonyCookie])
	h.want(h.do(http.MethodPost, "/auth/passkey/login/finish", answer, c), http.StatusOK)
	h.want(h.do(http.MethodPost, "/auth/passkey/login/finish", answer, c), http.StatusBadRequest)
}

func TestLoginRejectsStaleCounter(t *testing.T) {
	t.Parallel()
	h, key, _ := enrolled(t)
	key.counter = 0 // a clone that missed the last use; login() sends 1 again
	h.want(h.login(key), http.StatusUnauthorized)
	if h.count(`SELECT count(*) FROM actions WHERE tool = 'auth.login.reject'`) != 1 {
		t.Error("clone warning not recorded")
	}
}

func TestLoginWithUnknownPasskey(t *testing.T) {
	t.Parallel()
	h, key, _ := enrolled(t)
	stranger := newSoftKey(t, origin)
	stranger.user = key.user
	h.want(h.login(stranger), http.StatusUnauthorized)
	wrongOrigin := newSoftKey(t, "https://evil.example.net")
	wrongOrigin.priv, wrongOrigin.id, wrongOrigin.user, wrongOrigin.counter = key.priv, key.id, key.user, key.counter+5
	h.want(h.login(wrongOrigin), http.StatusUnauthorized)
}

func TestRecoveryCode(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	e := h.enroll()
	if len(e.RecoveryCodes) != RecoveryCodeCount {
		t.Fatalf("%d codes", len(e.RecoveryCodes))
	}
	// Typed sloppily: lower case, no dashes.
	typed := strings.ToLower(strings.ReplaceAll(e.RecoveryCodes[0], "-", ""))
	r := h.do(http.MethodPost, "/auth/recovery", map[string]string{"code": typed})
	h.want(r, http.StatusOK)
	if len(r.cookies) != 0 {
		t.Fatalf("recovery set cookies: %v", r.cookies)
	}
	var out struct {
		Token string `json:"enrollment_token"`
	}
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatal(err)
	}
	// The token is not a credential of any kind.
	h.want(h.do(http.MethodGet, "/auth/session", nil, withHeader("Authorization", "Bearer "+out.Token)), http.StatusUnauthorized)
	h.want(h.do(http.MethodGet, "/auth/session", nil, withCookie(&http.Cookie{Name: SessionCookie, Value: out.Token})), http.StatusUnauthorized)

	// Burned: the same code again fails.
	h.want(h.do(http.MethodPost, "/auth/recovery", map[string]string{"code": e.RecoveryCodes[0]}), http.StatusUnauthorized)

	// It buys exactly one passkey registration, and still no session.
	reg := h.register(newSoftKey(t, origin), out.Token)
	h.want(reg, http.StatusCreated)
	if _, ok := reg.cookies[SessionCookie]; ok {
		t.Fatal("registration via recovery set a session")
	}
	if h.count(`SELECT count(*) FROM credentials`) != 0 {
		t.Fatal("recovery led to a credential")
	}
	h.want(h.do(http.MethodPost, "/auth/passkey/register/begin", map[string]string{"enrollment_token": out.Token}), http.StatusUnauthorized)

	if h.count(`SELECT count(*) FROM recovery_codes WHERE used_at IS NULL`) != RecoveryCodeCount-1 {
		t.Error("code not burned")
	}
	if h.count(`SELECT count(*) FROM actions WHERE tool = 'auth.recovery.use' AND actor IS NOT NULL`) != 1 ||
		h.count(`SELECT count(*) FROM actions WHERE tool = 'auth.recovery.reject'`) != 1 {
		t.Error("recovery actions not recorded")
	}
	if got := h.alerts.got(); len(got) != 2 || !strings.Contains(got[0], "recovery code was used") {
		t.Errorf("alerts %q", got)
	}
}

func TestRecoveryCodesRotate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	old := h.enroll()
	h.enroll()
	h.want(h.do(http.MethodPost, "/auth/recovery", map[string]string{"code": old.RecoveryCodes[1]}), http.StatusUnauthorized)
	if n := h.count(`SELECT count(*) FROM recovery_codes`); n != RecoveryCodeCount {
		t.Fatalf("%d codes after rotation", n)
	}
	if n := h.count(`SELECT count(*) FROM principals WHERE kind = 'human'`); n != 1 {
		t.Fatalf("%d humans after enrolling twice", n)
	}
}

func TestRecoveryRateLimit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	e := h.enroll()
	for range 3 {
		h.want(h.do(http.MethodPost, "/auth/recovery", map[string]string{"code": "0000-0000-0000-0000"}), http.StatusUnauthorized)
	}
	// Even a good code waits once the bucket is empty.
	h.want(h.do(http.MethodPost, "/auth/recovery", map[string]string{"code": e.RecoveryCodes[0]}), http.StatusTooManyRequests)
	h.now = h.now.Add(5 * time.Minute)
	h.want(h.do(http.MethodPost, "/auth/recovery", map[string]string{"code": e.RecoveryCodes[0]}), http.StatusOK)
}

// from makes a request arrive from peer, carrying xff if not empty.
func from(peer, xff string) func(*http.Request) {
	return func(r *http.Request) {
		r.RemoteAddr = peer
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
	}
}

// Behind a configured proxy each client has its own login bucket, so one
// client hammering the login cannot lock the operator out, and the audit
// log names the client rather than the proxy. A peer that is not a
// configured proxy cannot pick its bucket by writing the header.
func TestClientAddressBehindTrustedProxy(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var err error
	if h.svc.proxies, err = clientip.ParseProxies("127.0.0.1/32, ::1/128"); err != nil {
		t.Fatal(err)
	}
	const proxy, attacker, operator = "127.0.0.1:40000", "203.0.113.66", "198.51.100.1"
	for range 20 {
		h.want(h.do(http.MethodPost, "/auth/passkey/login/begin", nil, from(proxy, attacker)), http.StatusOK)
	}
	h.want(h.do(http.MethodPost, "/auth/passkey/login/begin", nil, from(proxy, attacker)), http.StatusTooManyRequests)
	h.want(h.do(http.MethodPost, "/auth/passkey/login/begin", nil, from(proxy, operator)), http.StatusOK)
	// The client may prepend anything; only what the proxy appended counts.
	h.want(h.do(http.MethodPost, "/auth/passkey/login/begin", nil, from(proxy, operator+", "+attacker)), http.StatusTooManyRequests)

	for i := range 20 {
		h.want(h.do(http.MethodPost, "/auth/passkey/login/begin", nil, from("192.0.2.9:1", fmt.Sprintf("192.0.2.%d", 100+i))), http.StatusOK)
	}
	h.want(h.do(http.MethodPost, "/auth/passkey/login/begin", nil, from("192.0.2.9:1", operator)), http.StatusTooManyRequests)

	h.want(h.do(http.MethodPost, "/auth/recovery", map[string]string{"code": "0000-0000-0000-0000"}, from("[::1]:40000", "2001:db8::66")), http.StatusUnauthorized)
	if n := h.count(`SELECT count(*) FROM actions WHERE tool = 'auth.recovery.reject' AND args->>'remote' = '2001:db8::66'`); n != 1 {
		t.Errorf("audit rows naming the client: %d", n)
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	t.Parallel()
	h, _, session := enrolled(t)
	r := h.do(http.MethodPost, "/auth/logout", nil, withCookie(session))
	h.want(r, http.StatusNoContent)
	if c := r.cookies[SessionCookie]; c == nil || c.MaxAge >= 0 {
		t.Fatalf("logout did not clear the cookie: %+v", c)
	}
	h.want(h.do(http.MethodGet, "/auth/session", nil, withCookie(session)), http.StatusUnauthorized)
	if h.count(`SELECT count(*) FROM actions WHERE tool = 'auth.reject' AND args->>'reason' = 'revoked'`) != 1 {
		t.Error("use of a revoked session not recorded")
	}
	// Rejections of one credential are recorded at most every 10 minutes.
	h.want(h.do(http.MethodGet, "/auth/session", nil, withCookie(session)), http.StatusUnauthorized)
	if h.count(`SELECT count(*) FROM actions WHERE tool = 'auth.reject'`) != 1 {
		t.Error("repeated rejections flood the audit log")
	}
}

func TestSessionExpires(t *testing.T) {
	t.Parallel()
	h, _, session := enrolled(t)
	h.now = h.now.Add(SessionTTL)
	h.want(h.do(http.MethodGet, "/auth/session", nil, withCookie(session)), http.StatusUnauthorized)
	// The stale cookie does not get in the way of an enrollment link.
	h.beginRegister(tokenOf(h.enroll()), withCookie(session))
}

func TestSessionCrossOriginPostRefused(t *testing.T) {
	t.Parallel()
	h, _, session := enrolled(t)
	h.want(h.do(http.MethodPost, "/test/admin", nil, withCookie(session), withHeader("Sec-Fetch-Site", "cross-site")), http.StatusForbidden)
	h.want(h.do(http.MethodPost, "/test/admin", nil, withCookie(session), withHeader("Origin", "https://evil.example.net")), http.StatusForbidden)
	h.want(h.do(http.MethodPost, "/auth/logout", nil, withCookie(session), withHeader("Sec-Fetch-Site", "cross-site")), http.StatusForbidden)
	// Same origin passes, through a proxy that rewrote Host too.
	h.want(h.do(http.MethodPost, "/test/admin", nil, withCookie(session), withHeader("Origin", origin), withHeader("Sec-Fetch-Site", "same-origin")), http.StatusOK)
	h.want(h.do(http.MethodPost, "/test/admin", nil, withCookie(session), withHeader("Origin", origin)), http.StatusOK)
}

func TestBearerScopes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	agent, err := h.db.EnsurePrincipal(t.Context(), store.Principal{ID: "01M67SX66B38NKRKAYDXR834N3", Kind: "agent", Name: "arisu", DisplayName: "arisu", CreatedAt: h.now})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := h.svc.IssueToken(t.Context(), TokenRequest{PrincipalID: agent.ID, Label: "mcp", Scopes: []auth.Scope{auth.MessageRead}, IssuedBy: store.SystemPrincipalID})
	if err != nil {
		t.Fatal(err)
	}
	bearer := withHeader("Authorization", "Bearer "+tok)

	r := h.do(http.MethodGet, "/test/read", nil, bearer)
	h.want(r, http.StatusOK)
	if string(r.body) != "agent:arisu" {
		t.Fatalf("viewer %q", r.body)
	}
	h.want(h.do(http.MethodPost, "/test/admin", nil, bearer), http.StatusForbidden)
	if h.count(`SELECT count(*) FROM actions WHERE tool = 'auth.deny' AND actor = $1 AND args->'missing' ? 'admin'`, agent.ID) != 1 {
		t.Error("denial not recorded")
	}
	if h.count(`SELECT count(*) FROM credentials WHERE kind = 'token' AND last_used_at IS NOT NULL`) != 1 {
		t.Error("last_used_at not recorded")
	}

	// A token is never accepted as a session cookie, and only Bearer works.
	h.want(h.do(http.MethodGet, "/test/read", nil, withCookie(&http.Cookie{Name: SessionCookie, Value: tok})), http.StatusUnauthorized)
	h.want(h.do(http.MethodGet, "/test/read", nil, withHeader("Authorization", "Basic "+tok)), http.StatusUnauthorized)
	h.want(h.do(http.MethodGet, "/test/read", nil), http.StatusUnauthorized)
	// Nor may it add a passkey, or log out.
	h.want(h.do(http.MethodPost, "/auth/passkey/register/begin", map[string]string{}, bearer), http.StatusForbidden)
	h.want(h.do(http.MethodPost, "/auth/logout", nil, bearer), http.StatusForbidden)

	// Revoked: rejected from then on.
	id, _, _ := auth.ParseSecret(auth.PrefixToken, tok)
	if err := h.svc.Revoke(t.Context(), &auth.Viewer{PrincipalID: store.SystemPrincipalID}, id); err != nil {
		t.Fatal(err)
	}
	h.want(h.do(http.MethodGet, "/test/read", nil, bearer), http.StatusUnauthorized)
}

// Bearer authenticates tokens without a WebUI origin and never accepts a
// session secret in their place.
func TestBearer(t *testing.T) {
	t.Parallel()
	h, _, session := enrolled(t)
	agent, err := h.db.EnsurePrincipal(t.Context(), store.Principal{ID: "01M67SX66B38NKRKAYDXR834N4", Kind: "agent", Name: "arisu", DisplayName: "arisu", CreatedAt: h.now})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := h.svc.IssueToken(t.Context(), TokenRequest{PrincipalID: agent.ID, Scopes: []auth.Scope{auth.MessageRead}, IssuedBy: store.SystemPrincipalID})
	if err != nil {
		t.Fatal(err)
	}
	b := NewBearer(h.db, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return h.now })
	v, err := b.Authenticate(t.Context(), tok, "test")
	if err != nil || v.String() != "agent:arisu" || !v.Has(auth.MessageRead) {
		t.Fatalf("token: %v %v", v, err)
	}
	for _, bad := range []string{"", "nonsense", session.Value} {
		if _, err := b.Authenticate(t.Context(), bad, "test"); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	id, _, _ := auth.ParseSecret(auth.PrefixToken, tok)
	if err := h.svc.Revoke(t.Context(), &auth.Viewer{PrincipalID: store.SystemPrincipalID}, id); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Authenticate(t.Context(), tok, "test"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("revoked: %v", err)
	}
	if h.count(`SELECT count(*) FROM actions WHERE tool = 'auth.reject' AND credential_id = $1 AND args->>'via' = 'test'`, id) != 1 {
		t.Error("rejection not recorded")
	}
}

func TestSessionAddsPasskey(t *testing.T) {
	t.Parallel()
	h, _, session := enrolled(t)
	r := h.do(http.MethodPost, "/auth/passkey/register/begin", map[string]string{"label": "laptop"}, withCookie(session))
	h.want(r, http.StatusOK)
	second := newSoftKey(t, origin)
	fin := h.do(http.MethodPost, "/auth/passkey/register/finish", second.create(r.body),
		withCookie(session), withCookie(r.cookies[ceremonyCookie]))
	h.want(fin, http.StatusCreated)
	h.want(h.login(second), http.StatusOK)
	if h.count(`SELECT count(*) FROM passkeys WHERE label = 'laptop'`) != 1 {
		t.Error("label not stored")
	}
}

func TestPostNeedsJSON(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.do(http.MethodPost, "/auth/recovery", []byte("code=x"), withHeader("Content-Type", "application/x-www-form-urlencoded"))
	h.want(r, http.StatusUnsupportedMediaType)
}

func TestParseOrigin(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"https://Mail.Example.com":     "https://mail.example.com",
		"https://mail.example.com:443": "https://mail.example.com:443",
		"https://mail.example.com/":    "https://mail.example.com",
		"http://localhost:31480":       "http://localhost:31480",
	} {
		got, _, err := ParseOrigin(in)
		if err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"http://mail.example.com", "https://mail.example.com/app", "https://192.0.2.1", "mail.example.com", "https://u@mail.example.com"} {
		if _, _, err := ParseOrigin(in); err == nil {
			t.Errorf("%s accepted", in)
		}
	}
	if _, err := New(Config{Origin: origin, RPID: "other.example.net"}); err == nil {
		t.Error("unrelated RP ID accepted")
	}
	if _, err := New(Config{Origin: origin, RPID: "example.com"}); err != nil {
		t.Errorf("parent-domain RP ID: %v", err)
	}
}

// An SMTP password authenticates the submission port only: user name =
// its credential id, scope exactly submission:send; it is no bearer
// token, and a token is no SMTP password.
func TestSMTPPassword(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	svc, err := h.db.EnsurePrincipal(t.Context(), store.Principal{ID: "01M67SX66B38NKRKAYDXR834N8", Kind: "service", Name: "monitoring", DisplayName: "monitoring", CreatedAt: h.now})
	if err != nil {
		t.Fatal(err)
	}
	send := []auth.Scope{auth.SubmissionSend}
	for name, scopes := range map[string][]auth.Scope{
		"none":  nil,
		"read":  {auth.MessageRead},
		"extra": {auth.SubmissionSend, auth.MessageRead},
	} {
		if _, err := h.svc.IssueToken(t.Context(), TokenRequest{Kind: auth.KindSMTPPassword, PrincipalID: svc.ID, Scopes: scopes}); err == nil {
			t.Errorf("scopes %s: issued", name)
		}
	}
	if _, err := h.svc.IssueToken(t.Context(), TokenRequest{Kind: auth.KindSession, PrincipalID: svc.ID, Scopes: send}); err == nil {
		t.Error("issued a session")
	}
	pw, err := h.svc.IssueToken(t.Context(), TokenRequest{Kind: auth.KindSMTPPassword, PrincipalID: svc.ID, Scopes: send, IssuedBy: store.SystemPrincipalID})
	if err != nil {
		t.Fatal(err)
	}
	user, _, ok := auth.ParseSecret(auth.PrefixSMTP, pw)
	if !ok || h.count(`SELECT count(*) FROM credentials WHERE id = $1 AND kind = 'smtp_password'`, user) != 1 ||
		h.count(`SELECT count(*) FROM actions WHERE tool = 'auth.smtp_password.issue'`) != 1 {
		t.Fatalf("issued %q", pw)
	}
	tok, err := h.svc.IssueToken(t.Context(), TokenRequest{PrincipalID: svc.ID, Scopes: send, IssuedBy: store.SystemPrincipalID})
	if err != nil {
		t.Fatal(err)
	}
	tokID, _, _ := auth.ParseSecret(auth.PrefixToken, tok)

	b := NewBearer(h.db, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return h.now })
	v, err := b.AuthenticateSMTP(t.Context(), user, pw, "smtp")
	if err != nil || v.String() != "service:monitoring" || v.CredentialKind != auth.KindSMTPPassword || !v.Has(auth.SubmissionSend) {
		t.Fatalf("smtp: %+v %v", v, err)
	}
	for name, c := range map[string][2]string{
		"other user name": {tokID, pw},
		"empty user name": {"", pw},
		"a token":         {tokID, tok},
		"wrong secret":    {user, pw[:len(pw)-2] + "AA"},
	} {
		if _, err := b.AuthenticateSMTP(t.Context(), c[0], c[1], "smtp"); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := b.Authenticate(t.Context(), pw, "test"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("SMTP password as a bearer token: %v", err)
	}
	if err := RevokeToken(t.Context(), h.db, user, store.Action{ID: ulid.New(h.now), Actor: store.SystemPrincipalID, At: h.now}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AuthenticateSMTP(t.Context(), user, pw, "smtp"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("revoked: %v", err)
	}
}

// Tokens go to agents and services only, with the scopes named; and the
// token revocation of the CLI revokes tokens only.
func TestIssueTokenRefuses(t *testing.T) {
	t.Parallel()
	h, _, session := enrolled(t)
	humans, err := h.db.PrincipalsOfKind(t.Context(), "human")
	if err != nil || len(humans) != 1 {
		t.Fatalf("humans %v %v", humans, err)
	}
	agent, err := h.db.EnsurePrincipal(t.Context(), store.Principal{ID: "01M67SX66B38NKRKAYDXR834N5", Kind: "agent", Name: "arisu", DisplayName: "arisu", CreatedAt: h.now})
	if err != nil {
		t.Fatal(err)
	}
	read := []auth.Scope{auth.MessageRead}
	for name, req := range map[string]TokenRequest{
		"human":         {PrincipalID: humans[0].ID, Scopes: read},
		"system":        {PrincipalID: store.SystemPrincipalID, Scopes: read},
		"no scopes":     {PrincipalID: agent.ID},
		"unknown scope": {PrincipalID: agent.ID, Scopes: []auth.Scope{"message:everything"}},
		"negative ttl":  {PrincipalID: agent.ID, Scopes: read, TTL: -time.Hour},
		"no principal":  {PrincipalID: "01M67SX66B38NKRKAYDXR834N6", Scopes: read},
	} {
		if _, err := h.svc.IssueToken(t.Context(), req); err == nil {
			t.Errorf("%s: issued", name)
		}
	}
	if n := h.count(`SELECT count(*) FROM credentials WHERE kind = 'token'`); n != 0 {
		t.Errorf("%d tokens issued", n)
	}
	id, _, _ := auth.ParseSecret(auth.PrefixSession, session.Value)
	if err := RevokeToken(t.Context(), h.db, id, store.Action{ID: ulid.New(h.now), Actor: store.SystemPrincipalID, At: h.now}); !errors.Is(err, ErrNotToken) {
		t.Error("revoked a session as a token")
	}
	if err := RevokeToken(t.Context(), h.db, "01M67SX66B38NKRKAYDXR834N7", store.Action{ID: ulid.New(h.now), Actor: store.SystemPrincipalID, At: h.now}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown token: %v", err)
	}
}
