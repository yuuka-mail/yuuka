package mailnet

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/clientip"
)

var (
	certOnce   sync.Once
	serverTLS  *tls.Config
	clientTLS  *tls.Config
	errCertGen error
)

// testTLS returns a server config with a throwaway certificate for
// mail.example.com and a client config that trusts it.
func testTLS(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	certOnce.Do(func() {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			errCertGen = err
			return
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(1),
			Subject:      pkix.Name{CommonName: "mail.example.com"},
			DNSNames:     []string{"mail.example.com"},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			errCertGen = err
			return
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			errCertGen = err
			return
		}
		pool := x509.NewCertPool()
		pool.AddCert(cert)
		serverTLS = &tls.Config{
			Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
			MinVersion:   tls.VersionTLS12,
		}
		clientTLS = &tls.Config{RootCAs: pool, ServerName: "mail.example.com", MinVersion: tls.VersionTLS12}
	})
	if errCertGen != nil {
		t.Fatal(errCertGen)
	}
	return serverTLS, clientTLS
}

func proxies(t *testing.T, s string) clientip.Proxies {
	t.Helper()
	p, err := clientip.ParseProxies(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func listenRaw(t *testing.T, network, addr string) (net.Listener, error) {
	t.Helper()
	var lc net.ListenConfig
	return lc.Listen(t.Context(), network, addr)
}

// harness runs a Listener on loopback, collecting what it accepts and
// what it rejects.
type harness struct {
	l        *Listener
	accepted chan net.Conn
	rejected chan error

	mu    sync.Mutex
	conns []net.Conn // server side, closed at cleanup
}

func listen(t *testing.T, network, addr string, cfg Config) *harness {
	t.Helper()
	inner, err := listenRaw(t, network, addr)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{accepted: make(chan net.Conn, 4), rejected: make(chan error, 4)}
	cfg.Rejected = func(_ net.Addr, err error) { h.rejected <- err }
	h.l, err = NewListener(inner, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = h.l.Close()
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, c := range h.conns {
			_ = c.Close()
		}
	})
	go func() {
		for {
			c, err := h.l.Accept()
			if err != nil {
				return
			}
			h.mu.Lock()
			h.conns = append(h.conns, c)
			h.mu.Unlock()
			h.accepted <- c
		}
	}()
	return h
}

func (h *harness) dial(t *testing.T) net.Conn {
	t.Helper()
	d := net.Dialer{Timeout: time.Second}
	c, err := d.DialContext(t.Context(), h.l.Addr().Network(), h.l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func (h *harness) wait(t *testing.T, d time.Duration) (net.Conn, error) {
	t.Helper()
	select {
	case c := <-h.accepted:
		return c, nil
	case err := <-h.rejected:
		return nil, err
	case <-time.After(d):
		t.Fatalf("neither accepted nor rejected within %v", d)
		return nil, nil
	}
}

var (
	v1Header      = []byte("PROXY TCP4 203.0.113.7 192.0.2.1 4242 465\r\n")
	v2Header      = v2(0x21, 0x21, inet6Body("2001:db8::7", "2001:db8::1", 4242, 465))
	v2LocalHeader = v2(0x20, 0x00, nil)
	v1Unknown     = []byte("PROXY UNKNOWN\r\n")
)

// TestCombinations is the table in §13.6: who terminates TLS × whether
// the peer is a trusted proxy × what it sends first.
func TestCombinations(t *testing.T) {
	const peer = "127.0.0.1" // every test connection comes from here
	tests := []struct {
		name    string
		tls     bool   // yuuka terminates TLS
		trusted bool   // the peer is a configured proxy
		header  []byte // sent first; nil for none
		// client is what follows the header: "tls" does a handshake,
		// "plain" sends a command, "silent" waits for a greeting.
		client string
		err    error  // why it is rejected; nil if accepted
		remote string // the client address yuuka should see
	}{
		{name: "tls/direct/no header", tls: true, client: "tls", remote: peer},
		{name: "tls/direct/v1 header", tls: true, header: v1Header, client: "tls", err: ErrUntrustedHeader},
		{name: "tls/direct/v2 header", tls: true, header: v2Header, client: "tls", err: ErrUntrustedHeader},
		{name: "tls/direct/plaintext command", tls: true, client: "plain", err: ErrPlaintext},
		{name: "tls/direct/plaintext silent", tls: true, client: "silent", err: os.ErrDeadlineExceeded},
		{name: "tls/proxy/no header", tls: true, trusted: true, client: "tls", err: ErrMissingHeader},
		{name: "tls/proxy/v1", tls: true, trusted: true, header: v1Header, client: "tls", remote: "203.0.113.7"},
		{name: "tls/proxy/v2 tcp6", tls: true, trusted: true, header: v2Header, client: "tls", remote: "2001:db8::7"},
		{name: "tls/proxy/v2 local", tls: true, trusted: true, header: v2LocalHeader, client: "tls", remote: peer},
		{name: "tls/proxy/v1 then plaintext", tls: true, trusted: true, header: v1Header, client: "plain", err: errTLSHandshake},

		{name: "plain/direct/no header", client: "silent", err: ErrPlaintext},
		{name: "plain/direct/v1 header", header: v1Header, client: "plain", err: ErrPlaintext},
		{name: "plain/direct/tls", client: "tls", err: ErrPlaintext},
		{name: "plain/proxy/no header", trusted: true, client: "silent", err: os.ErrDeadlineExceeded},
		{name: "plain/proxy/no header, command", trusted: true, client: "plain", err: ErrMissingHeader},
		{name: "plain/proxy/v1", trusted: true, header: v1Header, client: "plain", remote: "203.0.113.7"},
		{name: "plain/proxy/v2 tcp6", trusted: true, header: v2Header, client: "plain", remote: "2001:db8::7"},
		{name: "plain/proxy/v2 local", trusted: true, header: v2LocalHeader, client: "plain", remote: peer},
		{name: "plain/proxy/v1 unknown", trusted: true, header: v1Unknown, client: "plain", remote: peer},
		{name: "plain/proxy/malformed", trusted: true, header: []byte("PROXY TCP4 1.2.3.4 5.6.7.8 01 2\r\n"), client: "plain", err: errMalformed},
	}
	srvTLS, cliTLS := testTLS(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{HandshakeTimeout: 300 * time.Millisecond, Proxies: proxies(t, "192.0.2.1")}
			if tt.trusted {
				cfg.Proxies = proxies(t, "127.0.0.0/8")
			}
			if tt.tls {
				cfg.TLS = srvTLS
			}
			h := listen(t, "tcp", "127.0.0.1:0", cfg)
			raw := h.dial(t)

			// The client runs on its own: on rejection it is cut off, and
			// in the accepted case it talks to the server below.
			clientDone := make(chan string, 1)
			go func() {
				c := raw
				first := tt.header
				switch tt.client {
				case "plain":
					// Same write as the header: what follows it must survive.
					first = append(append([]byte{}, first...), "EHLO client.example.com\r\n"...)
				}
				if len(first) > 0 {
					if _, err := raw.Write(first); err != nil {
						clientDone <- "write: " + err.Error()
						return
					}
				}
				if tt.client == "tls" {
					tc := tls.Client(raw, cliTLS)
					if _, err := tc.Write([]byte("EHLO client.example.com\r\n")); err != nil {
						clientDone <- "tls: " + err.Error()
						return
					}
					c = tc
				}
				line, err := bufio.NewReader(c).ReadString('\n')
				if err != nil {
					clientDone <- "read: " + err.Error()
					return
				}
				clientDone <- line
			}()

			got, err := h.wait(t, 3*time.Second)
			if tt.err != nil {
				if !rejectedWith(err, tt.err) {
					t.Fatalf("got conn %v, err %v; want rejected with %v", got, err, tt.err)
				}
				// The client must see the connection end, not a greeting.
				select {
				case line := <-clientDone:
					if strings.HasPrefix(line, "220") {
						t.Errorf("rejected client got %q", line)
					}
				case <-time.After(3 * time.Second):
					t.Error("rejected client still connected")
				}
				return
			}
			if err != nil {
				t.Fatalf("rejected: %v", err)
			}
			if _, isTLS := got.(*tls.Conn); isTLS != tt.tls {
				t.Errorf("conn is %T, want TLS %v", got, tt.tls)
			}
			if host, _, _ := net.SplitHostPort(got.RemoteAddr().String()); host != tt.remote {
				t.Errorf("RemoteAddr = %v, want host %v", got.RemoteAddr(), tt.remote)
			}
			proxy := Proxy(got)
			if tt.trusted != (proxy != nil) {
				t.Errorf("Proxy = %v, want one: %v", proxy, tt.trusted)
			}
			if proxy != nil && proxy.String() != raw.LocalAddr().String() {
				t.Errorf("Proxy = %v, want the TCP peer %v", proxy, raw.LocalAddr())
			}
			line, err := bufio.NewReader(got).ReadString('\n')
			if err != nil || line != "EHLO client.example.com\r\n" {
				t.Fatalf("server read %q, %v", line, err)
			}
			if _, err := io.WriteString(got, "220 mail.example.com\r\n"); err != nil {
				t.Fatal(err)
			}
			if line := <-clientDone; line != "220 mail.example.com\r\n" {
				t.Errorf("client read %q", line)
			}
		})
	}
}

// errTLSHandshake stands for any tls.RecordHeaderError in the table.
var errTLSHandshake = errors.New("tls record header error")

func rejectedWith(err, want error) bool {
	if errors.Is(want, errTLSHandshake) {
		var rh tls.RecordHeaderError
		return errors.As(err, &rh)
	}
	return errors.Is(err, want)
}

func TestNewListenerRefusesPlaintextWithoutProxies(t *testing.T) {
	inner, err := listenRaw(t, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	if _, err := NewListener(inner, Config{}); err == nil {
		t.Fatal("NewListener accepted plaintext with no trusted proxy")
	}
}

// A peer that sends half a header and stalls holds only its own
// goroutine, until the deadline; others are accepted meanwhile. Half a
// v2 signature, because half a v1 line is refused at once.
func TestSlowHeaderDoesNotBlockAccept(t *testing.T) {
	const timeout = time.Second
	h := listen(t, "tcp", "127.0.0.1:0", Config{HandshakeTimeout: timeout, Proxies: proxies(t, "127.0.0.1")})
	start := time.Now()
	slow := h.dial(t)
	if _, err := slow.Write(v2Sig[:6]); err != nil {
		t.Fatal(err)
	}
	fast := h.dial(t)
	if _, err := fast.Write(v1Header); err != nil {
		t.Fatal(err)
	}
	c, err := h.wait(t, timeout/2)
	if err != nil {
		t.Fatalf("fast client rejected: %v", err)
	}
	if host, _, _ := net.SplitHostPort(c.RemoteAddr().String()); host != "203.0.113.7" {
		t.Errorf("accepted %v, want the fast client", c.RemoteAddr())
	}
	_, err = h.wait(t, 3*timeout)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("slow client: %v, want a deadline error", err)
	}
	if d := time.Since(start); d < timeout {
		t.Errorf("slow client cut off after %v, before the %v timeout", d, timeout)
	}
	_ = slow.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := slow.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !isReset(err) {
		t.Errorf("slow client read: %v, want the connection closed", err)
	}
}

func isReset(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && strings.Contains(op.Err.Error(), "reset")
}

// Close cuts off handshakes in progress instead of waiting for their
// deadline, and does not report them as rejected.
func TestCloseAbortsHandshakes(t *testing.T) {
	h := listen(t, "tcp", "127.0.0.1:0", Config{HandshakeTimeout: time.Minute, Proxies: proxies(t, "127.0.0.1")})
	slow := h.dial(t)
	if _, err := slow.Write(v2Sig[:6]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // let the listener start reading
	if err := h.l.Close(); err != nil {
		t.Fatal(err)
	}
	_ = slow.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := slow.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !isReset(err) {
		t.Errorf("read after Close: %v, want the connection closed", err)
	}
	if _, err := h.l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Accept after Close: %v, want net.ErrClosed", err)
	}
	select {
	case err := <-h.rejected:
		t.Errorf("Close reported as a rejection: %v", err)
	default:
	}
}

// failingListener fails its first Accept, then blocks until closed.
type failingListener struct {
	net.Listener
	once sync.Once
	err  error
}

func (f *failingListener) Accept() (net.Conn, error) {
	var first bool
	f.once.Do(func() { first = true })
	if first {
		return nil, f.err
	}
	return f.Listener.Accept()
}

func TestAcceptPassesThroughErrors(t *testing.T) {
	inner, err := listenRaw(t, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("too many open files")
	l, err := NewListener(&failingListener{Listener: inner, err: want}, Config{TLS: &tls.Config{MinVersion: tls.VersionTLS12}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if _, err := l.Accept(); !errors.Is(err, want) {
		t.Fatalf("Accept: %v, want %v", err, want)
	}
}

// Trust is decided on IPv6 peers the same way.
func TestIPv6Peer(t *testing.T) {
	if ln, err := listenRaw(t, "tcp6", "[::1]:0"); err != nil {
		t.Skip("no IPv6 loopback:", err)
	} else {
		_ = ln.Close()
	}
	h := listen(t, "tcp6", "[::1]:0", Config{HandshakeTimeout: time.Second, Proxies: proxies(t, "::1")})
	c := h.dial(t)
	if _, err := c.Write(append(append([]byte{}, v1Header...), "EHLO\r\n"...)); err != nil {
		t.Fatal(err)
	}
	got, err := h.wait(t, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if host, _, _ := net.SplitHostPort(got.RemoteAddr().String()); host != "203.0.113.7" {
		t.Errorf("RemoteAddr = %v", got.RemoteAddr())
	}
	if p := Proxy(got); p == nil || !strings.HasPrefix(p.String(), "[::1]:") {
		t.Errorf("Proxy = %v, want [::1]", p)
	}
}

func TestProxyOfOtherConn(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { _, _ = a.Close(), b.Close() })
	if p := Proxy(a); p != nil {
		t.Errorf("Proxy of a plain conn = %v", p)
	}
	if p := Proxy(tls.Server(a, &tls.Config{MinVersion: tls.VersionTLS12})); p != nil {
		t.Errorf("Proxy of a foreign tls.Conn = %v", p)
	}
}
