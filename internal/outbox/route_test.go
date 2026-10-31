package outbox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"log/slog"
	"math/big"
	"net"
	"net/textproto"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/transport"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Sending through transports configured in the database (§10.2.2, §17).

// routeEnv is env with a key, transports configured in the database and
// a fake behind each of them, by name.
type routeEnv struct {
	*env
	sealer *secret.Sealer

	mu     sync.Mutex
	fakes  map[string]*fakeTransport
	built  []transport.Config // every client built from a configured row
	alerts []string
	logs   lockedBuffer
}

// withCaps is a fake as built from a row: with the row's caps.
type withCaps struct {
	*fakeTransport
	caps transport.Caps
}

func (w withCaps) Caps() transport.Caps { return w.caps }

func (re *routeEnv) TransportSecretAlert(_ context.Context, name string, err error) {
	re.mu.Lock()
	defer re.mu.Unlock()
	re.alerts = append(re.alerts, name+": "+err.Error())
}

func newRouteEnv(t *testing.T, mut func(*Config)) *routeEnv {
	t.Helper()
	re := &routeEnv{sealer: testSealer(t), fakes: map[string]*fakeTransport{}}
	re.env = newEnv(t, func(c *Config) {
		c.Sealer = re.sealer
		c.Alerter = re
		c.Log = slog.New(slog.NewTextHandler(&re.logs, nil))
		c.NewTransport = func(tc transport.Config) (Transport, error) {
			re.mu.Lock()
			defer re.mu.Unlock()
			re.built = append(re.built, tc)
			f, ok := re.fakes[tc.Name]
			if !ok {
				return nil, errors.New("no fake for " + tc.Name)
			}
			return withCaps{f, tc.Caps}, nil
		}
		if mut != nil {
			mut(c)
		}
	})
	return re
}

