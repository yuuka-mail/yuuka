package imap

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// The tests here run the client against go-imap's server, an
// independent implementation of the protocol, rather than against
// transcripts written by the same hand as the client. go-imap is a test
// dependency only: nothing outside _test.go files imports it, so it never
// reaches the yuuka binary.

type literal struct {
	*bytes.Reader
}

func (l literal) Size() int64 { return l.Reader.Size() }

type seeded struct {
	raw   []byte
	flags []goimap.Flag
	date  time.Time
}

// startServer runs go-imap's in-memory server over implicit TLS with one
// user, "user@example.com" / "app-password", and these mailboxes:
// INBOX with msgs, an empty "Archive/2026", and "Reçus".
func startServer(t *testing.T, msgs []seeded) Config {
	t.Helper()
	user := imapmemserver.NewUser("user@example.com", "app-password")
	for _, name := range []string{"INBOX", "Archive", "Archive/2026", "Reçus", `Work "old"`} {
		if err := user.Create(name, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range msgs {
		if _, err := user.Append("INBOX", literal{bytes.NewReader(m.raw)}, &goimap.AppendOptions{Flags: m.flags, Time: m.date}); err != nil {
			t.Fatal(err)
		}
	}
	mem := imapmemserver.New()
	mem.AddUser(user)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps: goimap.CapSet{
			goimap.CapIMAP4rev1:    {},
			goimap.CapListExtended: {},
			goimap.CapListStatus:   {},
			goimap.CapSpecialUse:   {},
			goimap.CapUIDPlus:      {},
			goimap.CapLiteralPlus:  {},
			goimap.CapNamespace:    {},
			goimap.CapStatusSize:   {},
			goimap.CapChildren:     {},
		},
		Logger: testLogger{t},
	})
	cert, pool := testCert(t)
	inner, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := tls.NewListener(inner, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		<-done
	})
	return Config{Host: "127.0.0.1", Port: inner.Addr().(*net.TCPAddr).Port, RootCAs: pool, IdleTimeout: 5 * time.Second}
}

type testLogger struct{ t *testing.T }

func (l testLogger) Printf(format string, args ...any) { l.t.Logf("imapserver: "+format, args...) }

func synthetic(i int, body string) []byte {
	return fmt.Appendf(nil, "From: Sender %d <sender%d@example.com>\r\nTo: user@example.com\r\nSubject: Message %d\r\nMessage-ID: <%d@example.com>\r\n\r\n%s\r\n", i, i, i, i, body)
}

