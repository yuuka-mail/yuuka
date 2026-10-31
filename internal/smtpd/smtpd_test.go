package smtpd

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/smtp"
	"net/textproto"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authn"
	"github.com/yuuka-mail/yuuka/internal/clientip"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
)

const host = "mail.example.com"

var (
	certOnce  sync.Once
	serverTLS *tls.Config
	clientTLS *tls.Config
	certErr   error
)

// testTLS returns a server config with a throwaway certificate for host
// and a client config that trusts it.
func testTLS(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	certOnce.Do(func() {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			certErr = err
			return
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(1),
			Subject:      pkix.Name{CommonName: host},
			DNSNames:     []string{host},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			certErr = err
			return
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			certErr = err
			return
		}
		pool := x509.NewCertPool()
		pool.AddCert(cert)
		serverTLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
		clientTLS = &tls.Config{RootCAs: pool, ServerName: host, MinVersion: tls.VersionTLS12}
	})
	if certErr != nil {
		t.Fatal(certErr)
	}
	return serverTLS, clientTLS
}

// fakeCore stands in for the submission core.
type fakeCore struct {
	mu     sync.Mutex
	calls  []call
	answer func(raw []byte) (Result, error)
}

type call struct {
	principal string
	from      string
	rcpts     []string
	raw       string
}

func (f *fakeCore) SubmitRaw(_ context.Context, v *auth.Viewer, from string, rcpts []string, raw []byte) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{v.String(), from, rcpts, string(raw)})
	if f.answer == nil {
		return Result{Queued: true, OutboxID: "01OUTBOX"}, nil
	}
	return f.answer(raw)
}

func (f *fakeCore) last(t *testing.T) call {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("core not called")
	}
	return f.calls[len(f.calls)-1]
}

type harness struct {
	t      *testing.T
	db     *store.DB
	pool   *pgxpool.Pool
	core   *fakeCore
	addr   string
	user   string // credential id of service:monitoring's SMTP password
	pw     string
	client *tls.Config
}

type option func(*Config)

func newHarness(t *testing.T, opts ...option) *harness {
	t.Helper()
	db, pool := storetest.New(t)
	now := time.Now()
	p, err := db.EnsurePrincipal(t.Context(), store.Principal{ID: "01M67SX66B38NKRKAYDXR834N8", Kind: "service", Name: "monitoring", DisplayName: "monitoring", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	pw, err := authn.IssueToken(t.Context(), db, now, authn.TokenRequest{
		Kind: auth.KindSMTPPassword, PrincipalID: p.ID, Scopes: []auth.Scope{auth.SubmissionSend}, IssuedBy: store.SystemPrincipalID,
	})
	if err != nil {
		t.Fatal(err)
	}
	user, _, _ := auth.ParseSecret(auth.PrefixSMTP, pw)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, client := testTLS(t)
	h := &harness{t: t, db: db, pool: pool, core: &fakeCore{}, user: user, pw: pw, client: client}
	cfg := Config{Submitter: h.core, Auth: authn.NewBearer(db, quiet, nil), Audit: db, TLS: server, Log: quiet}
	for _, o := range opts {
		o(&cfg)
	}
	srv := New(cfg)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h.addr = ln.Addr().String()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return h
}

func withProxies(s string) option {
	return func(c *Config) {
		p, err := clientip.ParseProxies(s)
		if err != nil {
			panic(err)
		}
		c.Proxies = p
	}
}

func withLimits(l limits) option { return func(c *Config) { c.limits = l } }

func withoutTLS(c *Config) { c.TLS = nil }

// dial opens a TLS session, after a PROXY header if one is given.
func (h *harness) dial(proxyHeader string) *smtp.Client {
	h.t.Helper()
	raw, err := (&net.Dialer{}).DialContext(h.t.Context(), "tcp", h.addr)
	if err != nil {
		h.t.Fatal(err)
	}
	if proxyHeader != "" {
		if _, err := io.WriteString(raw, proxyHeader); err != nil {
			h.t.Fatal(err)
		}
	}
	c, err := smtp.NewClient(tls.Client(raw, h.client), host)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = c.Close() })
	return c
}

