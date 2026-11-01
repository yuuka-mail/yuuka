package pull

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is a scripted IMAP server: just the commands the client
// sends, over the mailboxes the test arranges, with faults it can turn
// on. go-imap's server, which the client's own tests use, cannot speak
// Gmail's X-GM-MSGID and X-GM-LABELS or RFC 8474's EMAILID, which are
// what the keys are made of.
type fakeServer struct {
	mu   sync.Mutex
	user string
	pass string
	// caps are announced after login; before it, IMAP4rev1 and AUTH=PLAIN.
	caps    []string
	folders []*fakeFolder

	// Faults.
	refuse    bool            // refuse every login
	noBody    map[uint32]bool // answer NO to fetching these UIDs' bodies
	overquota bool            // answer NO [OVERQUOTA] to fetching any body
	lieSize   map[uint32]int64

	// What happened.
	logins   int
	examines int
	bodies   map[uint32]int // body fetches per UID

	addr *net.TCPAddr
	pool *x509.CertPool
}

type fakeFolder struct {
	name        string
	attrs       []string
	uidvalidity uint32
	uidnext     uint32
	msgs        []*fakeMsg // ascending UID
}

type fakeMsg struct {
	uid     uint32
	raw     []byte
	date    time.Time
	flags   []string
	gmID    uint64
	labels  []string
	emailID string
}

func newFakeServer(t testing.TB, caps ...string) *fakeServer {
	t.Helper()
	s := &fakeServer{
		user: "me@example.com", pass: "app-password", caps: caps, noBody: map[uint32]bool{},
		lieSize: map[uint32]int64{}, bodies: map[uint32]int{},
	}
	cert, pool := testCert(t)
	inner, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := tls.NewListener(inner, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	s.addr, s.pool = inner.Addr().(*net.TCPAddr), pool
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer func() { _ = conn.Close() }()
				s.serve(conn)
			})
		}
	})
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	return s
}

// folder adds a folder.
func (s *fakeServer) folder(name string, uidvalidity uint32, attrs ...string) *fakeFolder {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := &fakeFolder{name: name, attrs: attrs, uidvalidity: uidvalidity, uidnext: 1}
	s.folders = append(s.folders, f)
	return f
}

// add appends m to f with the next UID, which it returns.
func (s *fakeServer) add(f *fakeFolder, m fakeMsg) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	m.uid = f.uidnext
	f.uidnext++
	f.msgs = append(f.msgs, &m)
	return m.uid
}

// renumber gives f a new UIDVALIDITY and its messages new UIDs, as a
// server that rebuilt its index does.
func (s *fakeServer) renumber(f *fakeFolder, uidvalidity, first uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f.uidvalidity = uidvalidity
	for i, m := range f.msgs {
		m.uid = first + uint32(i) //nolint:gosec // a handful of test messages
	}
	f.uidnext = first + uint32(len(f.msgs)) //nolint:gosec // as above
}

func (s *fakeServer) set(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn()
}

func (s *fakeServer) count(fn func() int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn()
}

func (s *fakeServer) serve(conn net.Conn) {
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	out := func(format string, args ...any) {
		_, _ = fmt.Fprintf(w, format+"\r\n", args...)
	}
	out("* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN SASL-IR ID] fake ready")
	_ = w.Flush()
	var sel *fakeFolder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		tag, rest, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		toks := tokens(rest)
		if len(toks) == 0 {
			out("%s BAD empty", tag)
			_ = w.Flush()
			continue
		}
		cmd := strings.ToUpper(toks[0])
		if cmd == "UID" && len(toks) > 1 {
			cmd += " " + strings.ToUpper(toks[1])
			toks = toks[1:]
		}
		s.mu.Lock()
		switch cmd {
		case "CAPABILITY":
			out("* CAPABILITY IMAP4rev1 AUTH=PLAIN SASL-IR ID")
			out("%s OK done", tag)
		case "ID":
			out("* ID NIL")
			out("%s OK done", tag)
		case "LOGIN", "AUTHENTICATE":
			s.logins++
			user, pass := "", ""
			if cmd == "LOGIN" && len(toks) == 3 {
				user, pass = toks[1], toks[2]
			} else if len(toks) == 3 {
				b, _ := base64.StdEncoding.DecodeString(toks[2])
				parts := strings.Split(string(b), "\x00")
				if len(parts) == 3 {
					user, pass = parts[1], parts[2]
				}
			}
			if s.refuse || user != s.user || pass != s.pass {
				out("%s NO [AUTHENTICATIONFAILED] Invalid credentials", tag)
				break
			}
			out("%s OK [CAPABILITY %s] logged in", tag, strings.Join(append([]string{"IMAP4rev1"}, s.caps...), " "))
		case "LIST":
			status := slices.Contains(toks, "STATUS")
			for _, f := range s.folders {
				out(`* LIST (%s) "/" %s`, strings.Join(f.attrs, " "), strconv.Quote(f.name))
				if status {
					out(`* STATUS %s (MESSAGES %d UIDNEXT %d UIDVALIDITY %d)`, strconv.Quote(f.name), len(f.msgs), f.uidnext, f.uidvalidity)
				}
			}
			out("%s OK done", tag)
		case "STATUS":
			f := s.find(toks[1])
			if f == nil {
				out("%s NO no such mailbox", tag)
				break
			}
			out(`* STATUS %s (MESSAGES %d UIDNEXT %d UIDVALIDITY %d)`, strconv.Quote(f.name), len(f.msgs), f.uidnext, f.uidvalidity)
			out("%s OK done", tag)
		case "EXAMINE":
			sel = s.find(toks[1])
			if sel == nil {
				out("%s NO no such mailbox", tag)
				break
			}
			s.examines++
			out("* %d EXISTS", len(sel.msgs))
			out("* OK [UIDVALIDITY %d] ok", sel.uidvalidity)
			out("* OK [UIDNEXT %d] ok", sel.uidnext)
			out("%s OK [READ-ONLY] examined", tag)
		case "UID SEARCH":
			var uids []string
			for _, m := range sel.msgs {
				uids = append(uids, strconv.FormatUint(uint64(m.uid), 10))
			}
			out("* SEARCH %s", strings.Join(uids, " "))
			out("%s OK done", tag)
		case "UID FETCH":
			s.fetch(w, tag, sel, toks[1], toks[2:])
		case "NOOP", "UNSELECT":
			out("%s OK done", tag)
		case "LOGOUT":
			out("* BYE bye")
			out("%s OK done", tag)
			s.mu.Unlock()
			_ = w.Flush()
			return
		default:
			out("%s BAD unknown command", tag)
		}
		s.mu.Unlock()
		if err := w.Flush(); err != nil {
			return
		}
	}
}

