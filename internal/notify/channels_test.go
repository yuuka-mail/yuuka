package notify

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// bots is a fake Bot API for several bots at once: it records every
// call by token and hands each bot its queued updates once.
type bots struct {
	mu      sync.Mutex
	calls   []call
	updates map[string][]Update
}

type call struct {
	token, method string
	body          map[string]any
}

func (b *bots) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, method, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/bot"), "/")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	b.mu.Lock()
	b.calls = append(b.calls, call{token, method, body})
	var result any = true
	idle := false
	switch method {
	case "sendMessage":
		result = map[string]any{"message_id": 77}
	case "getUpdates":
		us := b.updates[token]
		delete(b.updates, token)
		idle = len(us) == 0
		result = append([]Update{}, us...)
	}
	b.mu.Unlock()
	if idle {
		time.Sleep(20 * time.Millisecond) // a long poll that ends without updates
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

// sent is what was sent to chat through token, in order.
func (b *bots) sent(token, chat string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, c := range b.calls {
		if c.token == token && c.method == "sendMessage" && c.body["chat_id"] == chat {
			out = append(out, c.body["text"].(string))
		}
	}
	return out
}

func (b *bots) count(token, method string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, c := range b.calls {
		if (token == "" || c.token == token) && c.method == method {
			n++
		}
	}
	return n
}

func (b *bots) all(method string) []call {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []call
	for _, c := range b.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

func (b *bots) queue(token string, us ...Update) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.updates == nil {
		b.updates = map[string][]Update{}
	}
	b.updates[token] = append(b.updates[token], us...)
}

// channelFixture is a database with channels whose tokens are really
// sealed, and a fake Bot API.
type channelFixture struct {
	t      *testing.T
	db     *store.DB
	pool   *pgxpool.Pool
	sealer *secret.Sealer
	api    *bots
	url    string
}

func newChannelFixture(t *testing.T) *channelFixture {
	t.Helper()
	db, pool := storetest.New(t)
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	key, err := secret.ParseKey([]byte(base64.StdEncoding.EncodeToString(kek)))
	if err != nil {
		t.Fatal(err)
	}
	ring, err := secret.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := secret.NewSealer(ring)
	if err != nil {
		t.Fatal(err)
	}
	api := &bots{}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	return &channelFixture{t: t, db: db, pool: pool, sealer: sealer, api: api, url: srv.URL}
}

func (f *channelFixture) dispatcher(sealer *secret.Sealer) *Dispatcher {
	return NewDispatcher(DispatcherConfig{DB: f.db, Sealer: sealer, BaseURL: f.url})
}

func (f *channelFixture) action(tool string) store.Action {
	now := time.Now()
	return store.Action{ID: ulid.New(now), Actor: store.SystemPrincipalID, Tool: tool, At: now}
}

func (f *channelFixture) human(name string) string {
	f.t.Helper()
	p, err := f.db.EnsurePrincipal(f.t.Context(), store.Principal{ID: ulid.New(time.Now()), Kind: "human", Name: name, DisplayName: name, CreatedAt: time.Now()})
	if err != nil {
		f.t.Fatal(err)
	}
	return p.ID
}

// channel adds channel name for bot, whose token is "<bot>:AAAA...".
func (f *channelFixture) channel(name string, bot int64) (store.Channel, string) {
	f.t.Helper()
	ctx := f.t.Context()
	token := itoa(bot) + ":AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	aud, _ := secret.TelegramBot(bot)
	sealed, err := f.sealer.Seal(ctx, ulid.New(time.Now()), store.SecretTelegramBotToken, aud, []byte(token))
	if err != nil {
		f.t.Fatal(err)
	}
	c, err := f.db.ConfigureChannel(ctx, store.ChannelConfig{Name: name, Kind: store.ChannelTelegram, ExternalID: itoa(bot)},
		0, &sealed, f.action("channel.add"))
	if err != nil {
		f.t.Fatal(err)
	}
	return c, token
}

func (f *channelFixture) bind(c store.Channel, principal, chat, user string, events ...string) store.Binding {
	f.t.Helper()
	ctx := f.t.Context()
	code := ulid.New(time.Now()) + "-code"
	bc, err := f.db.NewBindCode(ctx, c.ID, principal, "", code, f.action("channel.bind.begin"))
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.db.RedeemBindCode(ctx, c.ID, code, store.Seen{Address: chat, ActorID: user}, time.Now()); err != nil {
		f.t.Fatal(err)
	}
	b, err := f.db.ConfirmBindCode(ctx, bc.ID, "", events, f.action("channel.bind.confirm"))
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

// delivery commits one delivery and returns its id.
func (f *channelFixture) delivery() string {
	f.t.Helper()
	ctx := f.t.Context()
	acct := storetest.Account(f.t, f.db)
	raw := ulid.New(time.Now())
	d := ulid.New(time.Now())
	if _, err := f.db.CommitIngest(ctx, store.Ingest{
		RawID: raw, R2Key: "raw/2026/11/" + raw + ".eml", LocalPath: "raw/2026/11/" + raw + ".eml", Size: 1,
		SHA256: sha256.Sum256([]byte(raw)), ContentHash: sha256.Sum256([]byte(raw)), ReceivedAt: time.Now(),
		DeliveryID: d, AccountID: acct, Zone: "example.com", Prefix: "a", EnvelopeFrom: "x@example.net", EnvelopeTo: "a@example.com",
	}); err != nil {
		f.t.Fatal(err)
	}
	return d
}

// Events go to the live bindings subscribed to them, read on every
// event: a revocation counts from the next one. mail.new goes only where
// the delivery is visible.
func TestDispatcherEvents(t *testing.T) {
	t.Parallel()
	f := newChannelFixture(t)
	ctx := t.Context()
	c, token := f.channel("tg", 1001)
	alice, bob := f.human("alice"), f.human("bob")
	ba := f.bind(c, alice, "11", "1", store.EventMailNew, store.EventSendHeld)
	f.bind(c, bob, "22", "2", store.EventSendHeld)
	d := f.dispatcher(f.sealer)

	if err := d.NewMail(ctx, Mail{DeliveryID: f.delivery(), Text: "[example.com] a@ ← x: hi"}); err != nil {
		t.Fatal(err)
	}
	// A delivery nobody may see (here: none at all) is not pushed.
	if err := d.NewMail(ctx, Mail{DeliveryID: ulid.New(time.Now()), Text: "unseen"}); err != nil {
		t.Fatal(err)
	}
	if got := f.api.sent(token, "11"); !slices.Equal(got, []string{"[example.com] a@ ← x: hi"}) {
		t.Errorf("alice got %q", got)
	}
	if got := f.api.sent(token, "22"); len(got) != 0 {
		t.Errorf("bob, unsubscribed, got %q", got)
	}
	if err := f.db.RevokeBinding(ctx, ba.ID, f.action("channel.unbind")); err != nil {
		t.Fatal(err)
	}
	if err := d.NewMail(ctx, Mail{DeliveryID: f.delivery(), Text: "after"}); err != nil {
		t.Fatal(err)
	}
	if got := f.api.sent(token, "11"); len(got) != 1 {
		t.Errorf("revoked binding got %q", got)
	}

	// The recall button only where a button user is bound; a card about
	// a row nobody may see goes nowhere.
	if err := d.Held(ctx, HeldCard{OutboxID: ulid.New(time.Now()), Subject: "s"}); err != nil {
		t.Fatal(err)
	}
	if got := f.api.sent(token, "22"); len(got) != 0 {
		t.Errorf("card of an unknown row sent: %q", got)
	}

	// Locked: nothing is sent, nothing fails.
	if err := f.dispatcher(nil).NewMail(ctx, Mail{DeliveryID: f.delivery(), Text: "locked"}); err != nil {
		t.Fatal(err)
	}
	if n := f.api.count("", "sendMessage"); n != 1 {
		t.Errorf("%d messages sent", n)
	}
}

// account.trouble goes only to who may read the account (§13.8): a pull
// account has no default readers, so a person subscribed who has no row
// hears nothing of it, not even that it exists.
func TestDispatcherAccountTrouble(t *testing.T) {
	t.Parallel()
	f := newChannelFixture(t)
	ctx := t.Context()
	c, token := f.channel("tg", 1001)
	alice, bob, carol := f.human("alice"), f.human("bob"), f.human("carol")
	f.bind(c, alice, "11", "1", store.EventAccountTrouble)
	f.bind(c, bob, "22", "2", store.EventAccountTrouble)
	f.bind(c, carol, "33", "3", store.EventMailNew)

	cfg := store.SourceConfig{
		Address: "me@example.com", Host: "imap.example.com", Port: 993, Auth: "plain", Username: "me",
		MaxMessageBytes: 1 << 20, PollSeconds: 60,
	}
	aud, err := secret.IMAPS(cfg.Username, cfg.Host, cfg.Port)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := f.sealer.Seal(ctx, ulid.New(time.Now()), store.SecretIMAPPassword, aud, []byte("password"))
	if err != nil {
		t.Fatal(err)
	}
	src, err := f.db.AddPullAccount(ctx, "gmail", cfg, sealed, alice, f.action("account.add"))
	if err != nil {
		t.Fatal(err)
	}

	d := f.dispatcher(f.sealer)
	card := TroubleCard{AccountID: src.AccountID, Account: "gmail", Kind: TroubleRefused}
	n, err := d.AccountTrouble(ctx, card)
	if err != nil || n != 1 {
		t.Fatalf("reached %d, %v", n, err)
	}
	if got := f.api.sent(token, "11"); len(got) != 1 || !strings.Contains(got[0], "gmail") {
		t.Errorf("the reader got %q", got)
	}
	if got := f.api.sent(token, "22"); len(got) != 0 {
		t.Errorf("subscribed but cannot read the account: got %q", got)
	}
	if got := f.api.sent(token, "33"); len(got) != 0 {
		t.Errorf("not subscribed: got %q", got)
	}

	// A row lets bob read it, and so hear of it.
	canRead := true
	if err := f.db.SetAccountAccess(ctx, "gmail", bob, false, nil, &canRead, f.action("account.access")); err != nil {
		t.Fatal(err)
	}
	if n, err := d.AccountTrouble(ctx, card); err != nil || n != 2 {
		t.Fatalf("reached %d, %v", n, err)
	}
	if got := f.api.sent(token, "22"); len(got) != 1 {
		t.Errorf("bob, now a reader, got %q", got)
	}

	// Locked: nothing is sent, nothing fails, nobody was reached.
	if n, err := f.dispatcher(nil).AccountTrouble(ctx, card); err != nil || n != 0 {
		t.Errorf("locked: reached %d, %v", n, err)
	}
}

// security reaches every administrator and the subject, never through
// the binding left out, and regardless of subscriptions. A token that
// fails to authenticate locks its channel and raises an alert on the
// others.
func TestDispatcherSecurity(t *testing.T) {
	t.Parallel()
	f := newChannelFixture(t)
	ctx := t.Context()
	c, token := f.channel("tg", 1001)
	c2, token2 := f.channel("tg2", 1002)
	alice, bob := f.human("alice"), f.human("bob")
	f.bind(c, alice, "11", "1")
	ba2 := f.bind(c2, alice, "12", "1")
	f.bind(c, bob, "22", "2")
	d := f.dispatcher(f.sealer)

	if err := d.Security(ctx, Alert{Subject: alice, Except: ba2.ID, Text: "yuuka: bound"}); err != nil {
		t.Fatal(err)
	}
	if len(f.api.sent(token, "11")) != 1 || len(f.api.sent(token, "22")) != 1 || len(f.api.sent(token2, "12")) != 0 {
		t.Errorf("security alert: %v", f.api.all("sendMessage"))
	}
	if _, err := f.pool.Exec(ctx, `UPDATE principals SET disabled_at = now() WHERE id = $1`, bob); err != nil {
		t.Fatal(err)
	}
	if err := d.Notify(ctx, "yuuka: passkey"); err != nil {
		t.Fatal(err)
	}
	if len(f.api.sent(token, "22")) != 1 || len(f.api.sent(token, "11")) != 2 || len(f.api.sent(token2, "12")) != 1 {
		t.Errorf("after bob was disabled: %v", f.api.all("sendMessage"))
	}

	// Corrupt tg2's token: it is locked, and tg says so.
	if _, err := f.pool.Exec(ctx, `UPDATE secrets SET payload = set_byte(payload, 20, get_byte(payload, 20) # 1), revision = revision + 1
		WHERE id = $1`, c2.SecretID); err != nil {
		t.Fatal(err)
	}
	err := d.Notify(ctx, "yuuka: again")
	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Errorf("tampered channel: %v", err)
	}
	got := f.api.sent(token, "11")
	if !slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, "channel tg2 failed authentication") }) {
		t.Errorf("no tamper alert on tg: %q", got)
	}
	if n := len(f.api.sent(token2, "12")); n != 1 {
		t.Errorf("tampered channel still sent: %d", n)
	}
}