// login dials and authenticates as service:monitoring.
func (h *harness) login() *smtp.Client {
	h.t.Helper()
	c := h.dial("")
	if err := c.Auth(smtp.PlainAuth("", h.user, h.pw, host)); err != nil {
		h.t.Fatal(err)
	}
	return c
}

// send runs one transaction and returns the reply that ended it, which
// net/smtp's Data would swallow.
func send(c *smtp.Client, from string, rcpts []string, body string) (int, string) {
	if err := c.Mail(from); err != nil {
		return code(err)
	}
	for _, r := range rcpts {
		if err := c.Rcpt(r); err != nil {
			return code(err)
		}
	}
	id, err := c.Text.Cmd("DATA")
	if err != nil {
		return code(err)
	}
	c.Text.StartResponse(id)
	_, _, err = c.Text.ReadResponse(354)
	c.Text.EndResponse(id)
	if err != nil {
		return code(err)
	}
	w := c.Text.DotWriter()
	_, _ = io.WriteString(w, body)
	if err := w.Close(); err != nil {
		return code(err)
	}
	n, msg, err := c.Text.ReadResponse(250)
	if err != nil {
		return code(err)
	}
	return n, msg
}

func code(err error) (int, string) {
	if te, ok := errors.AsType[*textproto.Error](err); ok {
		return te.Code, te.Msg
	}
	return -1, err.Error()
}

