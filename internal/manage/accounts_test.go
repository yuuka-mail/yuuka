package manage

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"slices"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/yuuka-mail/yuuka/internal/imap"
	"github.com/yuuka-mail/yuuka/internal/store"
)

func TestSourceWidens(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	earlier, later := since.AddDate(-1, 0, 0), since.AddDate(0, 6, 0)
	base := store.SourceConfig{
		Address: "me@example.com", Host: "imap.example.com", Port: 993, Auth: store.SourceAuthPlain, Username: "me@example.com",
		Mailboxes: []string{"INBOX", "Archive"}, TrustedAuthservID: "mx.example.com", MaxMessageBytes: 16 << 20,
		BackfillSince: &since, DailyByteBudget: 1 << 30, PollSeconds: 60,
	}
	for _, c := range []struct {
		name   string
		change func(*store.SourceConfig)
		pw     bool
		want   []string
	}{
		{"nothing", func(*store.SourceConfig) {}, false, nil},
		{"new password", func(*store.SourceConfig) {}, true, []string{WidenPassword}},
		{"another host", func(c *store.SourceConfig) { c.Host = "imap.example.net" }, true, []string{WidenDestination, WidenPassword}},
		{"another user", func(c *store.SourceConfig) { c.Username = "other" }, false, []string{WidenDestination}},
		{"another address", func(c *store.SourceConfig) { c.Address = "you@example.com" }, false, []string{WidenAddress}},
		{"a preset", func(c *store.SourceConfig) { c.Preset = store.PresetGmail }, false, []string{WidenPreset}},
		{"authserv-id cleared", func(c *store.SourceConfig) { c.TrustedAuthservID = "" }, false, []string{WidenAuthservID}},
		{"authserv-id changed", func(c *store.SourceConfig) { c.TrustedAuthservID = "mx.example.net" }, false, []string{WidenAuthservID}},
		{"a folder fewer", func(c *store.SourceConfig) { c.Mailboxes = []string{"INBOX"} }, false, nil},
		{"folders reordered", func(c *store.SourceConfig) { c.Mailboxes = []string{"Archive", "INBOX"} }, false, nil},
		{"a folder more", func(c *store.SourceConfig) { c.Mailboxes = []string{"INBOX", "Archive", "Sent"} }, false, []string{WidenMailboxes}},
		{"default folders", func(c *store.SourceConfig) { c.Mailboxes = nil }, false, []string{WidenMailboxes}},
		{"backfill all", func(c *store.SourceConfig) { c.BackfillSince = nil }, false, []string{WidenBackfill}},
		{"backfill further", func(c *store.SourceConfig) { c.BackfillSince = &earlier }, false, []string{WidenBackfill}},
		{"backfill less", func(c *store.SourceConfig) { c.BackfillSince = &later }, false, nil},
		{"larger mail", func(c *store.SourceConfig) { c.MaxMessageBytes = 32 << 20 }, false, []string{WidenMaxSize}},
		{"smaller mail", func(c *store.SourceConfig) { c.MaxMessageBytes = 1 << 20 }, false, nil},
		{"no budget", func(c *store.SourceConfig) { c.DailyByteBudget = 0 }, false, []string{WidenBudget}},
		{"larger budget", func(c *store.SourceConfig) { c.DailyByteBudget = 2 << 30 }, false, []string{WidenBudget}},
		{"smaller budget", func(c *store.SourceConfig) { c.DailyByteBudget = 1 << 20 }, false, nil},
		{"faster poll", func(c *store.SourceConfig) { c.PollSeconds = 30 }, false, []string{WidenPoll}},
		{"slower poll", func(c *store.SourceConfig) { c.PollSeconds = 600 }, false, nil},
	} {
		next := base
		next.Mailboxes = slices.Clone(base.Mailboxes)
		c.change(&next)
		if got := SourceWidens(base, next, c.pw); !slices.Equal(got, c.want) {
			t.Errorf("%s: widens %v, want %v", c.name, got, c.want)
		}
	}
	// From an unlimited budget nothing is larger; from the default
	// folders a list is the server's to compare.
	unlimited := base
	unlimited.DailyByteBudget, unlimited.Mailboxes = 0, nil
	limited := unlimited
	limited.DailyByteBudget = 1 << 30
	if got := SourceWidens(unlimited, limited, false); len(got) != 0 {
		t.Errorf("a budget where there was none: %v", got)
	}
	limited.Mailboxes = []string{"INBOX"}
	if got := SourceWidens(unlimited, limited, false); !slices.Equal(got, []string{WidenMailboxes}) {
		t.Errorf("a list where the default was: %v", got)
	}
}

