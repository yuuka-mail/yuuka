package main

import (
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/manage"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// newPrincipal creates the principal kind:name.
func (e *hostEnv) newPrincipal(kind, name string) store.Principal {
	e.t.Helper()
	now := time.Now()
	p := store.Principal{ID: ulid.New(now), Kind: kind, Name: name, DisplayName: name, CreatedAt: now}
	if err := e.db.CreatePrincipal(e.t.Context(), p, store.Action{ID: ulid.New(now), Tool: "principal.create", At: now}); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// openedSource opens the password of account name for the audience
// recomputed from its source, as pulling would.
func (e *hostEnv) openedSource(name string) string {
	e.t.Helper()
	src, err := e.db.SourceByName(e.t.Context(), name)
	if err != nil {
		e.t.Fatal(err)
	}
	s, err := loadSealer(func(k string) string { return e.env[k] })
	if err != nil {
		e.t.Fatal(err)
	}
	pw, err := manage.SourcePassword(e.t.Context(), e.db, s, src)
	if err != nil {
		e.t.Fatalf("open %s: %v", name, err)
	}
	return pw
}

// tableLines are the lines of a table with each run of spaces as one.
func tableLines(out string) []string {
	var lines []string
	for l := range strings.SplitSeq(out, "\n") {
		lines = append(lines, strings.Join(strings.Fields(l), " "))
	}
	return lines
}

func TestAccountCommands(t *testing.T) {
	t.Parallel()
	e := newHostEnv(t)
	ctx := t.Context()
	op := e.newPrincipal("human", "operator")
	pw1, pw2 := "abcd efgh ijkl mnop", "qrst uvwx yzab cdef"
	file1 := writeSecret(t, pw1)

	// A secret as an argument is refused before anything else.
	if _, _, err := e.runIn(nil, "account", "add", "mail", "--imap", "--preset", "gmail", "--address", "me@example.com",
		"--password", pw1); err == nil || !strings.Contains(err.Error(), "--password-file") || strings.Contains(err.Error(), pw1) {
		t.Errorf("password as an argument: %v", err)
	}
	// Without a KEK nothing can be sealed, and nothing is written.
	if _, _, err := e.runIn(nil, "account", "add", "mail", "--imap", "--preset", "gmail", "--address", "me@example.com",
		"--password-file", file1); err == nil || !strings.Contains(err.Error(), "YUUKA_KEK_FILE") {
		t.Errorf("without a KEK: %v", err)
	}
	e.env["YUUKA_KEK_FILE"] = writeKEK(t, "kek", 3)
	for _, bad := range [][]string{
		{"add", "mail", "--preset", "gmail", "--address", "me@example.com", "--password-file", file1},           // no --imap
		{"add", "mail", "--imap", "--address", "me@example.com", "--password-file", file1},                      // no host
		{"add", "mail", "--imap", "--preset", "gmail", "--password-file", file1},                                // no address
		{"add", "mail", "--imap", "--preset", "gmail", "--address", "me@example.com"},                           // no password
		{"add", "mail", "--imap", "--preset", "other", "--address", "me@example.com", "--password-file", file1}, // unknown preset
		{
			"add", "mail", "--imap", "--preset", "gmail", "--address", "me@example.com", "--password-file", file1,
			"--max-message-size", "64MiB",
		}, // larger than a raw object
		{
			"add", "mail", "--imap", "--preset", "gmail", "--address", "me@example.com", "--password-file", file1,
			"--poll", "1s",
		},
		{
			"add", "mail", "--imap", "--preset", "gmail", "--address", "me@example.com", "--password-file", file1,
			"--backfill", "soon",
		},
		{
			"add", "mail", "--imap", "--preset", "gmail", "--address", "me@example.com", "--password-file", file1,
			"--read", "agent:nobody",
		}, // readers must exist
		{"set", "mail", "--poll", "120s"}, // no such account
	} {
		if _, _, err := e.runIn(nil, "account", bad...); err == nil {
			t.Errorf("account %v: no error", bad)
		}
	}
	if n := e.secretCount(); n != 0 {
		t.Fatalf("%d secrets after failed adds", n)
	}

	// The Gmail preset fills in the server, the budget and the user name;
	// the only human reads it, and its address is claimed.
	arisu := e.newPrincipal("agent", "arisu")
	out, msg, err := e.runIn(nil, "account", "add", "mail", "--imap", "--preset", "gmail", "--address", "Me@Example.com",
		"--password-file", file1, "--backfill", "90", "--read", "human:operator,agent:arisu")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "added pull account mail") || !strings.Contains(out, "me@example.com@imap.gmail.com:993") ||
		!strings.Contains(msg, "Claimed me@example.com") || !strings.Contains(msg, "--trusted-authserv-id") {
		t.Errorf("add said %q, %q", out, msg)
	}
	src, err := e.db.SourceByName(ctx, "mail")
	if err != nil {
		t.Fatal(err)
	}
	if src.Host != manage.GmailHost || src.Port != 993 || src.Username != "me@example.com" || src.DailyByteBudget != 2<<30 ||
		src.Preset != store.PresetGmail || src.TrustedAuthservID != "" || src.MaxMessageBytes != manage.MaxMessageBytes ||
		src.BackfillSince == nil || time.Since(*src.BackfillSince) < 89*24*time.Hour {
		t.Errorf("source %+v", src)
	}
	if got := e.openedSource("mail"); got != pw1 {
		t.Errorf("sealed %q", got)
	}
	rows, err := e.db.AccountAccess(ctx, "mail")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || manage.RowOf(rows, op.ID) == nil || manage.RowOf(rows, arisu.ID) == nil {
		t.Errorf("access rows %+v", rows)
	}
	if pf, err := e.db.PrefixStats(ctx, (&host{}).viewer(auth.PrefixRead), store.PrefixQuery{Zone: "example.com", Prefix: "me"}); err != nil ||
		len(pf.Prefixes) != 1 || pf.Prefixes[0].State != store.PrefixClaimed {
		t.Errorf("prefix %+v, %v", pf, err)
	}
	_, _, args := e.lastAction("account.add")
	if args["audience"] != "imaps://me%40example.com@imap.gmail.com:993" || args["wrapper"] != "local" || args["key_ref"] == nil ||
		args["host_user"] != "tester" {
		t.Errorf("account.add args %v", args)
	}
	// The name is never reused, and another account cannot pull the
	// same mailbox.
	for _, again := range [][]string{
		{"add", "mail", "--imap", "--preset", "gmail", "--address", "other@example.com", "--password-file", file1},
		{"add", "mail2", "--imap", "--preset", "gmail", "--address", "me@example.com", "--password-file", file1},
	} {
		if _, _, err := e.runIn(nil, "account", again...); err == nil || !strings.Contains(err.Error(), "was not added") {
			t.Errorf("%v: %v", again, err)
		}
	}

	// set: tightening is recorded and not counted as a widening; another
	// host needs the password again; the authserv-id says reparse.
	e.mustIn(nil, "account", "set", "mail", "--poll", "5m", "--daily-budget", "1GiB")
	_, _, args = e.lastAction("account.set")
	if w, _ := args["widens"].([]any); len(w) != 0 {
		t.Errorf("tightening widens %v", w)
	}
	if out := e.mustIn(nil, "account", "set", "mail", "--poll", "5m"); !strings.Contains(out, "nothing changed") {
		t.Errorf("no change: %q", out)
	}
	if _, _, err := e.runIn(nil, "account", "set", "mail", "--host", "imap.example.net"); err == nil ||
		!strings.Contains(err.Error(), "password-file") {
		t.Errorf("moved without a password: %v", err)
	}
	_, msg, err = e.runIn(nil, "account", "set", "mail", "--trusted-authserv-id", "mx.example.net", "--mailboxes", "INBOX,Archive")
	if err != nil || !strings.Contains(msg, "yuuka reparse") {
		t.Errorf("authserv-id: %q, %v", msg, err)
	}
	_, _, args = e.lastAction("account.set")
	if w, _ := args["widens"].([]any); !slices.Equal(w, []any{manage.WidenAuthservID, manage.WidenMailboxes}) {
		t.Errorf("widens %v", w)
	}
	before := e.secretCount()
	e.mustIn(strings.NewReader(pw2+"\n"), "account", "set", "mail", "--host", "imap.example.net", "--password-file", "-")
	if got := e.openedSource("mail"); got != pw2 {
		t.Errorf("after moving: %q", got)
	}
	if n := e.secretCount(); n != before {
		t.Errorf("%d secrets, had %d: the old one stays", n, before)
	}
	e.noPlaintext(pw1, pw2)

	// ls: state, secret, folders. Without a key the secret is locked.
	out = e.mustIn(nil, "account", "ls")
	if !strings.Contains(out, "mail") || !strings.Contains(out, "enabled") || !strings.Contains(out, "INBOX,Archive") ||
		!strings.Contains(out, "5m0s") || !strings.Contains(out, "1GiB") || !strings.Contains(out, " ok ") ||
		!strings.Contains(out, "not synced yet") || strings.Contains(out, pw2) {
		t.Errorf("ls:\n%s", out)
	}
	if _, err := e.db.StartMailbox(ctx, src.AccountID, store.MailboxStart{Name: "INBOX", KeyKind: store.KeyLocation, UIDValidity: 7, High: 40}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.AdvanceLow(ctx, src.AccountID, "INBOX", 7, 12, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.RecordPullFailure(ctx, store.PullFailure{AccountID: src.AccountID, RemoteLocation: store.RemoteLocation{
		Mailbox: "INBOX", UIDValidity: 7, UID: 30,
	}, Size: -1, Detail: "NO", LastAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	out = e.mustIn(nil, "account", "ls")
	if !strings.Contains(out, "UID 40") || !strings.Contains(out, "down to UID 12") {
		t.Errorf("ls with a folder:\n%s", out)
	}
	if out := e.mustIn(nil, "secret", "ls"); !strings.Contains(out, "account mail") || !strings.Contains(out, "imap_password") {
		t.Errorf("secret ls:\n%s", out)
	}
	if out, err := e.check(); err != nil || !strings.Contains(out, "account mail") {
		t.Errorf("secret check: %v\n%s", err, out)
	}
	kek := e.env["YUUKA_KEK_FILE"]
	delete(e.env, "YUUKA_KEK_FILE")
	if out := e.mustIn(nil, "account", "ls"); !strings.Contains(out, "locked") {
		t.Errorf("ls without a key:\n%s", out)
	}

	// Without a key, everything that seals or opens nothing still works.
	if out := e.mustIn(nil, "account", "sync", "mail", "--retry-failed"); !strings.Contains(out, "yuuka serve") {
		t.Errorf("sync: %q", out)
	}
	req, err := e.db.TakeSyncRequest(ctx, src.AccountID)
	if err != nil || !req.Sync || !req.RetryFailed {
		t.Errorf("sync request %+v, %v", req, err)
	}
	e.mustIn(nil, "account", "pause", "mail")
	if out := e.mustIn(nil, "account", "ls"); !strings.Contains(out, "paused") {
		t.Errorf("ls paused:\n%s", out)
	}
	if out := e.mustIn(nil, "account", "pause", "mail"); !strings.Contains(out, "already") {
		t.Errorf("pause again: %q", out)
	}
	e.mustIn(nil, "account", "resume", "mail")
	if _, _, args := e.lastAction("account.resume"); args["pause_reason"] != store.PauseOperator {
		t.Errorf("resume args %v", args)
	}
	if out := e.mustIn(nil, "account", "resume", "mail"); !strings.Contains(out, "not paused") {
		t.Errorf("resume again: %q", out)
	}

	// access: list, a row each way, back to the default, the default.
	out = e.mustIn(nil, "account", "access", "mail")
	if lines := tableLines(out); !slices.Contains(lines, "(default) no") || !slices.Contains(lines, "agent:arisu yes") {
		t.Errorf("access:\n%s", out)
	}
	e.mustIn(nil, "account", "access", "mail", "--principal", "agent:arisu", "--no-read")
	if _, _, args := e.lastAction("account.access"); args["widens"] != false || args["can_read"] != false {
		t.Errorf("--no-read args %v", args)
	}
	e.mustIn(nil, "account", "access", "mail", "--principal", "service:monitoring", "--read")
	if _, _, args := e.lastAction("account.access"); args["widens"] != true {
		t.Errorf("--read args %v", args)
	}
	e.mustIn(nil, "account", "access", "mail", "--principal", "agent:arisu", "--default")
	rows, err = e.db.AccountAccess(ctx, "mail")
	if err != nil {
		t.Fatal(err)
	}
	if manage.RowOf(rows, arisu.ID) != nil {
		t.Errorf("arisu still has a row: %+v", rows)
	}
	e.mustIn(nil, "account", "access", "mail", "--default-read", "yes")
	if _, _, args := e.lastAction("account.access"); args["widens"] != true || args["default_read"] != true {
		t.Errorf("--default-read args %v", args)
	}
	for _, bad := range [][]string{
		{"access", "mail", "--principal", "agent:arisu"},
		{"access", "mail", "--read"},
		{"access", "mail", "--principal", "agent:arisu", "--read", "--no-read"},
		{"access", "mail", "--default-read", "maybe"},
		{"access", "nowhere"},
	} {
		if _, _, err := e.runIn(nil, "account", bad...); err == nil {
			t.Errorf("account %v: no error", bad)
		}
	}

	// rm: the password is erased, the account and its name stay.
	e.env["YUUKA_KEK_FILE"] = kek
	e.mustIn(nil, "account", "rm", "mail")
	if n := e.secretCount(); n != before-1 {
		t.Errorf("%d secrets after rm, had %d", n, before)
	}
	if out := e.mustIn(nil, "account", "ls"); strings.Contains(out, "mail") {
		t.Errorf("ls lists a removed account:\n%s", out)
	}
	if out := e.mustIn(nil, "account", "ls", "--all"); !strings.Contains(out, "removed") {
		t.Errorf("ls --all:\n%s", out)
	}
	for _, verb := range []string{"set", "pause", "resume", "rm", "test", "sync"} {
		args := []string{verb, "mail"}
		if verb == "set" {
			args = append(args, "--poll", "60s")
		}
		if _, _, err := e.runIn(nil, "account", args...); err == nil || !strings.Contains(err.Error(), "removed") {
			t.Errorf("%s a removed account: %v", verb, err)
		}
	}
}

func TestAccountTest(t *testing.T) {
	t.Parallel()
	e := newHostEnv(t)
	e.newPrincipal("human", "operator")
	e.env["YUUKA_KEK_FILE"] = writeKEK(t, "kek", 9)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	e.mustIn(nil, "account", "add", "local", "--imap", "--host", "127.0.0.1", "--port", strconv.Itoa(port),
		"--address", "me@example.com", "--password-file", writeSecret(t, "pw"))
	if _, err := e.db.EnsureCloudflareAccount(t.Context(), "cloudflare"); err != nil {
		t.Fatal(err)
	}

	// The secret opens, so the test gets as far as connecting.
	if _, _, err := e.runIn(nil, "account", "test", "local"); err == nil || !strings.Contains(err.Error(), "connect") {
		t.Errorf("closed port: %v", err)
	}
	if _, _, err := e.runIn(nil, "account", "test", "cloudflare"); err == nil || !strings.Contains(err.Error(), "not a pull account") {
		t.Errorf("a Cloudflare account: %v", err)
	}
	delete(e.env, "YUUKA_KEK_FILE")
	if _, _, err := e.runIn(nil, "account", "test", "local"); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Errorf("without a KEK: %v", err)
	}
}

func TestParseSize(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]int64{"0": 0, "4096": 4096, "512KiB": 512 << 10, "32MiB": 32 << 20, "2GiB": 2 << 30, "7B": 7} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("%s: %d, %v", in, got, err)
		}
		if in != "7B" && in != "4096" {
			if got := formatSize(want); got != in {
				t.Errorf("formatSize(%d) = %s", want, got)
			}
		}
	}
	for _, bad := range []string{"", "-1", "2GB", "1.5MiB", "MiB", "99999999999GiB"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