func testSealer(t *testing.T) *secret.Sealer {
	t.Helper()
	kek := make([]byte, secret.KEKSize)
	_, _ = rand.Read(kek)
	key, err := secret.ParseKey([]byte(base64.StdEncoding.EncodeToString(kek)))
	if err != nil {
		t.Fatal(err)
	}
	ring, err := secret.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	s, err := secret.NewSealer(ring)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func action(tool string) store.Action {
	now := time.Now()
	return store.Action{ID: ulid.New(now), Actor: store.SystemPrincipalID, Tool: tool, At: now}
}

// configure configures transport name in the database as user@<name>.example.com:465
// with caps, its password sealed by sealer, and gives it a fake that
// accepts everything.
func (re *routeEnv) configure(name string, caps []string, password string, sealer *secret.Sealer) store.Transport {
	re.t.Helper()
	return re.configureAt(name, name+".example.com", 465, caps, password, sealer)
}

func (re *routeEnv) configureAt(name, host string, port int, caps []string, password string, sealer *secret.Sealer) store.Transport {
	re.t.Helper()
	ctx := re.t.Context()
	var rev int64
	if cur, err := re.db.TransportByName(ctx, name); err == nil {
		rev = cur.Revision
	}
	aud, err := secret.SMTPS("user", host, port)
	if err != nil {
		re.t.Fatal(err)
	}
	rec, err := sealer.Seal(ctx, ulid.New(time.Now()), store.SecretSMTPPassword, aud, []byte(password))
	if err != nil {
		re.t.Fatal(err)
	}
	c := store.TransportConfig{Name: name, Host: host, Port: port, Auth: store.TransportAuthPlain, Username: "user", Caps: caps}
	tr, err := re.db.ConfigureTransport(ctx, c, rev, &rec, action("transport.set"))
	if err != nil {
		re.t.Fatal(err)
	}
	re.mu.Lock()
	defer re.mu.Unlock()
	if re.fakes[name] == nil {
		f := &fakeTransport{}
		f.set(reply(nil, nil))
		re.fakes[name] = f
	}
	return tr
}

func (re *routeEnv) fake(name string) *fakeTransport {
	re.mu.Lock()
	defer re.mu.Unlock()
	return re.fakes[name]
}

// chain sets the identity's chain.
func (re *routeEnv) chain(names ...string) {
	re.t.Helper()
	if err := re.db.SetIdentityTransports(re.t.Context(), re.identity, names, action("identity.route")); err != nil {
		re.t.Fatal(err)
	}
}

func (e *env) transportID(name string) string {
	e.t.Helper()
	tr, err := e.db.TransportByName(e.t.Context(), name)
	if err != nil {
		e.t.Fatal(err)
	}
	return tr.ID
}

// via lists send_attempts.transport of the row's attempts, oldest first.
func (e *env) via(outboxID string) []string {
	e.t.Helper()
	rows, err := e.pool.Query(e.t.Context(), `SELECT transport FROM send_attempts WHERE outbox_id = $1 ORDER BY started_at, id`, outboxID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) take() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.b.String()
	l.b.Reset()
	return s
}

// A transport configured in the database sends with the password sealed
// for it, through a real SMTP session, and a second message reuses the
// client built for the first.
func TestDatabaseTransportSends(t *testing.T) {
	t.Parallel()
	srv := newAuthServer(t, "user", "s3cret")
	var builds int
	re := newRouteEnv(t, func(c *Config) {
		c.NewTransport = func(tc transport.Config) (Transport, error) {
			builds++
			tc.RootCAs = srv.pool
			return transport.NewSMTP(tc)
		}
	})
	re.configureAt("db", "127.0.0.1", srv.port, []string{"message-id"}, "s3cret", re.sealer)
	re.chain("db")
	q1 := re.enqueue("a@example.net")
	q2 := re.enqueue("b@example.net")
	for re.step() {
	}
	for _, q := range []Enqueued{q1, q2} {
		if r := re.row(q.OutboxID); r.state != store.OutboxSent || !slices.Equal(re.via(q.OutboxID), []string{"db"}) {
			t.Errorf("%s: %+v via %v", q.OutboxID, r, re.via(q.OutboxID))
		}
	}
	if got := srv.auths(); !slices.Equal(got, []string{"\x00user\x00s3cret", "\x00user\x00s3cret"}) {
		t.Errorf("AUTH %q", got)
	}
	if builds != 1 {
		t.Errorf("built %d clients for one revision", builds)
	}
}

// The secret opens only for the destination recomputed from the row. A
// database writer who moves the transport elsewhere gets a locked
// transport and a security alert, also when they rewrite the secret's
// stored audience to match: the audience is sealed into the payload.
func TestSecretOpensOnlyForItsDestination(t *testing.T) {
	t.Parallel()
	re := newRouteEnv(t, nil)
	re.configure("db", []string{}, "pw", re.sealer)
	re.chain("db")
	q := re.enqueue("a@example.net")
	if !re.step() {
		t.Fatal("not claimed")
	}
	if len(re.built) != 1 || re.built[0].Password != "pw" || re.built[0].Host != "db.example.com" || re.built[0].Username != "user" {
		t.Fatalf("built %+v", re.built)
	}
	if r := re.row(q.OutboxID); r.state != store.OutboxSent {
		t.Fatalf("row %+v", r)
	}
	ctx := t.Context()
	for _, c := range []struct {
		name, sql, why string
	}{
		{
			"host moved", `UPDATE transports SET host = 'evil.example.net', revision = revision + 1 WHERE name = 'db'`,
			"locked: its password was sealed for another destination; set it again",
		},
		{
			"audience moved with it", `UPDATE secrets SET audience = 'smtps://user@evil.example.net:465', revision = revision + 1
			WHERE id = (SELECT secret_id FROM transports WHERE name = 'db')`,
			"locked: its password failed authentication; set it again",
		},
	} {
		if _, err := re.pool.Exec(ctx, c.sql); err != nil {
			t.Fatal(err)
		}
		q := re.enqueue("a@example.net")
		if re.step() {
			t.Fatalf("%s: claimed", c.name)
		}
		re.svc.housekeep(ctx)
		if r := re.row(q.OutboxID); r.state != store.OutboxQueued || len(r.outcome) != 0 || r.err != "no usable transport: db: "+c.why {
			t.Errorf("%s: %+v", c.name, r)
		}
		// Re-resolving at the same revisions alerts no more.
		re.step()
	}
	if len(re.built) != 1 {
		t.Errorf("built %d clients", len(re.built))
	}
	if len(re.alerts) != 2 || !strings.Contains(re.alerts[0], "destination changed") || !strings.Contains(re.alerts[1], "tampered") {
		t.Errorf("alerts %q", re.alerts)
	}
}

// Without the key a configured transport is locked: its mail waits in the
// queue with the reason, is never failed or dead, and the executor logs an
// error at once and every ten minutes while it lasts. Enqueueing still
// works: caps are not secret.
func TestLockedTransportWaits(t *testing.T) {
	t.Parallel()
	re := newRouteEnv(t, func(c *Config) { c.Sealer = nil })
	re.configure("db", []string{}, "pw", testSealer(t))
	re.chain("db")
	q := re.enqueue("a@example.net")
	for range 3 {
		if re.step() {
			t.Fatal("claimed through a locked transport")
		}
		re.svc.housekeep(t.Context())
	}
	r := re.row(q.OutboxID)
	if r.state != store.OutboxQueued || len(r.outcome) != 0 || r.rcpts["a@example.net"] != "pending" ||
		r.err != "no usable transport: db: locked: no key to open its password (YUUKA_KEK_FILE)" {
		t.Errorf("row %+v", r)
	}
	logs := re.logs.take()
	if n := strings.Count(logs, "transports locked"); n != 1 || !strings.Contains(logs, "level=ERROR") ||
		!strings.Contains(logs, "wrapper local") {
		t.Errorf("%d reports in %s", n, logs)
	}
	re.clock.add(lockedReport)
	re.svc.housekeep(t.Context())
	if logs := re.logs.take(); !strings.Contains(logs, "transports locked") || !strings.Contains(logs, q.OutboxID) {
		t.Errorf("no report after ten minutes: %s", logs)
	}
	if len(re.built) != 0 {
		t.Errorf("built %+v", re.built)
	}
}

// The chain falls back to the next member when the primary is paused,
// disabled, locked or an unconfigured placeholder (§10.2.2), and the
// attempt records the transport it went through.
func TestFallback(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// primary turns the placeholder primary, already in the chain,
		// into what the case is about.
		primary func(re *routeEnv)
		// first: the primary is tried once before the fallback.
		first bool
	}{
		{"paused", func(re *routeEnv) {
			re.configure("primary", []string{}, "pw", re.sealer)
			re.fake("primary").set(func(context.Context, transport.Envelope, []byte, func(context.Context) error) (*transport.SendResult, error) {
				return &transport.SendResult{Outcome: transport.OutcomeRejected, Recipients: []transport.RecipientResult{{Addr: "a@example.net", Status: transport.StatusRejected, Code: 535}}},
					&transport.Error{Stage: transport.StageAuth, Err: &textproto.Error{Code: 535, Msg: "5.7.8 Authentication credentials invalid"}}
			})
		}, true},
		{"disabled", func(re *routeEnv) {
			tr := re.configure("primary", []string{}, "pw", re.sealer)
			if err := re.db.DisableTransport(re.t.Context(), "primary", tr.Revision, action("transport.rm")); err != nil {
				re.t.Fatal(err)
			}
		}, false},
		{"locked", func(re *routeEnv) {
			// Sealed under a key this process does not have.
			re.configure("primary", []string{}, "pw", testSealer(re.t))
		}, false},
		{"placeholder", func(*routeEnv) {}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			re := newRouteEnv(t, nil)
			if err := re.db.EnsureTransports(t.Context(), []string{"primary"}); err != nil {
				t.Fatal(err)
			}
			re.chain("primary", "cf")
			c.primary(re)
			q := re.enqueue("a@example.net")
			for re.step() {
			}
			want := []string{"cf"}
			if c.first {
				want = []string{"primary", "cf"}
			}
			if r := re.row(q.OutboxID); r.state != store.OutboxSent || !slices.Equal(re.via(q.OutboxID), want) {
				t.Errorf("row %+v via %v, want via %v", r, re.via(q.OutboxID), want)
			}
			if len(re.tr.sent()) != 1 {
				t.Errorf("fallback sent %d", len(re.tr.sent()))
			}
		})
	}
}