func TestAccessWidens(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	for _, c := range []struct {
		def     bool
		was, to *bool
		widens  bool
	}{
		{false, nil, &yes, true},
		{false, &no, &yes, true},
		{false, &yes, &yes, false},
		{false, &yes, nil, false}, // back to an unreadable default
		{false, &no, nil, false},
		{true, &no, nil, true}, // --default when the default is readable
		{true, nil, &yes, false},
		{true, nil, &no, false},
		{true, &no, &no, false},
	} {
		if got := AccessWidens(c.def, c.was, c.to); got != c.widens {
			t.Errorf("default %v, %v -> %v: widens %v", c.def, show(c.was), show(c.to), got)
		}
	}
	if !DefaultReadWidens(false, true) || DefaultReadWidens(true, false) || DefaultReadWidens(true, true) {
		t.Error("DefaultReadWidens")
	}
}

func show(b *bool) any {
	if b == nil {
		return "default"
	}
	return *b
}

func TestSyncFolders(t *testing.T) {
	t.Parallel()
	boxes := []imap.Mailbox{
		{Name: "INBOX"},
		{Name: "[Gmail]", Attrs: []string{`\Noselect`}},
		{Name: "[Gmail]/All Mail", Attrs: []string{`\All`}},
		{Name: "[Gmail]/Spam", Attrs: []string{`\Junk`}},
		{Name: "[Gmail]/Trash", Attrs: []string{`\Trash`}},
		{Name: "[Gmail]/Drafts", Attrs: []string{`\Drafts`}},
		{Name: "Re&AOc-us"}, // Reçus
	}
	names := func(ms []imap.Mailbox) []string {
		var out []string
		for _, m := range ms {
			out = append(out, m.Name)
		}
		return out
	}
	got, missing := SyncFolders(store.SourceConfig{Preset: store.PresetGmail}, boxes)
	if !slices.Equal(names(got), []string{"[Gmail]/All Mail"}) || missing != nil {
		t.Errorf("gmail: %v, missing %v", names(got), missing)
	}
	got, missing = SyncFolders(store.SourceConfig{}, boxes)
	if !slices.Equal(names(got), []string{"INBOX", "[Gmail]/All Mail", "Re&AOc-us"}) || missing != nil {
		t.Errorf("default: %v, missing %v", names(got), missing)
	}
	got, missing = SyncFolders(store.SourceConfig{Mailboxes: []string{"Reçus", "Gone", "[Gmail]"}}, boxes)
	if !slices.Equal(names(got), []string{"Re&AOc-us"}) || !slices.Equal(missing, []string{"Gone", "[Gmail]"}) {
		t.Errorf("configured: %v, missing %v", names(got), missing)
	}
	if _, missing := SyncFolders(store.SourceConfig{Preset: store.PresetGmail}, boxes[:1]); !slices.Equal(missing, []string{`\All`}) {
		t.Errorf("gmail without \\All: missing %v", missing)
	}
}

