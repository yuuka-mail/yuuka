package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
)

// writeKEK writes a KEK file of 32 bytes counting up from fill.
func writeKEK(t *testing.T, name string, fill byte) string {
	t.Helper()
	k := make([]byte, 32)
	for i := range k {
		k[i] = fill + byte(i)
	}
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(base64.StdEncoding.EncodeToString(k)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeSecret writes a password file as an editor leaves it.
func writeSecret(t *testing.T, value string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(p, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// runIn runs the commands this file adds, with in as standard input.
func (e *hostEnv) runIn(in io.Reader, cmd string, args ...string) (string, string, error) {
	e.t.Helper()
	var out, msg bytes.Buffer
	h := &host{db: e.db, out: &out, msg: &msg, in: in, now: time.Now, user: "tester", getenv: func(k string) string { return e.env[k] }}
	var err error
	switch cmd {
	case "transport":
		err = h.transportCmd(e.t.Context(), args)
	case "secret":
		err = h.secretCmd(e.t.Context(), args)
	case "identity":
		err = h.identityCmd(e.t.Context(), args)
	default:
		e.t.Fatalf("no command %s", cmd)
	}
	return out.String(), msg.String(), err
}

func (e *hostEnv) mustIn(in io.Reader, cmd string, args ...string) string {
	e.t.Helper()
	out, _, err := e.runIn(in, cmd, args...)
	if err != nil {
		e.t.Fatalf("%s %v: %v", cmd, args, err)
	}
	return out
}

// opened opens the secret of transport name as sending would, with the
// sealer the environment names.
func (e *hostEnv) opened(name string) string {
	e.t.Helper()
	ctx := e.t.Context()
	tr, err := e.db.TransportByName(ctx, name)
	if err != nil {
		e.t.Fatal(err)
	}
	st, err := e.db.Secret(ctx, tr.SecretID)
	if err != nil {
		e.t.Fatal(err)
	}
	s, err := loadSealer(func(k string) string { return e.env[k] })
	if err != nil {
		e.t.Fatal(err)
	}
	aud, err := secret.SMTPS(tr.Username, tr.Host, tr.Port)
	if err != nil {
		e.t.Fatal(err)
	}
	v, err := s.Open(ctx, st.Record, aud)
	if err != nil {
		e.t.Fatalf("open %s: %v", name, err)
	}
	defer v.Close()
	return string(v.Reveal())
}

// noPlaintext fails if any audit row mentions one of the secrets.
func (e *hostEnv) noPlaintext(secrets ...string) {
	e.t.Helper()
	var all string
	if err := e.pool.QueryRow(e.t.Context(), `SELECT coalesce(string_agg(args::text || coalesce(result::text, ''), ' '), '') FROM actions`).Scan(&all); err != nil {
		e.t.Fatal(err)
	}
	for _, s := range secrets {
		if strings.Contains(all, s) {
			e.t.Errorf("the audit log contains the secret %q", s)
		}
	}
}

func (e *hostEnv) secretCount() int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.t.Context(), `SELECT count(*) FROM secrets`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// bumpOnRead changes transport name in the database when the command
// reads the password, i.e. between its read of the row and its write.
type bumpOnRead struct {
	e    *hostEnv
	name string
	r    io.Reader
	done bool
}

func (b *bumpOnRead) Read(p []byte) (int, error) {
	if !b.done {
		b.done = true
		if _, err := b.e.pool.Exec(context.Background(), `UPDATE transports SET revision = revision + 1 WHERE name = $1`, b.name); err != nil {
			return 0, err
		}
	}
	return b.r.Read(p)
}

func TestTransportCommands(t *testing.T) {
	t.Parallel()
	e := newHostEnv(t)
	ctx := t.Context()
	pw1, pw2, pw3 := "correct horse battery", "stdin staple", "moved password"
	file1 := writeSecret(t, pw1)

	// A secret as an argument is refused before anything else, without
	// repeating it.
	for _, bad := range [][]string{
		{"add", "work", "--host", "smtp.example.com", "--username", "u", "--password", pw1, "--caps", "none"},
		{"set", "work", "--password=" + pw1},
	} {
		_, _, err := e.runIn(nil, "transport", bad...)
		if err == nil || !strings.Contains(err.Error(), "--password-file") || strings.Contains(err.Error(), pw1) {
			t.Errorf("%v: %v", bad, err)
		}
	}
	// A path that is not a file is not repeated either: it may have been
	// the password.
	e.env["YUUKA_KEK_FILE"] = writeKEK(t, "kek", 1)
	if _, _, err := e.runIn(nil, "transport", "add", "work", "--host", "smtp.example.com", "--username", "u",
		"--password-file", pw1, "--caps", "none"); err == nil || strings.Contains(err.Error(), pw1) {
		t.Errorf("password as a path: %v", err)
	}
	for _, bad := range [][]string{
		{"add", "work", "--host", "smtp.example.com", "--username", "u", "--caps", "none"},                   // no password
		{"add", "work", "--host", "smtp.example.com", "--username", "u", "--password-file", file1},           // no caps
		{"add", "work", "--username", "u", "--password-file", file1, "--caps", "none"},                       // no host
		{"add", "work", "--host", "smtp.example.com", "--password-file", file1, "--caps", "none"},            // password without user
		{"add", "work", "--host", "smtp.example.com.", "--caps", "none"},                                     // trailing dot
		{"add", "work", "--host", "smtp.example.com", "--caps", "dkim,bogus"},                                // unknown cap
		{"add", "work", "--host", "smtp.example.com", "--username", "u", "--auth", "none", "--caps", "none"}, // user without auth
		{"set", "work", "--caps", "none"},                                                                    // no such transport
	} {
		if _, _, err := e.runIn(nil, "transport", bad...); err == nil {
			t.Errorf("transport %v: no error", bad)
		}
	}
	// Without a KEK nothing can be sealed.
	kek := e.env["YUUKA_KEK_FILE"]
	delete(e.env, "YUUKA_KEK_FILE")
	if _, _, err := e.runIn(nil, "transport", "add", "work", "--host", "smtp.example.com", "--username", "u",
		"--password-file", file1, "--caps", "none"); err == nil || !strings.Contains(err.Error(), "YUUKA_KEK_FILE") {
		t.Errorf("without a KEK: %v", err)
	}
	e.env["YUUKA_KEK_FILE"] = kek
	if n := e.secretCount(); n != 0 {
		t.Fatalf("%d secrets after failed adds", n)
	}

	// From a file: the host is stored canonical, the password sealed for
	// exactly that destination.
	out := e.mustIn(nil, "transport", "add", "work", "--host", "SMTP.Example.com", "--username", "u",
		"--password-file", file1, "--caps", "standard")
	if !strings.Contains(out, "configured transport work") || !strings.Contains(out, "u@smtp.example.com:465") {
		t.Errorf("add: %q", out)
	}
	work, err := e.db.TransportByName(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	if work.Host != "smtp.example.com" || work.Port != 465 || work.Auth != "plain" || work.Username != "u" ||
		!slices.Equal(work.Caps, []string{"message-id", "return-path"}) || work.ConfiguredAt == nil || work.SecretID == "" {
		t.Errorf("work %+v", work)
	}
	if got := e.opened("work"); got != pw1 {
		t.Errorf("work opens to %q", got)
	}
	actor, cred, args := e.lastAction("transport.add")
	if actor != store.SystemPrincipalID || cred != nil || args["via"] != "cli" || args["host_user"] != "tester" ||
		args["audience"] != "smtps://u@smtp.example.com:465" || args["wrapper"] != "local" || args["key_ref"] == "" ||
		args["secret_id"] != work.SecretID || args["host"] != "smtp.example.com" {
		t.Errorf("transport.add: %s %v %v", actor, cred, args)
	}
	if _, _, err := e.runIn(nil, "transport", "add", "work", "--host", "smtp.example.com", "--caps", "none"); err == nil ||
		!strings.Contains(err.Error(), "transport set") {
		t.Errorf("add twice: %v", err)
	}

	// From standard input, and onto a placeholder.
	storetest.Transports(t, e.db, "relay")
	e.mustIn(strings.NewReader(pw2+"\n"), "transport", "add", "relay", "--host", "relay.example.com", "--port", "2465",
		"--username", "bot@example.com", "--password-file", "-", "--caps", "none")
	if got := e.opened("relay"); got != pw2 {
		t.Errorf("relay opens to %q", got)
	}

	// Changing what is not the destination keeps the secret.
	e.mustIn(nil, "transport", "set", "work", "--caps", "cloudflare")
	again, _ := e.db.TransportByName(ctx, "work")
	if again.SecretID != work.SecretID || len(again.Caps) != 0 || again.Revision != work.Revision+1 {
		t.Errorf("caps only: %+v", again)
	}
	// Moving it needs the password again, and seals it under a new id.
	if _, _, err := e.runIn(nil, "transport", "set", "work", "--host", "other.example.com"); err == nil ||
		!strings.Contains(err.Error(), "--password-file") {
		t.Errorf("move without a password: %v", err)
	}
	e.mustIn(nil, "transport", "set", "work", "--host", "other.example.com", "--password-file", writeSecret(t, pw3))
	moved, _ := e.db.TransportByName(ctx, "work")
	if moved.SecretID == again.SecretID || moved.Host != "other.example.com" {
		t.Errorf("moved %+v", moved)
	}
	if _, err := e.db.Secret(ctx, again.SecretID); err == nil {
		t.Error("the old secret survived the move")
	}
	if got := e.opened("work"); got != pw3 {
		t.Errorf("moved work opens to %q", got)
	}
	_, _, args = e.lastAction("transport.set")
	if was, _ := args["was"].(map[string]any); was["host"] != "smtp.example.com" || args["audience"] != "smtps://u@other.example.com:465" {
		t.Errorf("transport.set %v", args)
	}

	// A change between the read and the write is an error, and nothing
	// is written.
	before := e.secretCount()
	_, _, err = e.runIn(&bumpOnRead{e: e, name: "work", r: strings.NewReader("racing password")}, "transport", "set", "work",
		"--password-file", "-")
	if err == nil || !strings.Contains(err.Error(), "changed while this command ran") {
		t.Errorf("race: %v", err)
	}
	if n := e.secretCount(); n != before {
		t.Errorf("%d secrets after a refused write, %d before", n, before)
	}

	out = e.mustIn(nil, "transport", "ls")
	for _, want := range []string{"work", "relay", "configured", "other.example.com", "2465", "bot@example.com", "none", "local:"} {
		if !strings.Contains(out, want) {
			t.Errorf("ls lacks %q:\n%s", want, out)
		}
	}

	// Identities route through the chain.
	storetest.Account(t, e.db)
	e.must("identity", "add", "alerts@example.com", "--owner", "service:monitoring", "--transport", "work")
	if _, _, err := e.runIn(nil, "identity", "set", "alerts@example.com", "--transports", "work,nope"); err == nil {
		t.Error("unknown transport in the chain")
	}
	if out := e.mustIn(nil, "identity", "set", "alerts@example.com", "--transports", "relay, work"); !strings.Contains(out, "relay, then work") {
		t.Errorf("identity set: %q", out)
	}
	i, err := e.db.IdentityAt(ctx, "example.com", "alerts")
	if err != nil || !slices.Equal(i.Transports, []string{"relay", "work"}) {
		t.Fatalf("chain %v %v", i.Transports, err)
	}
	_, _, args = e.lastAction("identity.route")
	if args["identity_id"] != i.ID || args["via"] != "cli" {
		t.Errorf("identity.route %v", args)
	}

	// rm erases the secret and keeps the row.
	_, notes, err := e.runIn(nil, "transport", "rm", "work")
	if err != nil || !strings.Contains(notes, "alerts@example.com still names it") {
		t.Errorf("rm: %q %v", notes, err)
	}
	gone, _ := e.db.TransportByName(ctx, "work")
	if gone.DisabledAt == nil || gone.SecretID != "" {
		t.Errorf("removed %+v", gone)
	}
	if _, _, err := e.runIn(nil, "transport", "rm", "work"); err == nil {
		t.Error("rm twice")
	}
	if out := e.mustIn(nil, "transport", "ls"); strings.Contains(out, "work") {
		t.Errorf("ls shows the removed one:\n%s", out)
	}
	if out := e.mustIn(nil, "transport", "ls", "--all"); !strings.Contains(out, "disabled") {
		t.Errorf("ls --all:\n%s", out)
	}

	e.noPlaintext(pw1, pw2, pw3, "racing password")
}

func TestTransportTest(t *testing.T) {
	t.Parallel()
	e := newHostEnv(t)
	e.env["YUUKA_KEK_FILE"] = writeKEK(t, "kek", 7)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	storetest.Transports(t, e.db, "placeholder")
	e.mustIn(nil, "transport", "add", "local", "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--username", "u",
		"--password-file", writeSecret(t, "pw"), "--caps", "none")

	if _, _, err := e.runIn(nil, "transport", "test", "placeholder"); err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Errorf("placeholder: %v", err)
	}
	// The secret opens, so the probe gets as far as connecting.
	if _, _, err := e.runIn(nil, "transport", "test", "local"); err == nil || !strings.Contains(err.Error(), "connect") {
		t.Errorf("closed port: %v", err)
	}
	delete(e.env, "YUUKA_KEK_FILE")
	if _, _, err := e.runIn(nil, "transport", "test", "local"); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Errorf("without a KEK: %v", err)
	}
}