func (h *harness) count(q string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.pool.QueryRow(h.t.Context(), q, args...).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

const message = "From: alerts@example.com\r\nTo: ops@example.net\r\nSubject: disk\r\n\r\nDisk almost full.\r\n"

// The core's answer decides the reply: anything it keeps is 250 with
// the id, a refusal is 550 5.7.1 with its reason, anything else 451.
func TestResultMapping(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	cases := []struct {
		name   string
		res    Result
		err    error
		code   int
		prefix string
	}{
		{"queued", Result{Queued: true, OutboxID: "01QUEUED"}, nil, 250, "2.0.0 Queued as 01QUEUED"},
		{"held", Result{OutboxID: "01HELD", HoldReason: "review"}, nil, 250, "2.0.0 Held for review as 01HELD: review"},
		{"draft", Result{DraftID: "01DRAFT", HoldReason: "daily limit"}, nil, 250, "2.0.0 Kept as draft 01DRAFT: daily limit"},
		{"refused", Result{}, fmt.Errorf("%w: From alerts@example.com: no grant\nfor service:monitoring", ErrRefused), 550, "5.7.1 refused: From alerts@example.com: no grant?for"},
		{"temporary", Result{}, errors.New("database is down"), 451, "4.3.0 Temporary failure"},
	}
	c := h.login()
	for _, tc := range cases {
		h.core.mu.Lock()
		h.core.answer = func([]byte) (Result, error) { return tc.res, tc.err }
		h.core.mu.Unlock()
		n, msg := send(c, "alerts@example.com", []string{"ops@example.net", "oncall@example.net"}, message)
		if n != tc.code || !strings.HasPrefix(msg, tc.prefix) {
			t.Errorf("%s: %d %q, want %d %q", tc.name, n, msg, tc.code, tc.prefix)
		}
		if strings.Contains(msg, "database") {
			t.Errorf("%s: internal error leaked: %q", tc.name, msg)
		}
	}
	got := h.core.last(t)
	if got.principal != "service:monitoring" || got.from != "alerts@example.com" ||
		!slices.Equal(got.rcpts, []string{"ops@example.net", "oncall@example.net"}) || got.raw != message {
		t.Errorf("core got %+v", got)
	}
	if n := h.count(`SELECT count(*) FROM actions WHERE tool = 'smtp.auth' AND credential_id = $1
		AND args->>'via' = 'smtp' AND args->>'remote' = '127.0.0.1' AND NOT args ? 'proxy'`, h.user); n != 1 {
		t.Errorf("smtp.auth entries: %d", n)
	}
	for _, tc := range cases {
		if n := h.count(`SELECT count(*) FROM actions WHERE tool = 'smtp.submit' AND credential_id = $1
			AND (result->>'code')::int = $2 AND args->>'mail_from' = 'alerts@example.com' AND (args->>'rcpts')::int = 2`, h.user, tc.code); n == 0 {
			t.Errorf("%s: no smtp.submit entry with code %d", tc.name, tc.code)
		}
	}
	if n := h.count(`SELECT count(*) FROM actions WHERE tool = 'smtp.submit' AND result->>'draft_id' = '01DRAFT' AND result->>'hold_reason' = 'daily limit'`); n != 1 {
		t.Errorf("draft entry: %d", n)
	}
	if n := h.count(`SELECT count(*) FROM actions WHERE args::text LIKE '%' || $1 || '%' OR result::text LIKE '%Disk almost%'`, h.pw); n != 0 {
		t.Errorf("%d entries hold the password or the body", n)
	}
}

// What EHLO offers: PLAIN only, no STARTTLS, no SMTPUTF8, the exit's size
// and recipient limits.
func TestExtensions(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c := h.dial("")
	if err := c.Hello("client.example.com"); err != nil {
		t.Fatal(err)
	}
	for ext, want := range map[string]string{"AUTH": "PLAIN", "SIZE": "26214400", "LIMITS": "RCPTMAX=50"} {
		if ok, got := c.Extension(ext); !ok || got != want {
			t.Errorf("%s: %v %q, want %q", ext, ok, got, want)
		}
	}
	for _, ext := range []string{"STARTTLS", "SMTPUTF8", "DSN"} {
		if ok, _ := c.Extension(ext); ok {
			t.Errorf("%s offered", ext)
		}
	}
}

// Only a valid SMTP password with submission:send gets in; refusals are
// recorded with the client's address and never with the password.
func TestAuth(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withLimits(limits{authFailures: 100}))

	c := h.dial("")
	if n, msg := send(c, "alerts@example.com", []string{"ops@example.net"}, message); n != 530 {
		t.Errorf("MAIL before AUTH: %d %s", n, msg)
	}

	tok, err := authn.IssueToken(t.Context(), h.db, time.Now(), authn.TokenRequest{
		PrincipalID: "01M67SX66B38NKRKAYDXR834N8", Scopes: []auth.Scope{auth.SubmissionSend}, IssuedBy: store.SystemPrincipalID,
	})
	if err != nil {
		t.Fatal(err)
	}
	tokID, _, _ := auth.ParseSecret(auth.PrefixToken, tok)
	for name, a := range map[string]smtp.Auth{
		"wrong secret":   smtp.PlainAuth("", h.user, h.pw[:len(h.pw)-2]+"AA", host),
		"other user":     smtp.PlainAuth("", tokID, h.pw, host),
		"bearer token":   smtp.PlainAuth("", tokID, tok, host),
		"authz identity": smtp.PlainAuth("service:other", h.user, h.pw, host),
		"swapped fields": smtp.PlainAuth("", h.pw, h.user, host),
	} {
		if n, msg := code(h.dial("").Auth(a)); n != 535 {
			t.Errorf("%s: %d %s", name, n, msg)
		}
	}
	if n := h.count(`SELECT count(*) FROM actions WHERE tool = 'smtp.auth.reject' AND args->>'remote' = '127.0.0.1'`); n != 5 {
		t.Errorf("reject entries: %d", n)
	}
	if n := h.count(`SELECT count(*) FROM actions WHERE args::text LIKE '%' || $1 || '%'`, h.pw[4:]); n != 0 {
		t.Errorf("%d entries hold the password", n)
	}

	// An SMTP password whose scope was changed behind the issuer's back.
	if _, err := h.pool.Exec(t.Context(), `UPDATE credentials SET scopes = '{message:read}' WHERE id = $1`, h.user); err != nil {
		t.Fatal(err)
	}
	if n, msg := code(h.dial("").Auth(smtp.PlainAuth("", h.user, h.pw, host))); n != 535 {
		t.Errorf("wrong scope: %d %s", n, msg)
	}
	if n := h.count(`SELECT count(*) FROM actions WHERE tool = 'smtp.auth.reject' AND credential_id = $1 AND args->>'reason' LIKE 'missing scope%'`, h.user); n != 1 {
		t.Errorf("missing-scope entries: %d", n)
	}
	if _, err := h.pool.Exec(t.Context(), `UPDATE credentials SET scopes = '{submission:send}' WHERE id = $1`, h.user); err != nil {
		t.Fatal(err)
	}

	// Revocation reaches a session that is already open at its next MAIL,
	// and new sessions at AUTH.
	open := h.login()
	if err := authn.RevokeToken(t.Context(), h.db, time.Now(), h.user, store.SystemPrincipalID, nil); err != nil {
		t.Fatal(err)
	}
	if n, msg := send(open, "alerts@example.com", []string{"ops@example.net"}, message); n != 421 {
		t.Errorf("MAIL after revocation: %d %s", n, msg)
	}
	if n, msg := code(h.dial("").Auth(smtp.PlainAuth("", h.user, h.pw, host))); n != 535 {
		t.Errorf("revoked: %d %s", n, msg)
	}
	h.core.mu.Lock()
	defer h.core.mu.Unlock()
	if len(h.core.calls) != 0 {
		t.Errorf("core called %d times", len(h.core.calls))
	}
}

