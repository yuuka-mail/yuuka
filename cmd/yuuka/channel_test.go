package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

const testBotToken = "1001:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// fakeBotAPI answers getMe for bot 1001 (@yuuka_bot) and records what is
// sent. It points telegramAPI at itself for the test, which therefore
// cannot be parallel.
type fakeBotAPI struct {
	mu   sync.Mutex
	sent []map[string]any
}

func newFakeBotAPI(t *testing.T) *fakeBotAPI {
	t.Helper()
	f := &fakeBotAPI{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	old := telegramAPI
	telegramAPI = srv.URL
	t.Cleanup(func() { telegramAPI = old })
	return f
}

func (f *fakeBotAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, method, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/bot"), "/")
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	var result any = true
	switch {
	case !strings.HasPrefix(token, "1001:"):
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Unauthorized"}`))
		return
	case method == "getMe":
		result = map[string]any{"id": 1001, "username": "yuuka_bot"}
	case method == "sendMessage":
		f.mu.Lock()
		f.sent = append(f.sent, body)
		f.mu.Unlock()
		result = map[string]any{"message_id": 1}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func (f *fakeBotAPI) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, b := range f.sent {
		out = append(out, b["text"].(string))
	}
	return out
}

// syncBuf is a buffer the command writes while the test reads it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (e *hostEnv) channel(in io.Reader, out, msg io.Writer, args ...string) error {
	h := &host{db: e.db, out: out, msg: msg, in: in, now: time.Now, user: "tester", getenv: func(k string) string { return e.env[k] }}
	return h.channelCmd(e.t.Context(), args)
}

func (e *hostEnv) mustChannel(in io.Reader, args ...string) string {
	e.t.Helper()
	var out, msg bytes.Buffer
	if err := e.channel(in, &out, &msg, args...); err != nil {
		e.t.Fatalf("channel %v: %v\n%s", args, err, msg.String())
	}
	return out.String()
}

func waitUntil(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The host's side of channels: add asks Telegram which bot the token is
// and seals it; bind shows who sent /start and binds only on yes; the
// rest lists, subscribes, unbinds and removes.
func TestChannelCommands(t *testing.T) {
	api := newFakeBotAPI(t)
	old := bindPoll
	bindPoll = 10 * time.Millisecond
	t.Cleanup(func() { bindPoll = old })
	e := newHostEnv(t)
	ctx := t.Context()
	alice, err := e.db.EnsurePrincipal(ctx, store.Principal{ID: ulid.New(time.Now()), Kind: "human", Name: "alice", DisplayName: "alice", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}

	if err := e.channel(strings.NewReader(testBotToken), io.Discard, io.Discard, "add", "tg", "--token-file", "-"); err == nil ||
		!strings.Contains(err.Error(), "YUUKA_KEK_FILE") {
		t.Errorf("add without a KEK: %v", err)
	}
	e.env["YUUKA_KEK_FILE"] = writeKEK(t, "kek", 5)
	if err := e.channel(strings.NewReader("1002:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"), io.Discard, io.Discard, "add", "tg", "--token-file", "-"); err == nil {
		t.Error("a token Telegram refuses was added")
	}
	if out := e.mustChannel(strings.NewReader(testBotToken+"\n"), "add", "tg", "--token-file", "-"); !strings.Contains(out, "bot @yuuka_bot (id 1001)") {
		t.Errorf("add: %s", out)
	}
	var args string
	if err := e.pool.QueryRow(ctx, `SELECT args::text FROM actions WHERE tool = 'channel.add'`).Scan(&args); err != nil ||
		strings.Contains(args, "AAAAAAAA") || !strings.Contains(args, `"audience": "telegram:bot1001"`) || !strings.Contains(args, `"via": "cli"`) {
		t.Errorf("channel.add audit %s %v", args, err)
	}
	if out := e.mustChannel(nil, "ls"); !strings.Contains(out, "@yuuka_bot (1001)") {
		t.Errorf("ls: %s", out)
	}

	// bind runs until someone sends the code to the bot (here: the test,
	// as serve's poller would) and the person answers.
	bind := func(seen store.Seen, answer string) (string, error) {
		t.Helper()
		in, write := io.Pipe()
		var out, msg syncBuf
		done := make(chan error, 1)
		go func() { done <- e.channel(in, &out, &msg, "bind", "tg", "--events", "send.held,mail.new") }()
		code := regexp.MustCompile(`start=([A-Za-z0-9_-]+)`)
		waitUntil(t, func() bool { return code.MatchString(msg.String()) })
		c, err := e.db.ChannelByName(ctx, "tg")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.db.RedeemBindCode(ctx, c.ID, code.FindStringSubmatch(msg.String())[1], seen, time.Now()); err != nil {
			t.Fatal(err)
		}
		waitUntil(t, func() bool { return strings.Contains(msg.String(), "[y/N]") })
		if !strings.Contains(msg.String(), seen.Label) || !strings.Contains(msg.String(), "user id "+seen.ActorID+", in chat "+seen.Address) {
			t.Errorf("bind does not show who sent /start:\n%s", msg.String())
		}
		_, _ = write.Write([]byte(answer + "\n"))
		err = <-done
		return out.String(), err
	}
	// Someone else got hold of the link: the person sees a stranger and
	// says no; nothing is bound and the code is void.
	out, err := bind(store.Seen{Address: "-100", ActorID: "666", Label: "Mallory (@mallory) in Not yours", ChatKind: "group"}, "n")
	if err != nil || !strings.Contains(out, "not bound") {
		t.Errorf("declined: %q %v", out, err)
	}
	if bs, _ := e.db.Bindings(ctx, true); len(bs) != 0 {
		t.Errorf("bound after no: %+v", bs)
	}
	out, err = bind(store.Seen{Address: "42", ActorID: "7", Label: "Alice", ChatKind: "private"}, "y")
	if err != nil || !strings.Contains(out, "bound ") {
		t.Fatalf("confirmed: %q %v", out, err)
	}
	bs, err := e.db.Bindings(ctx, false)
	if err != nil || len(bs) != 1 || bs[0].PrincipalID != alice.ID || bs[0].Address != "42" || bs[0].ActorID != "7" ||
		!slices.Equal(bs[0].Events, []string{store.EventMailNew, store.EventSendHeld}) {
		t.Fatalf("bindings %+v %v", bs, err)
	}
	b := bs[0]
	// The chat is told; the security alert goes to alice's other
	// bindings, of which there are none, never to the new one.
	if got := api.texts(); len(got) != 1 || !strings.Contains(got[0], "bound to human:alice") {
		t.Errorf("sent %q", got)
	}

	if out := e.mustChannel(nil, "subscribe", b.ID, "grant.suspended"); !strings.Contains(out, "mail.new,send.held,grant.suspended") {
		t.Errorf("subscribe: %s", out)
	}
	if err := e.channel(nil, io.Discard, io.Discard, "subscribe", b.ID, "security"); err == nil {
		t.Error("subscribed to security")
	}
	if out := e.mustChannel(nil, "unsubscribe", b.ID, "mail.new", "send.held"); !strings.Contains(out, "receives: grant.suspended") {
		t.Errorf("unsubscribe: %s", out)
	}
	if out := e.mustChannel(nil, "test", "tg"); !strings.Contains(out, "1 binding") {
		t.Errorf("test: %s", out)
	}
	sec, _, err := e.runIn(nil, "secret", "check")
	if err != nil || !strings.Contains(sec, "channel tg") {
		t.Errorf("secret check: %q %v", sec, err)
	}
	e.mustChannel(nil, "unbind", b.ID)
	if out := e.mustChannel(nil, "ls", "--all"); !strings.Contains(out, b.ID) {
		t.Errorf("ls --all: %s", out)
	}
	e.mustChannel(nil, "rm", "tg")
	if cs, err := e.db.Channels(ctx, false); err != nil || len(cs) != 0 {
		t.Errorf("channels after rm %v %v", cs, err)
	}
}

// serve's import of YUUKA_TELEGRAM_*: one channel and one binding with
// the button user, recorded as config.import via env, once; an imported
// bot makes the database the source of notifications.
func TestImportEnvTelegram(t *testing.T) {
	newFakeBotAPI(t)
	e := newHostEnv(t)
	ctx := t.Context()
	h := &host{db: e.db, getenv: func(k string) string { return map[string]string{"YUUKA_KEK_FILE": writeKEK(t, "kek", 9)}[k] }}
	sealer, err := h.sealer()
	if err != nil {
		t.Fatal(err)
	}
	a := &app{db: e.db, cfg: config{TelegramToken: testBotToken, TelegramChat: "42", TelegramOperatorID: 7, TelegramOperator: "human:alice"}}
	log := slog.New(slog.DiscardHandler)
	// Without the operator there is nobody to bind the chat to: nothing
	// is imported, and the variables keep working.
	if err := a.importTelegram(ctx, log, sealer); err != nil {
		t.Fatal(err)
	}
	if in, err := a.envBotIsChannel(ctx); err != nil || in {
		t.Fatalf("imported without its person: %v %v", in, err)
	}
	if _, err := e.db.EnsurePrincipal(ctx, store.Principal{ID: ulid.New(time.Now()), Kind: "human", Name: "alice", DisplayName: "alice", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := a.importTelegram(ctx, log, sealer); err != nil {
			t.Fatal(err)
		}
	}
	if in, err := a.envBotIsChannel(ctx); err != nil || !in {
		t.Fatalf("not imported: %v %v", in, err)
	}
	c, err := e.db.ChannelByName(ctx, importedChannel)
	if err != nil || c.Settings["username"] != "yuuka_bot" || c.ExternalID != "1001" {
		t.Errorf("channel %+v %v", c, err)
	}
	bs, err := e.db.Bindings(ctx, false)
	if err != nil || len(bs) != 1 || bs[0].PrincipalName != "alice" || bs[0].Address != "42" || bs[0].ActorID != "7" ||
		!slices.Equal(bs[0].Events, store.Events) {
		t.Errorf("binding %+v %v", bs, err)
	}
	var n int
	var args string
	if err := e.pool.QueryRow(ctx, `SELECT count(*), min(args::text) FROM actions WHERE tool = 'config.import' AND actor = $1`,
		store.SystemPrincipalID).Scan(&n, &args); err != nil || n != 1 || !strings.Contains(args, `"via": "env"`) || strings.Contains(args, "AAAAAAAA") {
		t.Errorf("%d config.import actions: %s %v", n, args, err)
	}
	if sealed, err := e.db.Secret(ctx, c.SecretID); err != nil || bytes.Contains(sealed.Payload, []byte("AAAAAAAA")) {
		t.Errorf("token stored in the clear: %v", err)
	}
}

// Widenings made on the host by commands without a dispatcher of their
// own reach the administrators' chats (§17.6); narrowing does not.
func TestWideningAlerts(t *testing.T) {
	api := newFakeBotAPI(t)
	e := newHostEnv(t)
	ctx := t.Context()
	e.env["YUUKA_KEK_FILE"] = writeKEK(t, "kek", 9)
	storetest.Account(t, e.db)
	alice, err := e.db.EnsurePrincipal(ctx, store.Principal{ID: ulid.New(time.Now()), Kind: "human", Name: "alice", DisplayName: "alice", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	e.mustChannel(strings.NewReader(testBotToken), "add", "tg", "--token-file", "-")
	c, err := e.db.ChannelByName(ctx, "tg")
	if err != nil {
		t.Fatal(err)
	}
	h := &host{db: e.db, now: time.Now, user: "tester", getenv: func(k string) string { return e.env[k] }}
	act := func(tool string) store.Action { return h.action(tool, nil) }
	code, err := e.db.NewBindCode(ctx, c.ID, alice.ID, "", "widening-alert-code", act("channel.bind"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.RedeemBindCode(ctx, c.ID, "widening-alert-code", store.Seen{Address: "42", ActorID: "7", Label: "Alice", ChatKind: "private"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.ConfirmBindCode(ctx, code.ID, "", nil, act("channel.bind.confirm")); err != nil {
		t.Fatal(err)
	}
	sent := func() []string { t.Helper(); return api.texts() }
	before := len(sent())

	pw := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(pw, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.mustIn(nil, "transport", "add", "work", "--host", "smtp.example.com", "--username", "u", "--password-file", pw, "--caps", "standard")
	e.mustIn(nil, "transport", "add", "spare", "--host", "smtp2.example.com", "--auth", "none", "--caps", "none")
	got := sent()[before:]
	if len(got) != 2 || !strings.Contains(got[0], "transport work was changed, with a new password, on the host by tester: u@smtp.example.com:465") ||
		strings.Contains(strings.Join(got, "\n"), "secret") {
		t.Fatalf("transport alerts %q", got)
	}

	e.must("identity", "add", "*@example.com", "--owner", "human:alice", "--transport", "work")
	before = len(sent())
	e.must("identity", "set", "*@example.com", "--transports", "work,spare")
	if got := sent()[before:]; len(got) != 1 || !strings.Contains(got[0], "now sends through work, then spare (was work)") {
		t.Errorf("widening the chain: %q", got)
	}
	before = len(sent())
	e.must("identity", "set", "*@example.com", "--transports", "work")
	if got := sent()[before:]; len(got) != 0 {
		t.Errorf("dropping a fallback alerted: %q", got)
	}
}