func TestAgainstImapserver(t *testing.T) {
	base := time.Date(2026, 10, 1, 8, 30, 0, 0, time.FixedZone("", 9*3600))
	msgs := []seeded{
		{raw: synthetic(1, "Hello."), flags: []goimap.Flag{goimap.FlagSeen}, date: base},
		{raw: synthetic(2, "Unread, and it must stay unread."), date: base.Add(time.Hour)},
		{raw: synthetic(3, strings.Repeat("A large line of an attachment.\r\n", 4096)), flags: []goimap.Flag{goimap.FlagFlagged, "$Forwarded"}, date: base.Add(2 * time.Hour)},
	}
	cfg := startServer(t, msgs)
	ctx := t.Context()

	c := dial(t, cfg)
	if !c.Caps().Has("AUTH=PLAIN") || !c.Caps().Has("SASL-IR") {
		t.Errorf("greeting caps = %v", c.Caps())
	}
	wantKind(t, c.Login(ctx, "user@example.com", "wrong"), KindAuth)
	must(t, c.Login(ctx, "user@example.com", "app-password"))
	if !c.Caps().Has("LIST-STATUS") || c.Caps().Has("ID") {
		t.Errorf("post-login caps = %v", c.Caps())
	}

	boxes, err := c.List(ctx, ListOptions{SpecialUse: true, Status: []string{"MESSAGES", "UIDNEXT", "UIDVALIDITY"}})
	must(t, err)
	byName := map[string]Mailbox{}
	for _, b := range boxes {
		byName[b.DisplayName()] = b
	}
	if len(byName) != 5 {
		t.Fatalf("mailboxes = %+v", boxes)
	}
	inbox := byName["INBOX"]
	if inbox.Status == nil || inbox.Status.Messages != 3 || inbox.Status.UIDNext != 4 || inbox.Status.UIDValidity == 0 {
		t.Errorf("INBOX = %+v %+v", inbox, inbox.Status)
	}
	recus := byName["Reçus"]
	if recus.Name != "Re&AOc-us" || recus.Delim != "/" || recus.Status == nil {
		t.Errorf("Reçus = %+v", recus)
	}

	// The wire names round-trip: what LIST returned opens the mailbox.
	for _, name := range []string{recus.Name, byName[`Work "old"`].Name, byName["Archive/2026"].Name} {
		st, err := c.Status(ctx, name, "MESSAGES", "UIDVALIDITY")
		must(t, err)
		if st.Messages != 0 || st.UIDValidity == 0 {
			t.Errorf("STATUS %s = %+v", name, st)
		}
	}
	e := kindErr(t, func() error { _, err := c.Examine(ctx, "Missing"); return err }(), KindRefused)
	if e.Resp.Type != "NO" {
		t.Errorf("EXAMINE Missing = %v", e)
	}

	sel, err := c.Examine(ctx, inbox.Name)
	must(t, err)
	if sel.UIDValidity != inbox.Status.UIDValidity || sel.UIDNext != 4 || sel.Exists != 3 || !sel.ReadOnly {
		t.Errorf("Examine = %+v", sel)
	}

	type meta struct {
		size  int64
		date  time.Time
		flags []string
	}
	got := map[uint32]meta{}
	must(t, c.UIDFetch(ctx, fmt.Sprintf("1:%d", sel.UIDNext-1), []string{"UID", "RFC822.SIZE", "INTERNALDATE", "FLAGS"}, 0, func(m *Message) error {
		uid, _ := m.UID()
		size, ok1 := m.Size()
		date, ok2 := m.InternalDate()
		flags, ok3 := m.Flags()
		if !ok1 || !ok2 || !ok3 {
			t.Errorf("UID %d: %+v", uid, m.Attrs)
		}
		got[uid] = meta{size, date, flags}
		return nil
	}))
	for i, m := range msgs {
		g := got[uint32(i+1)]
		if g.size != int64(len(m.raw)) || !g.date.Equal(m.date) {
			t.Errorf("UID %d: size %d date %v, want %d %v", i+1, g.size, g.date, len(m.raw), m.date)
		}
		want := make([]string, len(m.flags))
		for j, f := range m.flags {
			want[j] = strings.ToLower(string(f))
		}
		// Flags are case-insensitive, and go-imap's server lowers them.
		for j := range g.flags {
			g.flags[j] = strings.ToLower(g.flags[j])
		}
		slices.Sort(want)
		slices.Sort(g.flags)
		if !slices.Equal(g.flags, want) {
			t.Errorf("UID %d: flags %q, want %q", i+1, g.flags, want)
		}
	}

	bodies := map[uint32][]byte{}
	headers := map[uint32][]byte{}
	must(t, c.UIDFetch(ctx, "1:3", []string{"UID", "BODY.PEEK[]", "BODY.PEEK[HEADER]"}, 1<<20, func(m *Message) error {
		uid, _ := m.UID()
		bodies[uid], _ = m.Body("")
		headers[uid], _ = m.Body("HEADER")
		return nil
	}))
	for i, m := range msgs {
		uid := uint32(i + 1)
		if !bytes.Equal(bodies[uid], m.raw) {
			t.Errorf("UID %d: body differs (%d octets, want %d)", uid, len(bodies[uid]), len(m.raw))
		}
		if h, _, _ := bytes.Cut(m.raw, []byte("\r\n\r\n")); !bytes.Equal(headers[uid], append(h, "\r\n\r\n"...)) {
			t.Errorf("UID %d: header = %q", uid, headers[uid])
		}
	}

	// EXAMINE and BODY.PEEK leave the mailbox as it was.
	must(t, c.UIDFetch(ctx, "2", []string{"FLAGS"}, 0, func(m *Message) error {
		if f, _ := m.Flags(); slices.ContainsFunc(f, func(s string) bool { return strings.EqualFold(s, `\Seen`) }) {
			t.Error("fetching set \\Seen")
		}
		return nil
	}))
	uids, err := c.UIDSearchAll(ctx)
	must(t, err)
	if !reflect.DeepEqual(uids, []uint32{1, 2, 3}) {
		t.Errorf("SEARCH = %v", uids)
	}
	must(t, c.Unselect(ctx))
	must(t, c.Noop(ctx))

	// A message over the limit ends the connection before it is read.
	_, err = c.Examine(ctx, inbox.Name)
	must(t, err)
	err = c.UIDFetch(ctx, "3", []string{"BODY.PEEK[]"}, int64(len(msgs[2].raw)-1), func(*Message) error {
		t.Error("callback ran for an oversized message")
		return nil
	})
	wantKind(t, err, KindTooLarge)
	wantKind(t, c.Noop(ctx), KindTooLarge)

	// A second session, with AUTHENTICATE PLAIN.
	c = dial(t, cfg)
	wantKind(t, c.AuthenticatePlain(ctx, "user@example.com", "wrong"), KindAuth)
	must(t, c.AuthenticatePlain(ctx, "user@example.com", "app-password"))
	if _, err := c.Examine(ctx, "INBOX"); err != nil {
		t.Fatal(err)
	}
	must(t, c.Logout(ctx))
	if err := c.Noop(ctx); !errors.Is(err, net.ErrClosed) {
		t.Errorf("after Logout: %v", err)
	}
}