func (s *fakeServer) find(name string) *fakeFolder {
	for _, f := range s.folders {
		if f.name == name {
			return f
		}
	}
	return nil
}

func (s *fakeServer) fetch(w *bufio.Writer, tag string, f *fakeFolder, set string, items []string) {
	if f == nil {
		_, _ = fmt.Fprintf(w, "%s BAD no mailbox\r\n", tag)
		return
	}
	in := parseSet(set, f)
	var body string
	for _, it := range items {
		if strings.HasPrefix(strings.ToUpper(it), "BODY.PEEK[") {
			body = strings.ToUpper(it)
		}
	}
	for i, m := range f.msgs {
		if !in(m.uid) {
			continue
		}
		if body != "" {
			switch {
			case s.overquota:
				_, _ = fmt.Fprintf(w, "%s NO [OVERQUOTA] Account exceeded command or bandwidth limits.\r\n", tag)
				return
			case s.noBody[m.uid]:
				_, _ = fmt.Fprintf(w, "%s NO Some messages could not be FETCHed\r\n", tag)
				return
			}
			s.bodies[m.uid]++
		}
		var parts []string
		for _, it := range items {
			switch strings.ToUpper(strings.Trim(it, "()")) {
			case "UID":
				parts = append(parts, fmt.Sprintf("UID %d", m.uid))
			case "RFC822.SIZE":
				size := int64(len(m.raw))
				if n, ok := s.lieSize[m.uid]; ok {
					size = n
				}
				parts = append(parts, fmt.Sprintf("RFC822.SIZE %d", size))
			case "INTERNALDATE":
				parts = append(parts, fmt.Sprintf("INTERNALDATE %q", m.date.Format("02-Jan-2006 15:04:05 -0700")))
			case "FLAGS":
				parts = append(parts, "FLAGS ("+strings.Join(m.flags, " ")+")")
			case "X-GM-MSGID":
				parts = append(parts, fmt.Sprintf("X-GM-MSGID %d", m.gmID))
			case "X-GM-LABELS":
				var ls []string
				for _, l := range m.labels {
					if strings.HasPrefix(l, `\`) {
						ls = append(ls, l)
					} else {
						ls = append(ls, strconv.Quote(l))
					}
				}
				parts = append(parts, "X-GM-LABELS ("+strings.Join(ls, " ")+")")
			case "EMAILID":
				parts = append(parts, "EMAILID ("+m.emailID+")")
			case "BODY.PEEK[]":
				parts = append(parts, fmt.Sprintf("BODY[] {%d}\r\n%s", len(m.raw), m.raw))
			case "BODY.PEEK[HEADER]":
				h, _, _ := strings.Cut(string(m.raw), "\r\n\r\n")
				h += "\r\n\r\n"
				parts = append(parts, fmt.Sprintf("BODY[HEADER] {%d}\r\n%s", len(h), h))
			}
		}
		_, _ = fmt.Fprintf(w, "* %d FETCH (%s)\r\n", i+1, strings.Join(parts, " "))
	}
	_, _ = fmt.Fprintf(w, "%s OK done\r\n", tag)
}

// parseSet reads a UID set such as 1:5,7,9:* against f.
func parseSet(set string, f *fakeFolder) func(uint32) bool {
	top := uint32(0)
	if n := len(f.msgs); n > 0 {
		top = f.msgs[n-1].uid
	}
	num := func(s string) uint32 {
		if s == "*" {
			return top
		}
		n, _ := strconv.ParseUint(s, 10, 32)
		return uint32(n)
	}
	type span struct{ a, b uint32 }
	var spans []span
	for p := range strings.SplitSeq(set, ",") {
		a, b, ok := strings.Cut(p, ":")
		if !ok {
			b = a
		}
		x, y := num(a), num(b)
		if x > y {
			x, y = y, x
		}
		spans = append(spans, span{x, y})
	}
	return func(u uint32) bool {
		for _, s := range spans {
			if u >= s.a && u <= s.b {
				return true
			}
		}
		return false
	}
}

// tokens splits a command line into atoms, quoted strings (unquoted) and
// parentheses, which are dropped: the fake only needs the words.
func tokens(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		switch s[i] {
		case ' ', '(', ')':
			i++
		case '"':
			var b strings.Builder
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				b.WriteByte(s[i])
				i++
			}
			i++
			out = append(out, b.String())
		default:
			j := i
			for j < len(s) && s[j] != ' ' && s[j] != '(' && s[j] != ')' {
				j++
			}
			out = append(out, s[i:j])
			i = j
		}
	}
	return out
}

func testCert(t testing.TB) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "imap.example.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(c)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}