// Past the commit point there is no fallback: an unknown stays unknown
// for a person to resolve, whatever the rest of the chain could do.
func TestNoFallbackAfterTheCommitPoint(t *testing.T) {
	t.Parallel()
	re := newRouteEnv(t, nil)
	re.configure("primary", []string{}, "pw", re.sealer)
	re.chain("primary", "cf")
	re.fake("primary").set(reply(map[string]transport.RecipientResult{
		"a@example.net": {Status: transport.StatusUnknown, Text: "no final reply"},
	}, &transport.Error{Stage: transport.StageFinal, Err: errors.New("connection reset")}))
	q := re.enqueue("a@example.net")
	for re.step() {
	}
	re.svc.housekeep(t.Context())
	if r := re.row(q.OutboxID); r.state != store.OutboxUnknown || !slices.Equal(re.via(q.OutboxID), []string{"primary"}) {
		t.Errorf("row %+v via %v", r, re.via(q.OutboxID))
	}
	if len(re.tr.sent()) != 0 {
		t.Error("sent through the fallback after the commit point")
	}
}

// A new revision of the transport or its secret ends its pause and its
// cached client: a fixed password is tried at the next claim, without a
// restart and without waiting the pause out.
func TestNewRevisionEndsThePause(t *testing.T) {
	t.Parallel()
	re := newRouteEnv(t, func(c *Config) { c.PauseMin = time.Hour })
	tr := re.configure("db", []string{}, "old", re.sealer)
	re.chain("db")
	re.fake("db").set(func(context.Context, transport.Envelope, []byte, func(context.Context) error) (*transport.SendResult, error) {
		return &transport.SendResult{Outcome: transport.OutcomeRejected, Recipients: []transport.RecipientResult{{Addr: "a@example.net", Status: transport.StatusRejected, Code: 535}}},
			&transport.Error{Stage: transport.StageAuth, Err: &textproto.Error{Code: 535, Msg: "5.7.8 Authentication credentials invalid"}}
	})
	q := re.enqueue("a@example.net")
	re.step()
	re.due(q.OutboxID)
	if re.step() {
		t.Fatal("claimed through a paused transport")
	}

	// The password is set again: same destination, same secret row, a
	// new revision.
	ctx := t.Context()
	stored, err := re.db.Secret(ctx, tr.SecretID)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := re.sealer.Seal(ctx, stored.ID, stored.Kind, stored.Audience, []byte("new"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := re.db.UpdateSecret(ctx, rec, stored.Revision, action("secret.set")); err != nil {
		t.Fatal(err)
	}
	re.fake("db").set(reply(nil, nil))
	if !re.step() {
		t.Fatal("not claimed after the password changed")
	}
	if r := re.row(q.OutboxID); r.state != store.OutboxSent {
		t.Errorf("row %+v", r)
	}
	if len(re.built) != 2 || re.built[1].Password != "new" {
		t.Errorf("built %+v", re.built)
	}
	if _, ok := re.svc.pauses[tr.ID]; ok {
		t.Error("still paused")
	}
}

// Enqueue strips what any configured, enabled member of the chain does
// not allow, so a fallback never gets header fields it cannot carry;
// placeholders without configuration and disabled transports do not
// count, and a chain with nothing that counts cannot be enqueued.
func TestEnqueueStripsForTheWholeChain(t *testing.T) {
	t.Parallel()
	re := newRouteEnv(t, nil)
	re.configure("std", []string{"message-id", "return-path"}, "pw", re.sealer)
	if err := re.db.EnsureTransports(t.Context(), []string{"empty"}); err != nil {
		t.Fatal(err)
	}
	stored := func(q Enqueued) string {
		t.Helper()
		var key string
		if err := re.pool.QueryRow(t.Context(), `SELECT r2_key FROM raw_objects WHERE id = $1`, q.RawID).Scan(&key); err != nil {
			t.Fatal(err)
		}
		b, err := re.local.Get(key)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	check := func(messageID bool) {
		t.Helper()
		got := stored(re.enqueue("a@example.net"))
		if strings.Contains(got, "Message-ID:") != messageID || strings.Contains(got, "Return-Path:") {
			t.Errorf("stored %q", got)
		}
	}
	re.chain("std")
	check(true)
	re.chain("std", "empty")
	check(true)
	re.chain("empty", "std")
	check(true)
	re.chain("std", "cf") // cf: the environment's Cloudflare caps
	check(false)
	gone := re.configure("gone", []string{}, "pw", re.sealer)
	re.chain("std", "gone")
	check(false)
	if err := re.db.DisableTransport(t.Context(), "gone", gone.Revision, action("transport.rm")); err != nil {
		t.Fatal(err)
	}
	check(true)

	re.chain("empty")
	if _, err := re.submit(re.operator, "me@example.com", Submission{Raw: []byte(testRaw), Recipients: to("a@example.net")}); !errors.Is(err, ErrUnconfigured) {
		t.Errorf("chain of a placeholder: %v", err)
	}
}

// The chain's caps narrowed after a message was queued: the claim skips
// the members that would not take the stored bytes, and with none left
// the message waits, never rejected.
func TestClaimSkipsMembersThatCannotCarryTheBytes(t *testing.T) {
	t.Parallel()
	re := newRouteEnv(t, nil)
	re.configure("std", []string{"message-id"}, "pw", re.sealer)
	re.chain("std")
	q := re.enqueue("a@example.net") // keeps its Message-ID
	re.chain("cf", "std")            // cf strips it
	if !re.step() {
		t.Fatal("not claimed")
	}
	if r := re.row(q.OutboxID); r.state != store.OutboxSent || !slices.Equal(re.via(q.OutboxID), []string{"std"}) {
		t.Errorf("row %+v via %v", r, re.via(q.OutboxID))
	}
	if len(re.tr.sent()) != 0 {
		t.Error("sent through a transport that does not allow the bytes")
	}

	re.chain("std")
	q = re.enqueue("b@example.net")
	re.chain("cf")
	re.step()
	r := re.row(q.OutboxID)
	if r.state != store.OutboxQueued || !near(r.next, misfitRetry) || !slices.Equal(r.outcome, []string{"abandoned"}) ||
		!strings.Contains(r.err, "allows the header fields") || r.rcpts["b@example.net"] != "pending" {
		t.Errorf("row %+v", r)
	}
}

// authServer is an SMTP submission server with implicit TLS that takes
// one username and password and accepts every message.
type authServer struct {
	port int
	pool *x509.CertPool
	mu   sync.Mutex
	seen []string
}

func (s *authServer) auths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.seen)
}

func newAuthServer(t *testing.T, user, password string) *authServer {
	t.Helper()
	cert, pool := selfSigned(t)
	inner, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := tls.NewListener(inner, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	s := &authServer{port: inner.Addr().(*net.TCPAddr).Port, pool: pool}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() { s.serve(conn, "\x00"+user+"\x00"+password) })
		}
	})
	return s
}

