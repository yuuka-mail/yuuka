package transport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"math/big"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// behavior scripts the fake server. Zero fields take the defaults, which
// accept everything.
type behavior struct {
	greeting string            // first reply; "hang" never sends one
	ext      []string          // EHLO keywords
	auth     string            // reply to AUTH
	mail     string            // reply to MAIL
	rcpt     map[string]string // reply per RCPT address
	data     string            // reply to DATA
	// final is the reply to the end of data. "drop" closes the connection
	// instead; "hang" never answers and waits for the client to go away.
	final string
}

func (b behavior) withDefaults() behavior {
	def := func(s *string, v string) {
		if *s == "" {
			*s = v
		}
	}
	def(&b.greeting, "220 fake.example.com ESMTP")
	def(&b.auth, "235 2.7.0 Authentication successful")
	def(&b.mail, "250 2.1.0 Ok")
	def(&b.data, "354 End data with <CR><LF>.<CR><LF>")
	def(&b.final, "250 2.0.0 Ok: queued as ABC123")
	if b.ext == nil {
		b.ext = []string{"PIPELINING", "SIZE 10240000", "AUTH PLAIN LOGIN", "8BITMIME"}
	}
	return b
}

// session is what the server saw on one connection.
type session struct {
	auth    string   // decoded AUTH PLAIN response
	mail    string   // MAIL command line
	rcpts   []string // RCPT addresses
	sawData bool     // a DATA command arrived
	data    []byte   // message lines received, dot-unstuffed, CRLF kept
	dot     bool     // the end-of-data arrived
}

type fakeServer struct {
	t        *testing.T
	b        behavior
	addr     *net.TCPAddr
	pool     *x509.CertPool
	conns    atomic.Int32
	sessions chan *session

	mu   sync.Mutex
	live *session // the session in progress, for progress checks
}

func newServer(t *testing.T, b behavior) *fakeServer {
	t.Helper()
	cert, pool := testCert(t)
	inner, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := tls.NewListener(inner, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	s := &fakeServer{
		t:        t,
		b:        b.withDefaults(),
		addr:     inner.Addr().(*net.TCPAddr),
		pool:     pool,
		sessions: make(chan *session, 16),
	}
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
			s.conns.Add(1)
			wg.Go(func() { s.serve(conn) })
		}
	})
	return s
}

// config points a transport at the server, trusting its test CA.
func (s *fakeServer) config() Config {
	return Config{
		Name:     "fake",
		Host:     "127.0.0.1",
		Port:     s.addr.Port,
		Username: "user",
		Password: "secret",
		RootCAs:  s.pool,
	}
}

// next returns the next finished session.
func (s *fakeServer) next(t *testing.T) *session {
	t.Helper()
	select {
	case sess := <-s.sessions:
		return sess
	case <-time.After(5 * time.Second):
		t.Fatal("no session finished")
		return nil
	}
}

// progress reports how many message bytes the live session has received
// and whether the end-of-data arrived.
func (s *fakeServer) progress() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live == nil {
		return 0, false
	}
	return len(s.live.data), s.live.dot
}

func (s *fakeServer) serve(conn net.Conn) {
	sess := &session{}
	s.mu.Lock()
	s.live = sess
	s.mu.Unlock()
	defer func() {
		_ = conn.Close()
		s.sessions <- sess
	}()
	tp := textproto.NewConn(conn)
	if s.b.greeting == "hang" {
		_, _ = io.Copy(io.Discard, conn)
		return
	}
	if tp.PrintfLine("%s", s.b.greeting) != nil || s.b.greeting[0] != '2' {
		return
	}
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		reply := "250 2.0.0 Ok"
		switch strings.ToUpper(verb) {
		case "EHLO":
			lines := append([]string{"fake.example.com"}, s.b.ext...)
			for i, l := range lines {
				sep := "-"
				if i == len(lines)-1 {
					sep = " "
				}
				_ = tp.PrintfLine("250%s%s", sep, l)
			}
			continue
		case "AUTH":
			_, resp, _ := strings.Cut(arg, " ")
			dec, _ := base64.StdEncoding.DecodeString(resp)
			sess.auth = string(dec)
			reply = s.b.auth
		case "MAIL":
			sess.mail = line
			reply = s.b.mail
		case "RCPT":
			addr := strings.TrimSuffix(strings.TrimPrefix(arg, "TO:<"), ">")
			sess.rcpts = append(sess.rcpts, addr)
			reply = "250 2.1.5 Ok"
			if r, ok := s.b.rcpt[addr]; ok {
				reply = r
			}
		case "DATA":
			sess.sawData = true
			_ = tp.PrintfLine("%s", s.b.data)
			if !strings.HasPrefix(s.b.data, "354") {
				continue
			}
			if !s.readData(tp, sess) {
				return // connection dropped before the end-of-data: discard
			}
			switch s.b.final {
			case "drop":
				return
			case "hang":
				_, _ = io.Copy(io.Discard, tp.R)
				return
			}
			reply = s.b.final
		case "QUIT":
			_ = tp.PrintfLine("221 2.0.0 Bye")
			return
		case "RSET", "NOOP":
		default:
			reply = "500 5.5.2 Error: command not recognized"
		}
		if tp.PrintfLine("%s", reply) != nil {
			return
		}
	}
}

// readData reads message lines up to the end-of-data, keeping the bytes
// exactly as sent apart from dot-unstuffing. It reports whether the
// end-of-data arrived.
func (s *fakeServer) readData(tp *textproto.Conn, sess *session) bool {
	for {
		line, err := tp.R.ReadString('\n')
		if err != nil {
			return false
		}
		s.mu.Lock()
		if line == ".\r\n" {
			sess.dot = true
		} else {
			sess.data = append(sess.data, strings.TrimPrefix(line, ".")...)
		}
		s.mu.Unlock()
		if sess.dot {
			return true
		}
	}
}

// testCert returns a self-signed certificate for 127.0.0.1 and a pool
// that trusts it.
func testCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "yuuka transport test"},
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

// commits counts calls to a commit callback that returns err.
type commits struct {
	n   atomic.Int32
	err error
}

func (c *commits) fn(_ context.Context) error {
	c.n.Add(1)
	return c.err
}
