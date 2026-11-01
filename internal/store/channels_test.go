package store_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// 0018 applies on top of 0017 with transports in it, and its constraints
// hold: one enabled channel per bot, one live binding per chat, events
// from the closed list only.
func TestMigrateChannels(t *testing.T) {
	t.Parallel()
	ownerURL, _ := storetest.URLs(t)
	pool, err := pgxpool.New(t.Context(), ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ctx := t.Context()
	db := store.New(pool)
	if _, to, err := db.MigrateTo(ctx, 17); err != nil || to != 17 {
		t.Fatalf("migrate to 17: %d, %v", to, err)
	}
	if err := db.EnsureTransports(ctx, []string{"cf"}); err != nil {
		t.Fatal(err)
	}
	if _, to, err := db.Migrate(ctx); err != nil || to < 18 {
		t.Fatalf("migrate: %d, %v", to, err)
	}
	if trs, err := db.Transports(ctx, true); err != nil || len(trs) != 1 {
		t.Fatalf("transports after 0018: %v %v", trs, err)
	}
	e := &channelEnv{t: t, db: db, pool: pool}
	op := e.human("operator")
	c := e.channel("tg", 1001)
	for _, bad := range []string{
		// Another enabled channel of the same bot.
		`INSERT INTO notification_channels (id, name, kind, external_id, secret_id) VALUES ('` + ulid.New(time.Now()) + `', 'tg2', 'telegram', '1001', NULL)`,
		// An enabled channel without its secret.
		`INSERT INTO notification_channels (id, name, kind, external_id) VALUES ('` + ulid.New(time.Now()) + `', 'tg3', 'telegram', '1002')`,
	} {
		if _, err := pool.Exec(ctx, bad); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
	b := e.bind(c, op, "42", "1001", store.Events)
	if _, err := pool.Exec(ctx, `INSERT INTO notification_subscriptions (binding_id, event) VALUES ($1, 'security')`, b.ID); err == nil {
		t.Error("security subscribed")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO channel_bindings (id, channel_id, principal_id, address) VALUES ($1, $2, $3, '42')`,
		ulid.New(time.Now()), c.ID, op); err == nil {
		t.Error("a chat bound twice")
	}
}

// 0020 adds account.trouble to the bindings that took every event, and
// leaves one narrowed by hand as it is. Pauses and failures older than it
// count as not told of yet: they never had their push.
func TestMigrateAccountTrouble(t *testing.T) {
	t.Parallel()
	ownerURL, _ := storetest.URLs(t)
	pool, err := pgxpool.New(t.Context(), ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ctx := t.Context()
	db := store.New(pool)
	if _, to, err := db.MigrateTo(ctx, 19); err != nil || to != 19 {
		t.Fatalf("migrate to 19: %d, %v", to, err)
	}
	e := &channelEnv{t: t, db: db, pool: pool}
	op := e.human("operator")
	c := e.channel("tg", 1001)
	every := []string{store.EventMailNew, store.EventSendHeld, store.EventSendEscalated, store.EventGrantSuspended}
	all, narrowed := e.bind(c, op, "1", "1", every), e.bind(c, op, "2", "2", every[:1])
	if _, err := pool.Exec(ctx, `INSERT INTO notification_subscriptions (binding_id, event) VALUES ($1, 'account.trouble')`, all.ID); err == nil {
		t.Error("0019 accepted account.trouble")
	}
	if _, to, err := db.Migrate(ctx); err != nil || to < 20 {
		t.Fatalf("migrate: %d, %v", to, err)
	}
	got, err := db.SubscribedBindings(ctx, store.EventAccountTrouble)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{all.ID}; !slices.Equal(ids(got), want) {
		t.Errorf("subscribed %v, want %v, not %s", ids(got), want, narrowed.ID)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO notification_subscriptions (binding_id, event) VALUES ($1, 'security')`, all.ID); err == nil {
		t.Error("security subscribed")
	}
}

type channelEnv struct {
	t    *testing.T
	db   *store.DB
	pool *pgxpool.Pool
	n    int
}

func newChannelEnv(t *testing.T) *channelEnv {
	t.Helper()
	db, pool := storetest.New(t)
	return &channelEnv{t: t, db: db, pool: pool}
}

func (e *channelEnv) at() time.Time {
	e.n++
	return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(e.n) * time.Second)
}

func (e *channelEnv) action(tool string) store.Action {
	at := e.at()
	return store.Action{ID: ulid.New(at), Actor: store.SystemPrincipalID, Tool: tool, At: at}
}

func (e *channelEnv) principal(kind, name string) string {
	e.t.Helper()
	p, err := e.db.EnsurePrincipal(e.t.Context(), store.Principal{ID: ulid.New(e.at()), Kind: kind, Name: name, DisplayName: name, CreatedAt: time.Now()})
	if err != nil {
		e.t.Fatal(err)
	}
	return p.ID
}

func (e *channelEnv) human(name string) string { return e.principal("human", name) }

// token is a stand-in for a sealed bot token: the store neither encrypts
// nor checks the ciphertext.
func (e *channelEnv) token(bot int64) *secret.Record {
	e.t.Helper()
	aud, err := secret.TelegramBot(bot)
	if err != nil {
		e.t.Fatal(err)
	}
	return &secret.Record{
		ID: ulid.New(e.at()), Kind: store.SecretTelegramBotToken, Audience: aud,
		Wrapper: "local", KeyRef: "0011223344556677", WrappedDEK: []byte("wrapped"), Payload: []byte("sealed"),
	}
}

func (e *channelEnv) channel(name string, bot int64) store.Channel {
	e.t.Helper()
	c, err := e.db.ConfigureChannel(e.t.Context(), store.ChannelConfig{
		Name: name, Kind: store.ChannelTelegram, ExternalID: itoa(bot), Settings: map[string]string{"username": "yuuka_bot"},
	}, 0, e.token(bot), e.action("channel.add"))
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

// bind makes a binding the way §13.7 does: a code, /start, confirmation.
func (e *channelEnv) bind(c store.Channel, principal, chat, user string, events []string) store.Binding {
	e.t.Helper()
	ctx := e.t.Context()
	code := "code-" + ulid.New(e.at())
	bc, err := e.db.NewBindCode(ctx, c.ID, principal, "", code, e.action("channel.bind.begin"))
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.db.RedeemBindCode(ctx, c.ID, code, store.Seen{Address: chat, ActorID: user, Label: "Alice", ChatKind: "private"}, e.at()); err != nil {
		e.t.Fatal(err)
	}
	b, err := e.db.ConfirmBindCode(ctx, bc.ID, "", events, e.action("channel.bind.confirm"))
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func ids(bs []store.Binding) []string {
	out := []string{}
	for _, b := range bs {
		out = append(out, b.ID)
	}
	slices.Sort(out)
	return out
}

// A channel is one bot: a token sealed for another bot, a second
// enabled channel of the same bot and moving an enabled channel to
// another bot are refused; rm erases the token and revokes bindings.
func TestConfigureChannel(t *testing.T) {
	t.Parallel()
	e := newChannelEnv(t)
	ctx := t.Context()
	cfg := store.ChannelConfig{Name: "tg", Kind: store.ChannelTelegram, ExternalID: "1001"}
	if _, err := e.db.ConfigureChannel(ctx, cfg, 0, e.token(1002), e.action("channel.add")); err == nil {
		t.Error("token of another bot accepted")
	}
	if _, err := e.db.ConfigureChannel(ctx, cfg, 0, nil, e.action("channel.add")); !errors.Is(err, store.ErrNeedsSecret) {
		t.Errorf("no token: %v", err)
	}
	bad := cfg
	bad.Settings = map[string]string{"token": "x"}
	if _, err := e.db.ConfigureChannel(ctx, bad, 0, e.token(1001), e.action("channel.add")); err == nil {
		t.Error("unknown setting accepted")
	}
	c := e.channel("tg", 1001)
	if c.Revision != 1 || c.SecretID == "" || c.BotID() != 1001 || c.Settings["username"] != "yuuka_bot" {
		t.Fatalf("channel %+v", c)
	}
	other := cfg
	other.Name = "tg2"
	if _, err := e.db.ConfigureChannel(ctx, other, 0, e.token(1001), e.action("channel.add")); !errors.Is(err, store.ErrDuplicate) {
		t.Errorf("second channel of one bot: %v", err)
	}
	// A new token for the same bot replaces the secret.
	tok := e.token(1001)
	c2, err := e.db.ConfigureChannel(ctx, cfg, c.Revision, tok, e.action("channel.add"))
	if err != nil || c2.SecretID != tok.ID || c2.Revision != 2 {
		t.Fatalf("rotate: %+v %v", c2, err)
	}
	if _, err := e.db.Secret(ctx, c.SecretID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("old token kept: %v", err)
	}
	if _, err := e.db.ConfigureChannel(ctx, cfg, c.Revision, e.token(1001), e.action("channel.add")); !errors.Is(err, store.ErrStale) {
		t.Errorf("stale: %v", err)
	}
	moved := cfg
	moved.ExternalID = "1002"
	if _, err := e.db.ConfigureChannel(ctx, moved, c2.Revision, e.token(1002), e.action("channel.add")); !errors.Is(err, store.ErrChannelBot) {
		t.Errorf("enabled channel moved to another bot: %v", err)
	}

	op := e.human("operator")
	b := e.bind(c2, op, "42", "7", store.Events)
	if err := e.db.DisableChannel(ctx, "tg", c2.Revision, e.action("channel.rm")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Secret(ctx, tok.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("token kept after rm: %v", err)
	}
	if got, err := e.db.BindingByID(ctx, b.ID); err != nil || got.RevokedAt == nil {
		t.Errorf("binding after rm: %+v %v", got, err)
	}
	if cs, err := e.db.EnabledChannels(ctx); err != nil || len(cs) != 0 {
		t.Errorf("enabled channels %v %v", cs, err)
	}
	// Added again, to another bot this time: no bindings come back.
	gone, err := e.db.ChannelByName(ctx, "tg")
	if err != nil {
		t.Fatal(err)
	}
	back, err := e.db.ConfigureChannel(ctx, moved, gone.Revision, e.token(1002), e.action("channel.add"))
	if err != nil || back.DisabledAt != nil || back.BotID() != 1002 {
		t.Fatalf("re-enable: %+v %v", back, err)
	}
	if bs, err := e.db.Bindings(ctx, false); err != nil || len(bs) != 0 {
		t.Errorf("bindings after re-enable: %v %v", bs, err)
	}
	if cs, err := e.db.EnabledChannels(ctx); err != nil || len(cs) != 1 || cs[0].Secret.Audience != "telegram:bot1002" {
		t.Errorf("enabled channels %+v %v", cs, err)
	}
}

// Bindings belong to enabled humans only (§13.8), checked when the code
// is made and again when it is confirmed.
func TestBindingsHumanOnly(t *testing.T) {
	t.Parallel()
	e := newChannelEnv(t)
	ctx := t.Context()
	c := e.channel("tg", 1001)
	agent := e.principal("agent", "arisu")
	if _, err := e.db.NewBindCode(ctx, c.ID, agent, "", "agent-code-0123456789", e.action("channel.bind.begin")); !errors.Is(err, store.ErrNotHuman) {
		t.Errorf("agent: %v", err)
	}
	if _, err := e.db.NewBindCode(ctx, c.ID, store.SystemPrincipalID, "", "system-code-0123456789", e.action("channel.bind.begin")); !errors.Is(err, store.ErrNotHuman) {
		t.Errorf("system: %v", err)
	}
	op := e.human("operator")
	bc, err := e.db.NewBindCode(ctx, c.ID, op, "", "human-code-0123456789", e.action("channel.bind.begin"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.RedeemBindCode(ctx, c.ID, "human-code-0123456789", store.Seen{Address: "42", ActorID: "7"}, e.at()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE principals SET disabled_at = now() WHERE id = $1`, op); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.ConfirmBindCode(ctx, bc.ID, "", store.Events, e.action("channel.bind.confirm")); !errors.Is(err, store.ErrNotHuman) {
		t.Errorf("disabled since the code: %v", err)
	}
}

// A bind code: only its hash is stored, it is redeemed once, on its
// channel, before it expires, and confirmed once, after the /start.
func TestBindCode(t *testing.T) {
	t.Parallel()
	e := newChannelEnv(t)
	ctx := t.Context()
	c, other := e.channel("tg", 1001), e.channel("tg2", 1002)
	op := e.human("operator")
	const code = "s3cr3t-c0de-0123456789"
	bc, err := e.db.NewBindCode(ctx, c.ID, op, "", code, e.action("channel.bind.begin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bc.ExpiresAt.Equal(bc.CreatedAt.Add(store.BindCodeTTL)) {
		t.Errorf("expires %v, created %v", bc.ExpiresAt, bc.CreatedAt)
	}
	// Nothing in the database holds the code, and no action its hash.
	sum := sha256.Sum256([]byte(code))
	var hash []byte
	if err := e.pool.QueryRow(ctx, `SELECT code_hash FROM channel_bind_codes WHERE id = $1`, bc.ID).Scan(&hash); err != nil || !bytes.Equal(hash, sum[:]) {
		t.Errorf("code_hash %x %v", hash, err)
	}
	var leaks int
	if err := e.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM channel_bind_codes t WHERE t::text LIKE '%' || $1 || '%') +
		(SELECT count(*) FROM actions a WHERE a::text LIKE '%' || $1 || '%' OR a::text LIKE '%' || $2 || '%')`,
		code, hex.EncodeToString(sum[:])).Scan(&leaks); err != nil || leaks != 0 {
		t.Errorf("code or its hash stored in the clear: %d %v", leaks, err)
	}

	if _, err := e.db.ConfirmBindCode(ctx, bc.ID, "", store.Events, e.action("channel.bind.confirm")); !errors.Is(err, store.ErrNotRedeemed) {
		t.Errorf("confirm before /start: %v", err)
	}
	seen := store.Seen{Address: "-100200", ActorID: "7", Label: "Mallory (@mallory) in Family", ChatKind: "supergroup"}
	if _, err := e.db.RedeemBindCode(ctx, other.ID, code, seen, e.at()); !errors.Is(err, store.ErrBindCode) {
		t.Errorf("redeemed on another bot: %v", err)
	}
	if _, err := e.db.RedeemBindCode(ctx, c.ID, "wrong", seen, e.at()); !errors.Is(err, store.ErrBindCode) {
		t.Errorf("wrong code: %v", err)
	}
	got, err := e.db.RedeemBindCode(ctx, c.ID, code, seen, e.at())
	if err != nil || got.SeenAddress != "-100200" || got.SeenActorID != "7" || got.SeenLabel != seen.Label || got.SeenChatKind != "supergroup" {
		t.Fatalf("redeem: %+v %v", got, err)
	}
	// Whoever sends it second, the first stands: the person who
	// confirms sees that one, and says no if it is not them.
	if _, err := e.db.RedeemBindCode(ctx, c.ID, code, store.Seen{Address: "42", ActorID: "8"}, e.at()); !errors.Is(err, store.ErrBindCode) {
		t.Errorf("redeemed twice: %v", err)
	}
	b, err := e.db.ConfirmBindCode(ctx, bc.ID, "", []string{store.EventSendHeld}, e.action("channel.bind.confirm"))
	if err != nil || b.Address != "-100200" || b.ActorID != "7" || b.Label != seen.Label || !slices.Equal(b.Events, []string{store.EventSendHeld}) {
		t.Fatalf("confirm: %+v %v", b, err)
	}
	var result string
	if err := e.pool.QueryRow(ctx, `SELECT result->>'binding_id' FROM actions WHERE tool = 'channel.bind.confirm'`).Scan(&result); err != nil || result != b.ID {
		t.Errorf("confirm action names %q, %v", result, err)
	}
	if _, err := e.db.ConfirmBindCode(ctx, bc.ID, "", store.Events, e.action("channel.bind.confirm")); !errors.Is(err, store.ErrBindCode) {
		t.Errorf("confirmed twice: %v", err)
	}

	// Expired: neither redeemed nor confirmed.
	late, err := e.db.NewBindCode(ctx, c.ID, op, "", "late-code-0123456789", e.action("channel.bind.begin"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.RedeemBindCode(ctx, c.ID, "late-code-0123456789", seen, late.ExpiresAt); !errors.Is(err, store.ErrBindCode) {
		t.Errorf("redeemed when expired: %v", err)
	}
	if _, err := e.db.RedeemBindCode(ctx, c.ID, "late-code-0123456789", store.Seen{Address: "43", ActorID: "7"}, late.ExpiresAt.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	a := e.action("channel.bind.confirm")
	a.At = late.ExpiresAt
	if _, err := e.db.ConfirmBindCode(ctx, late.ID, "", store.Events, a); !errors.Is(err, store.ErrBindCode) {
		t.Errorf("confirmed when expired: %v", err)
	}
	// A chat is bound once.
	dup, err := e.db.NewBindCode(ctx, c.ID, op, "", "dup-code-0123456789", e.action("channel.bind.begin"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.RedeemBindCode(ctx, c.ID, "dup-code-0123456789", seen, e.at()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.ConfirmBindCode(ctx, dup.ID, "", store.Events, e.action("channel.bind.confirm")); !errors.Is(err, store.ErrDuplicate) {
		t.Errorf("chat bound twice: %v", err)
	}
	if err := e.db.DropBindCode(ctx, dup.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.BindCodeByID(ctx, dup.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("dropped code: %v", err)
	}
}

// Who an event and a press reach is read when they happen: revoking,
// unsubscribing, disabling the person or removing the channel counts
// from the next one. security reaches every administrator (every enabled
// human) and the person it is about, subscriptions or not.
func TestBindingsLive(t *testing.T) {
	t.Parallel()
	e := newChannelEnv(t)
	ctx := t.Context()
	c, c2 := e.channel("tg", 1001), e.channel("tg2", 1002)
	alice, bob, carol := e.human("alice"), e.human("bob"), e.human("carol")
	ba := e.bind(c, alice, "1", "1", store.Events)
	bb := e.bind(c, bob, "2", "2", []string{store.EventSendHeld})
	bc := e.bind(c2, carol, "3", "3", nil)

	sub := func(event string) []string {
		t.Helper()
		bs, err := e.db.SubscribedBindings(ctx, event)
		if err != nil {
			t.Fatal(err)
		}
		return ids(bs)
	}
	if got := sub(store.EventMailNew); !slices.Equal(got, []string{ba.ID}) {
		t.Errorf("mail.new %v", got)
	}
	if got := sub(store.EventSendHeld); !slices.Equal(got, ids([]store.Binding{ba, bb})) {
		t.Errorf("send.held %v", got)
	}
	if _, err := e.db.SubscribedBindings(ctx, "security"); err == nil {
		t.Error("security subscribable")
	}
	if err := e.db.Subscribe(ctx, bc.ID, []string{"security"}, true, e.action("channel.subscribe")); err == nil {
		t.Error("subscribed to security")
	}
	sec, err := e.db.SecurityBindings(ctx, "")
	if err != nil || !slices.Equal(ids(sec), ids([]store.Binding{ba, bb, bc})) {
		t.Errorf("security %v %v", ids(sec), err)
	}

	// A press counts on a live binding, from its button user, in its chat.
	if b, err := e.db.BindingForPress(ctx, c.ID, "1", "1", e.at()); err != nil || b.ID != ba.ID || b.LastUsedAt == nil {
		t.Errorf("press: %+v %v", b, err)
	}
	for _, p := range [][3]string{{c.ID, "1", "2"}, {c.ID, "2", "1"}, {c2.ID, "1", "1"}} {
		if _, err := e.db.BindingForPress(ctx, p[0], p[1], p[2], e.at()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("press %v: %v", p, err)
		}
	}

	if err := e.db.RevokeBinding(ctx, ba.ID, e.action("channel.unbind")); err != nil {
		t.Fatal(err)
	}
	if err := e.db.RevokeBinding(ctx, ba.ID, e.action("channel.unbind")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("revoked twice: %v", err)
	}
	if got := sub(store.EventSendHeld); !slices.Equal(got, []string{bb.ID}) {
		t.Errorf("send.held after revoking %v", got)
	}
	if _, err := e.db.BindingForPress(ctx, c.ID, "1", "1", e.at()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("press on a revoked binding: %v", err)
	}
	if err := e.db.Subscribe(ctx, bb.ID, []string{store.EventSendHeld}, false, e.action("channel.unsubscribe")); err != nil {
		t.Fatal(err)
	}
	if got := sub(store.EventSendHeld); len(got) != 0 {
		t.Errorf("send.held after unsubscribing %v", got)
	}

	// A disabled person is no administrator and gets nothing, even
	// as the subject; the others still get security alerts.
	if _, err := e.pool.Exec(ctx, `UPDATE principals SET disabled_at = now() WHERE id = $1`, carol); err != nil {
		t.Fatal(err)
	}
	if sec, err := e.db.SecurityBindings(ctx, carol); err != nil || !slices.Equal(ids(sec), []string{bb.ID}) {
		t.Errorf("security with carol disabled %v %v", ids(sec), err)
	}
	if err := e.db.DisableChannel(ctx, "tg", c.Revision, e.action("channel.rm")); err != nil {
		t.Fatal(err)
	}
	if sec, err := e.db.SecurityBindings(ctx, ""); err != nil || len(sec) != 0 {
		t.Errorf("security with the channel removed %v %v", ids(sec), err)
	}
}

// The import from the environment writes once however many processes
// start at once, and never over a channel that is there.
func TestImportTelegram(t *testing.T) {
	t.Parallel()
	e := newChannelEnv(t)
	ctx := t.Context()
	op := e.human("operator")
	imp := func() store.ImportedTelegram {
		return store.ImportedTelegram{
			Channel: store.ChannelConfig{Name: "telegram", Kind: store.ChannelTelegram, ExternalID: "1001"},
			Token:   *e.token(1001), Principal: op, Address: "42", ActorID: "7", Label: "imported", Events: store.Events,
		}
	}
	if _, err := e.db.ImportTelegram(ctx, store.ImportedTelegram{
		Channel: imp().Channel, Token: *e.token(1001), Principal: e.principal("agent", "arisu"), Address: "42", Events: store.Events,
	}, e.action("config.import")); !errors.Is(err, store.ErrNotHuman) {
		t.Errorf("imported for an agent: %v", err)
	}
	const n = 4
	type res struct {
		ok  bool
		err error
	}
	out := make(chan res, n)
	imps, acts := make([]store.ImportedTelegram, n), make([]store.Action, n)
	for i := range n {
		imps[i], acts[i] = imp(), e.action("config.import")
	}
	for i := range n {
		go func() {
			ok, err := e.db.ImportTelegram(ctx, imps[i], acts[i])
			out <- res{ok, err}
		}()
	}
	wrote := 0
	for range n {
		r := <-out
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.ok {
			wrote++
		}
	}
	if wrote != 1 {
		t.Errorf("%d imports wrote", wrote)
	}
	if ok, err := e.db.ImportTelegram(ctx, imp(), e.action("config.import")); ok || err != nil {
		t.Errorf("import again: %v %v", ok, err)
	}
	var channels, secrets, actions int
	if err := e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM notification_channels), (SELECT count(*) FROM secrets),
		(SELECT count(*) FROM actions WHERE tool = 'config.import')`).Scan(&channels, &secrets, &actions); err != nil {
		t.Fatal(err)
	}
	if channels != 1 || secrets != 1 || actions != 1 {
		t.Errorf("%d channels, %d secrets, %d actions", channels, secrets, actions)
	}
	bs, err := e.db.Bindings(ctx, false)
	if err != nil || len(bs) != 1 || bs[0].PrincipalID != op || bs[0].Address != "42" || bs[0].ActorID != "7" ||
		!slices.Equal(bs[0].Events, store.Events) {
		t.Errorf("binding %+v %v", bs, err)
	}
}
