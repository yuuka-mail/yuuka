package imap

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"flag"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the client lines of testdata/transcripts")

// A transcript is an exact IMAP exchange, one line per protocol line:
//
//	S: <line>    the server sends <line> and CRLF
//	C: <line>    the client must send <line> and CRLF
//	C: <closed>  the client must close the connection
//	S: <hang>    the server sends nothing more and waits for the client
//	             to close
//
// Lines starting with # and blank lines are comments. A literal's octets
// are written as further S: lines; the CRLF ending each counts. With
// -update, the C: lines are rewritten from what the client sent.
type step struct {
	server bool
	text   string
	line   int // index into the file's lines, for -update
}

const (
	closed = "<closed>"
	hang   = "<hang>"
)

func loadTranscript(t *testing.T, path string) ([]string, []step) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	var steps []step
	for i, l := range lines {
		switch {
		case l == "" || strings.HasPrefix(l, "#"):
		case strings.HasPrefix(l, "S:"):
			steps = append(steps, step{server: true, text: strings.TrimPrefix(l[2:], " "), line: i})
		case strings.HasPrefix(l, "C:"):
			steps = append(steps, step{text: strings.TrimPrefix(l[2:], " "), line: i})
		default:
			t.Fatalf("%s:%d: not a transcript line: %q", path, i+1, l)
		}
	}
	return lines, steps
}

// scripted is a TLS server that plays one transcript on one connection.
type scripted struct {
	addr *net.TCPAddr
	pool *x509.CertPool
	done chan struct{}
	// got holds what the client sent for each C: step.
	got []string
}

func newScripted(t *testing.T, steps []step) *scripted {
	t.Helper()
	cert, pool := testCert(t)
	inner, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := tls.NewListener(inner, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	s := &scripted{addr: inner.Addr().(*net.TCPAddr), pool: pool, done: make(chan struct{})}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		defer close(s.done)
		conn, err := ln.Accept()
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		s.play(t, conn, steps)
	}()
	return s
}

func (s *scripted) play(t *testing.T, conn net.Conn, steps []step) {
	r := bufio.NewReader(conn)
	for _, st := range steps {
		switch {
		case st.server && st.text == hang:
			_, _ = io.Copy(io.Discard, r)
		case st.server:
			if _, err := io.WriteString(conn, st.text+"\r\n"); err != nil {
				t.Errorf("server write: %v", err)
				return
			}
		default:
			line, err := r.ReadString('\n')
			got := strings.TrimSuffix(line, "\r\n")
			if err != nil {
				got = closed
				if line != "" {
					got = line + " " + closed
				}
			}
			s.got = append(s.got, got)
			if !*update && got != st.text {
				t.Errorf("client sent %q, transcript has %q", got, st.text)
			}
			if err != nil {
				return
			}
		}
	}
}

func (s *scripted) config() Config {
	return Config{Host: "127.0.0.1", Port: s.addr.Port, RootCAs: s.pool, IdleTimeout: 5 * time.Second}
}

// runTranscript plays testdata/transcripts/<name>.txt against the client
// that drive works with. drive returns the client, if it got one, and the
// harness closes it.
func runTranscript(t *testing.T, name string, drive func(cfg Config) *Client) {
	t.Helper()
	path := filepath.Join("testdata", "transcripts", name+".txt")
	lines, steps := loadTranscript(t, path)
	s := newScripted(t, steps)
	if c := drive(s.config()); c != nil {
		_ = c.Close()
	}
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		t.Fatal("server did not finish the transcript")
	}
	var want []step
	for _, st := range steps {
		if !st.server {
			want = append(want, st)
		}
	}
	if *update {
		if len(s.got) != len(want) {
			t.Fatalf("client sent %d lines, transcript expects %d; fix the S: lines first", len(s.got), len(want))
		}
		for i, st := range want {
			lines[st.line] = "C: " + s.got[i]
			if s.got[i] == "" {
				lines[st.line] = "C:" // no trailing space
			}
		}
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if len(s.got) != len(want) {
		t.Errorf("client sent %d lines, transcript expects %d", len(s.got), len(want))
	}
}

// wantKind fails unless err is an *Error of kind k.
func wantKind(t *testing.T, err error, k ErrorKind) {
	t.Helper()
	_ = kindErr(t, err, k)
}

// kindErr is wantKind returning the error.
func kindErr(t *testing.T, err error, k ErrorKind) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want an *Error of kind %v", err, k)
	}
	if e.Kind != k {
		t.Fatalf("kind = %v, want %v (%v)", e.Kind, k, err)
	}
	return e
}

var certOnce struct {
	sync.Once
	cert tls.Certificate
	pool *x509.CertPool
	err  error
}

// testCert returns a self-signed certificate for 127.0.0.1 and a pool
// that trusts it, made once per test binary.
func testCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	certOnce.Do(func() {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			certOnce.err = err
			return
		}
		tmpl := &x509.Certificate{
			SerialNumber:          big.NewInt(1),
			Subject:               pkix.Name{CommonName: "yuuka imap test"},
			NotBefore:             time.Now().Add(-time.Hour),
			NotAfter:              time.Now().Add(24 * time.Hour),
			IsCA:                  true,
			BasicConstraintsValid: true,
			KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
			ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			certOnce.err = err
			return
		}
		leaf, err := x509.ParseCertificate(der)
		if err != nil {
			certOnce.err = err
			return
		}
		certOnce.pool = x509.NewCertPool()
		certOnce.pool.AddCert(leaf)
		certOnce.cert = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	})
	if certOnce.err != nil {
		t.Fatal(certOnce.err)
	}
	return certOnce.cert, certOnce.pool
}