// Past the allowed failures AUTH is refused unchecked, even with the
// right password, and the throttling is recorded once.
func TestAuthThrottle(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withLimits(limits{authFailures: 2, authFailureEvery: time.Hour}))
	bad := smtp.PlainAuth("", h.user, h.pw[:len(h.pw)-2]+"AA", host)
	for i := range 2 {
		if n, msg := code(h.dial("").Auth(bad)); n != 535 {
			t.Fatalf("failure %d: %d %s", i, n, msg)
		}
	}
	for range 2 {
		if n, msg := code(h.dial("").Auth(smtp.PlainAuth("", h.user, h.pw, host))); n != 454 {
			t.Errorf("throttled: %d %s", n, msg)
		}
	}
	if n := h.count(`SELECT count(*) FROM actions WHERE tool = 'smtp.auth.reject' AND args->>'reason' = 'throttled'`); n != 1 {
		t.Errorf("throttled entries: %d", n)
	}
	if n := h.count(`SELECT count(*) FROM actions WHERE tool = 'smtp.auth'`); n != 0 {
		t.Errorf("%d successful logins", n)
	}
}

// The size and recipient caps are the exit's, enforced before the core.
func TestMessageLimits(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c := h.login()

	if err := c.Text.PrintfLine("MAIL FROM:<alerts@example.com> SIZE=%d", MaxMessageBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, msg, err := c.Text.ReadResponse(250); err == nil || !strings.HasPrefix(msg, "5.3.4") {
		t.Errorf("declared SIZE over the cap: %v", err)
	}

	big := message + strings.Repeat(strings.Repeat("x", 998)+"\r\n", MaxMessageBytes/1000+1)
	if n, msg := send(c, "alerts@example.com", []string{"ops@example.net"}, big); n != 552 {
		t.Errorf("oversized DATA: %d %s", n, msg)
	}

	rcpts := make([]string, MaxRecipients+1)
	for i := range rcpts {
		rcpts[i] = fmt.Sprintf("r%d@example.net", i)
	}
	if n, msg := send(c, "alerts@example.com", rcpts, message); n != 452 || !strings.HasPrefix(msg, "4.5.3") {
		t.Errorf("recipient %d: %d %s", MaxRecipients+1, n, msg)
	}
	if err := c.Reset(); err != nil {
		t.Fatal(err)
	}
	if n, msg := send(c, "alerts@example.com", rcpts[:MaxRecipients], message); n != 250 {
		t.Errorf("%d recipients: %d %s", MaxRecipients, n, msg)
	}
	if got := h.core.last(t); len(got.rcpts) != MaxRecipients {
		t.Errorf("core got %d recipients", len(got.rcpts))
	}
	if n, msg := send(c, "alerts@example.com", []string{"büro@example.net"}, message); n != 553 || !strings.HasPrefix(msg, "5.6.7") {
		t.Errorf("non-ASCII recipient: %d %s", n, msg)
	}
	if n := h.count(`SELECT count(*) FROM actions WHERE tool = 'smtp.submit' AND (result->>'code')::int = 552`); n != 1 {
		t.Errorf("552 entries: %d", n)
	}
}

// Behind a TLS-terminating proxy the port is plaintext, AUTH is offered
// there, and the audit names the client from the PROXY header and the
// proxy it came through.
func TestBehindProxy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		opts []option
	}{
		{"yuuka terminates TLS", []option{withProxies("127.0.0.1/32")}},
		{"proxy terminates TLS", []option{withProxies("127.0.0.1/32"), withoutTLS}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, tc.opts...)
			var c *smtp.Client
			if len(tc.opts) == 1 {
				c = h.dial("PROXY TCP4 203.0.113.7 192.0.2.1 40000 465\r\n")
			} else {
				raw, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", h.addr)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := io.WriteString(raw, "PROXY TCP4 203.0.113.7 192.0.2.1 40000 465\r\n"); err != nil {
					t.Fatal(err)
				}
				// net/smtp sends PLAIN in the clear only to localhost.
				if c, err = smtp.NewClient(raw, "localhost"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = c.Close() })
			}
			name := host
			if len(tc.opts) > 1 {
				name = "localhost"
			}
			if err := c.Auth(smtp.PlainAuth("", h.user, h.pw, name)); err != nil {
				t.Fatal(err)
			}
			if n, msg := send(c, "alerts@example.com", []string{"ops@example.net"}, message); n != 250 {
				t.Fatalf("%d %s", n, msg)
			}
			for _, tool := range []string{"smtp.auth", "smtp.submit"} {
				if n := h.count(`SELECT count(*) FROM actions WHERE tool = $1 AND args->>'remote' = '203.0.113.7' AND args->>'proxy' = '127.0.0.1'`, tool); n != 1 {
					t.Errorf("%s entries from the real client: %d", tool, n)
				}
			}
		})
	}
}