// The manager polls each enabled channel. /start <code> records who
// sent it, whichever chat that is, and binds nothing; a press counts on
// a live binding only. Locked, nothing runs.
func TestManager(t *testing.T) {
	t.Parallel()
	f := newChannelFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c, token := f.channel("tg", 1001)
	alice := f.human("alice")
	b := f.bind(c, alice, "11", "1", store.EventSendHeld)

	const code = "Zm9vYmFyYmF6cXV4cXV1eA"
	bc, err := f.db.NewBindCode(ctx, c.ID, alice, "", code, f.action("channel.bind.begin"))
	if err != nil {
		t.Fatal(err)
	}
	start := &Message{ID: 1, From: &User{ID: 666, FirstName: "Mallory", Username: "mallory"}, Text: "/start@yuuka_bot " + code}
	start.Chat = Chat{ID: -100, Type: "group", Title: "Not yours"}
	pressBy := func(id, from, chat int64) Update {
		u := press(id, from, chat, "w:"+ulid.New(time.Now()))
		return u
	}
	f.api.queue(token,
		Update{ID: 1, Message: start},
		Update{ID: 2, Message: &Message{From: &User{ID: 7}, Chat: Chat{ID: 7, Type: "private"}, Text: "/start nope"}},
		pressBy(3, 1, 11), // alice on her binding
		pressBy(4, 2, 11), // someone else in her chat
	)

	m := &Manager{D: f.dispatcher(f.sealer), Interval: 50 * time.Millisecond, PollTimeout: 1}
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	waitFor(t, func() bool {
		return f.api.count(token, "answerCallbackQuery") >= 1 && f.api.count(token, "sendMessage") >= 2
	})

	got, err := f.db.BindCodeByID(ctx, bc.ID)
	if err != nil || got.RedeemedAt == nil || got.SeenAddress != "-100" || got.SeenActorID != "666" ||
		got.SeenLabel != "Mallory (@mallory) in Not yours" || got.SeenChatKind != "group" || got.BindingID != "" {
		t.Errorf("redeemed code %+v %v", got, err)
	}
	if r := f.api.all("sendMessage"); !slices.ContainsFunc(r, func(c call) bool {
		return c.body["chat_id"] == float64(7) && strings.Contains(c.body["text"].(string), "not valid")
	}) {
		t.Errorf("no answer to a bad code: %v", r)
	}
	answers := f.api.all("answerCallbackQuery")
	if len(answers) != 1 || answers[0].body["text"] != "No such message." {
		t.Errorf("answers %v", answers)
	}
	if lb, err := f.db.BindingByID(ctx, b.ID); err != nil || lb.LastUsedAt == nil {
		t.Errorf("binding not stamped: %+v %v", lb, err)
	}

	// Revoked: the next press does not count.
	if err := f.db.RevokeBinding(ctx, b.ID, f.action("channel.unbind")); err != nil {
		t.Fatal(err)
	}
	f.api.queue(token, pressBy(5, 1, 11))
	waitFor(t, func() bool {
		f.api.mu.Lock()
		defer f.api.mu.Unlock()
		return f.api.updates[token] == nil
	})
	time.Sleep(100 * time.Millisecond)
	if n := f.api.count(token, "answerCallbackQuery"); n != 1 {
		t.Errorf("a press on a revoked binding was answered (%d)", n)
	}

	// A removed channel stops being polled.
	if err := f.db.DisableChannel(ctx, "tg", c.Revision, f.action("channel.rm")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	before := f.api.count(token, "getUpdates")
	time.Sleep(200 * time.Millisecond)
	if after := f.api.count(token, "getUpdates"); after != before {
		t.Errorf("removed channel still polled: %d -> %d", before, after)
	}
	cancel()
	<-done
}

func TestManagerLocked(t *testing.T) {
	t.Parallel()
	f := newChannelFixture(t)
	f.channel("tg", 1001)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	(&Manager{D: f.dispatcher(nil), Interval: 10 * time.Millisecond, LockedEvery: 10 * time.Millisecond}).Run(ctx)
	if n := len(f.api.all("getUpdates")) + len(f.api.all("sendMessage")); n != 0 {
		t.Errorf("locked manager made %d calls", n)
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Error("Run returned before its context ended")
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStartCode(t *testing.T) {
	for text, want := range map[string]string{
		"/start abc": "abc", "/start@yuuka_bot abc": "abc", "/start": "", "/start a b": "", "/stop abc": "", "hello": "",
	} {
		if got := StartCode(text); got != want {
			t.Errorf("StartCode(%q) = %q, want %q", text, got, want)
		}
	}
}