func TestCheckSourceConfig(t *testing.T) {
	t.Parallel()
	c := NewSourceConfig(store.PresetGmail)
	c.Address, c.Username = "Me@Example.com", "me@example.com"
	got, err := CheckSourceConfig(c)
	if err != nil || got.Address != "me@example.com" || got.Host != GmailHost || got.DailyByteBudget != GmailDailyBudget ||
		got.TrustedAuthservID != "" {
		t.Errorf("gmail preset: %+v, %v", got, err)
	}
	c.MaxMessageBytes = MaxMessageBytes + 1
	var inv *Invalid
	if _, err := CheckSourceConfig(c); !errors.As(err, &inv) {
		t.Errorf("larger than a raw object: %v", err)
	}
	c = NewSourceConfig("")
	c.Address, c.Username = "me@example.com", "me"
	if _, err := CheckSourceConfig(c); !errors.As(err, &inv) {
		t.Errorf("no host: %v", err)
	}
}

// TestProbeSource runs a test against go-imap's server: login, the
// folders to sync, and what is missing.
func TestProbeSource(t *testing.T) {
	t.Parallel()
	user := imapmemserver.NewUser("me@example.com", "app-password")
	for _, name := range []string{"INBOX", "Archive", "Reçus"} {
		if err := user.Create(name, nil); err != nil {
			t.Fatal(err)
		}
	}
	// The in-memory server keeps no special-use attributes, so which
	// folders the default leaves out is TestSyncFolders'.
	raw := []byte("From: a@example.net\r\nTo: me@example.com\r\nSubject: hi\r\n\r\nhello\r\n")
	if _, err := user.Append("INBOX", literal{bytes.NewReader(raw)}, &goimap.AppendOptions{Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	base := startIMAP(t, user)
	src := store.Source{AccountName: "mail", SourceConfig: store.SourceConfig{
		Address: "me@example.com", Host: "127.0.0.1", Port: base.Port, Auth: store.SourceAuthPlain, Username: "me@example.com",
	}}
	p, err := ProbeSource(t.Context(), base, src, "app-password")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range p.Folders {
		got = append(got, f.Name)
		if f.UIDValidity == 0 || f.UIDNext == 0 {
			t.Errorf("%s: no STATUS: %+v", f.Name, f.MailboxStatus)
		}
	}
	if !slices.Equal(got, []string{"Archive", "INBOX", "Reçus"}) || len(p.Missing) != 0 || len(p.Problems) != 0 {
		t.Errorf("folders %v, missing %v, problems %v", got, p.Missing, p.Problems)
	}
	if p.Folders[1].Messages != 1 {
		t.Errorf("INBOX has %d messages", p.Folders[1].Messages)
	}

	src.Mailboxes = []string{"INBOX", "Elsewhere"}
	src.Preset = store.PresetGmail
	p, err = ProbeSource(t.Context(), base, src, "app-password")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Missing, []string{"Elsewhere"}) || len(p.Problems) != 1 {
		t.Errorf("missing %v, problems %v", p.Missing, p.Problems)
	}

	_, err = ProbeSource(t.Context(), base, src, "wrong")
	var ie *imap.Error
	if !errors.As(err, &ie) || ie.Kind != imap.KindAuth {
		t.Errorf("a wrong password: %v", err)
	}
}

type literal struct{ *bytes.Reader }

func (l literal) Size() int64 { return l.Reader.Size() }

// startIMAP serves user over implicit TLS on 127.0.0.1 and returns the
// client configuration that trusts it.
func startIMAP(t *testing.T, user *imapmemserver.User) imap.Config {
	t.Helper()
	mem := imapmemserver.New()
	mem.AddUser(user)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps: goimap.CapSet{
			goimap.CapIMAP4rev1: {}, goimap.CapListExtended: {}, goimap.CapListStatus: {}, goimap.CapSpecialUse: {},
		},
	})
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "yuuka manage test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
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
	inner, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := tls.NewListener(inner, &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}}, MinVersion: tls.VersionTLS12,
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		<-done
	})
	return imap.Config{Port: inner.Addr().(*net.TCPAddr).Port, RootCAs: pool, IdleTimeout: 5 * time.Second}
}