func (s *authServer) serve(conn net.Conn, want string) {
	defer func() { _ = conn.Close() }()
	tp := textproto.NewConn(conn)
	_ = tp.PrintfLine("220 fake.example.com ESMTP")
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		reply := "250 2.0.0 Ok"
		switch strings.ToUpper(verb) {
		case "EHLO":
			reply = "250-fake.example.com\r\n250 AUTH PLAIN"
		case "AUTH":
			_, resp, _ := strings.Cut(arg, " ")
			dec, _ := base64.StdEncoding.DecodeString(resp)
			s.mu.Lock()
			s.seen = append(s.seen, string(dec))
			s.mu.Unlock()
			reply = "235 2.7.0 Authentication successful"
			if string(dec) != want {
				reply = "535 5.7.8 Authentication credentials invalid"
			}
		case "DATA":
			_ = tp.PrintfLine("354 End data with <CR><LF>.<CR><LF>")
			if !readDot(tp.R) {
				return
			}
			reply = "250 2.0.0 Ok: queued as ABC123"
		case "QUIT":
			_ = tp.PrintfLine("221 2.0.0 Bye")
			return
		}
		if tp.PrintfLine("%s", reply) != nil {
			return
		}
	}
}

func readDot(r *bufio.Reader) bool {
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return false
		}
		if line == ".\r\n" {
			return true
		}
	}
}

// selfSigned returns a certificate for 127.0.0.1 and a pool that trusts it.
func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "yuuka outbox test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}