// Connections are capped per client and in total; one over a cap gets
// 421 and a slot frees when a connection closes.
func TestConnectionCaps(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withProxies("127.0.0.1/32"), withLimits(limits{conns: 3, connsPerClient: 2}))
	from := func(ip string) string { return "PROXY TCP4 " + ip + " 192.0.2.1 40000 465\r\n" }
	greeting := func(ip string) (net.Conn, string) {
		raw, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", h.addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = raw.Close() })
		if _, err := io.WriteString(raw, from(ip)); err != nil {
			t.Fatal(err)
		}
		c := tls.Client(raw, h.client)
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, _ := bufio.NewReader(c).ReadString('\n')
		return c, line
	}
	a1, l1 := greeting("203.0.113.1")
	_, l2 := greeting("203.0.113.1")
	_, l3 := greeting("203.0.113.1")
	if !strings.HasPrefix(l1, "220 ") || !strings.HasPrefix(l2, "220 ") || !strings.HasPrefix(l3, "421 4.7.0") {
		t.Fatalf("per client: %q %q %q", l1, l2, l3)
	}
	_, l4 := greeting("203.0.113.2")
	_, l5 := greeting("203.0.113.3")
	if !strings.HasPrefix(l4, "220 ") || !strings.HasPrefix(l5, "421 4.7.0") {
		t.Fatalf("in total: %q %q", l4, l5)
	}
	_ = a1.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, l := greeting("203.0.113.3")
		if strings.HasPrefix(l, "220 ") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot not freed: %q", l)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestClientKey(t *testing.T) {
	t.Parallel()
	for in, want := range map[string][2]string{
		"192.0.2.1:25":                {"192.0.2.1", "192.0.2.1"},
		"[::ffff:192.0.2.1]:25":       {"192.0.2.1", "192.0.2.1"},
		"[2001:db8:1:2:3:4:5:6]:25":   {"2001:db8:1:2:3:4:5:6", "2001:db8:1:2::/64"},
		"[2001:db8:1:2:ffff::1]:4000": {"2001:db8:1:2:ffff::1", "2001:db8:1:2::/64"},
	} {
		a, err := net.ResolveTCPAddr("tcp", in)
		if err != nil {
			t.Fatal(err)
		}
		if addr, key := clientOf(a); addr != want[0] || key != want[1] {
			t.Errorf("%s: %s %s", in, addr, key)
		}
	}
}
