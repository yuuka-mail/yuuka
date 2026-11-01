package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/manage"
	"github.com/yuuka-mail/yuuka/internal/notify"
	"github.com/yuuka-mail/yuuka/internal/openapi"
	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/transport"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Transports, channels and bindings are configuration: reads without a
// viewer, writes recorded as the actor.

func (s *spy) TransportByName(ctx context.Context, name string) (store.Transport, error) {
	s.saw("TransportByName", configRead)
	return s.DB.TransportByName(ctx, name)
}

func (s *spy) ConfigureTransport(ctx context.Context, c store.TransportConfig, revision int64, sealed *secret.Record, a store.Action) (store.Transport, error) {
	s.saw("ConfigureTransport", actor(a))
	return s.DB.ConfigureTransport(ctx, c, revision, sealed, a)
}

func (s *spy) DisableTransport(ctx context.Context, name string, revision int64, a store.Action) error {
	s.saw("DisableTransport", actor(a))
	return s.DB.DisableTransport(ctx, name, revision, a)
}

func (s *spy) ReplaceIdentityTransports(ctx context.Context, id string, was, names []string, a store.Action) error {
	s.saw("ReplaceIdentityTransports", actor(a))
	return s.DB.ReplaceIdentityTransports(ctx, id, was, names, a)
}

func (s *spy) Secret(ctx context.Context, id string) (store.StoredSecret, error) {
	s.saw("Secret", configRead)
	return s.DB.Secret(ctx, id)
}

func (s *spy) Secrets(ctx context.Context) ([]store.StoredSecret, error) {
	s.saw("Secrets", configRead)
	return s.DB.Secrets(ctx)
}

func (s *spy) Channels(ctx context.Context, all bool) ([]store.Channel, error) {
	s.saw("Channels", configRead)
	return s.DB.Channels(ctx, all)
}

func (s *spy) ChannelByName(ctx context.Context, name string) (store.Channel, error) {
	s.saw("ChannelByName", configRead)
	return s.DB.ChannelByName(ctx, name)
}

func (s *spy) ConfigureChannel(ctx context.Context, c store.ChannelConfig, revision int64, sealed *secret.Record, a store.Action) (store.Channel, error) {
	s.saw("ConfigureChannel", actor(a))
	return s.DB.ConfigureChannel(ctx, c, revision, sealed, a)
}

func (s *spy) DisableChannel(ctx context.Context, name string, revision int64, a store.Action) error {
	s.saw("DisableChannel", actor(a))
	return s.DB.DisableChannel(ctx, name, revision, a)
}

func (s *spy) Bindings(ctx context.Context, all bool) ([]store.Binding, error) {
	s.saw("Bindings", configRead)
	return s.DB.Bindings(ctx, all)
}

func (s *spy) BindingByID(ctx context.Context, id string) (store.Binding, error) {
	s.saw("BindingByID", configRead)
	return s.DB.BindingByID(ctx, id)
}

func (s *spy) RevokeBinding(ctx context.Context, id string, a store.Action) error {
	s.saw("RevokeBinding", actor(a))
	return s.DB.RevokeBinding(ctx, id, a)
}

func (s *spy) Subscribe(ctx context.Context, id string, events []string, on bool, a store.Action) error {
	s.saw("Subscribe", actor(a))
	return s.DB.Subscribe(ctx, id, events, on, a)
}

func (s *spy) NewBindCode(ctx context.Context, channel, principal, createdWith, code string, a store.Action) (store.BindCode, error) {
	s.saw("NewBindCode", actor(a))
	return s.DB.NewBindCode(ctx, channel, principal, createdWith, code, a)
}

func (s *spy) BindCodeByID(ctx context.Context, id string) (store.BindCode, error) {
	s.saw("BindCodeByID", configRead)
	return s.DB.BindCodeByID(ctx, id)
}

func (s *spy) ConfirmBindCode(ctx context.Context, id, label string, events []string, a store.Action) (store.Binding, error) {
	s.saw("ConfirmBindCode", actor(a))
	return s.DB.ConfirmBindCode(ctx, id, label, events, a)
}

// DropBindCode records nothing: nothing was granted.
func (s *spy) DropBindCode(ctx context.Context, id string) error {
	s.saw("DropBindCode", configRead)
	return s.DB.DropBindCode(ctx, id)
}

func testSealer(t *testing.T) *secret.Sealer {
	t.Helper()
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
	s, err := secret.NewSealer(ring)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// telegramFake is the Bot API as far as the settings use it: getMe
// answers with the bot a token names, as acl_<id>_bot; test messages are
// recorded.
type telegramFake struct {
	mu    sync.Mutex
	tests []string // channel/binding
}

func (f *telegramFake) GetMe(_ context.Context, token string) (notify.Bot, error) {
	id := manage.TokenBotID(token)
	if strings.HasSuffix(token, "refused") {
		return notify.Bot{}, errors.New("telegram: getMe: status 401: Unauthorized")
	}
	return notify.Bot{ID: id, Username: fmt.Sprintf("acl_%d_bot", id)}, nil
}

func (f *telegramFake) Test(_ context.Context, name, binding string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tests = append(f.tests, name+"/"+binding)
	return 1, nil
}

func (f *telegramFake) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.tests
	f.tests = nil
	return out
}

// probeSpy records what a transport test connected with.
type probeSpy struct {
	mu   sync.Mutex
	seen []transport.Config
	// sources are the pull sources probed, as user:password.
	sources []string
}

func (p *probeSpy) Probe(_ context.Context, cfg transport.Config) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, cfg)
	if cfg.Host == "refuses.example.net" {
		return errors.New("transport: dial: connection refused")
	}
	return nil
}

func (h *harness) act(tool string) store.Action {
	at := time.Now()
	return store.Action{ID: ulid.New(at), Actor: store.SystemPrincipalID, Tool: tool, At: at}
}

// transport configures name to send through host without a password,
// as `yuuka transport add` does.
func (h *harness) transport(name, host string) store.Transport {
	h.t.Helper()
	t, err := h.db.ConfigureTransport(h.t.Context(), store.TransportConfig{
		Name: name, Host: host, Port: 465, Auth: store.TransportAuthNone, Caps: []string{},
	}, 0, nil, h.act("transport.add"))
	if err != nil {
		h.t.Fatal(err)
	}
	return t
}

// channel adds name with bot botID, as `yuuka channel add` does.
func (h *harness) channel(name string, botID int64) store.Channel {
	h.t.Helper()
	aud, _ := secret.TelegramBot(botID)
	sealed, err := h.sealer.Seal(h.t.Context(), ulid.New(time.Now()), store.SecretTelegramBotToken, aud,
		[]byte(strconv.FormatInt(botID, 10)+":AAAAAAAAAAAAAAAAAAAAAAAA"))
	if err != nil {
		h.t.Fatal(err)
	}
	c, err := h.db.ConfigureChannel(h.t.Context(), store.ChannelConfig{
		Name: name, Kind: store.ChannelTelegram, ExternalID: strconv.FormatInt(botID, 10),
		Settings: map[string]string{"username": fmt.Sprintf("acl_%d_bot", botID)},
	}, 0, &sealed, h.act("channel.add"))
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

// bindCode starts binding a chat of p on channel c as the CLI does,
// and has chat send it to the bot unless chat is "".
func (h *harness) bindCode(p store.Principal, c store.Channel, chat string) store.BindCode {
	h.t.Helper()
	code, err := manage.NewBindCode()
	if err != nil {
		h.t.Fatal(err)
	}
	bc, err := h.db.NewBindCode(h.t.Context(), c.ID, p.ID, "", code, h.act("channel.bind.begin"))
	if err != nil {
		h.t.Fatal(err)
	}
	if chat != "" {
		if bc, err = h.db.RedeemBindCode(h.t.Context(), c.ID, code, store.Seen{
			Address: chat, ActorID: "777", Label: "Operator (@operator)", ChatKind: "private",
		}, time.Now()); err != nil {
			h.t.Fatal(err)
		}
	}
	return bc
}

// binding binds chat to p on channel c.
func (h *harness) binding(p store.Principal, c store.Channel, chat string) store.Binding {
	h.t.Helper()
	bc := h.bindCode(p, c, chat)
	b, err := h.db.ConfirmBindCode(h.t.Context(), bc.ID, "", store.Events, h.act("channel.bind.confirm"))
	if err != nil {
		h.t.Fatal(err)
	}
	return b
}

// elevated is a fresh session of p's that may widen.
func (h *harness) elevated(p store.Principal) string {
	h.t.Helper()
	s := h.tokenFor(p, auth.AllScopes...)
	h.elevate(s)
	return s
}

// actionsMention counts the actions whose args mention s anywhere.
func (h *harness) actionsMention(s string) int {
	return h.count(`SELECT count(*) FROM actions WHERE args::text LIKE '%' || $1 || '%' OR coalesce(result::text, '') LIKE '%' || $1 || '%'`, s)
}

func TestTransportSettings(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	s := h.elevated(h.human)
	const pw = "correct-horse-battery-staple"
	put := func(name, body string) *httptest.ResponseRecorder {
		return h.do(http.MethodPut, "/api/v1/admin/transports/"+name, body, s)
	}

	w := put("relay", `{"revision":0,"host":"SMTP.example.net","port":465,"username":"u","password":"`+pw+`","caps":["message-id","return-path"]}`)
	tr := decode[ManagedTransport](t, w)
	if w.Code != http.StatusOK || tr.State != "configured" || tr.Host != "smtp.example.net" || tr.Secret == nil || tr.Secret.State != "ok" {
		t.Fatalf("add: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), pw) || h.actionsMention(pw) != 0 {
		t.Error("the password was answered or recorded")
	}
	if actor, cred, via := h.audit("transport.add"); actor != h.human.ID || cred == "" || via != "api" {
		t.Errorf("recorded as %s with %s via %s", actor, cred, via)
	}
	if a := h.alerts.takeFull(); len(a) != 1 || a[0].Subject != h.human.ID || !strings.Contains(a[0].Text, "u@smtp.example.net:465") {
		t.Errorf("alerts: %+v", a)
	}

	// A second add of the same name, and a change at an old revision,
	// are refused: neither overwrites what is there.
	if w := put("relay", `{"revision":0,"host":"smtp.example.net","port":465,"caps":[]}`); w.Code != http.StatusConflict {
		t.Errorf("add over a configured one: %d %s", w.Code, w.Body.String())
	}
	if w := put("relay", `{"revision":`+strconv.FormatInt(tr.Revision+1, 10)+`,"host":"smtp.example.net","port":465,"caps":[]}`); w.Code != http.StatusConflict ||
		!strings.Contains(problemOf(t, w).Detail, "changed since") {
		t.Errorf("stale: %d %s", w.Code, w.Body.String())
	}
	// Caps alone keep the password; another host needs it again.
	rev := strconv.FormatInt(tr.Revision, 10)
	if w := put("relay", `{"revision":`+rev+`,"host":"smtp.example.net","port":465,"username":"u","caps":["message-id"]}`); w.Code != http.StatusOK {
		t.Fatalf("caps: %d %s", w.Code, w.Body.String())
	} else if got := decode[ManagedTransport](t, w); got.Secret == nil || len(got.Caps) != 1 {
		t.Errorf("caps: %+v", got)
	} else {
		rev = strconv.FormatInt(got.Revision, 10)
	}
	// Changing caps widens too: they change the bytes sent (§13.10).
	if a := h.alerts.take(); len(a) != 1 {
		t.Errorf("caps alerts: %v", a)
	}
	if w := put("relay", `{"revision":`+rev+`,"host":"elsewhere.example.net","port":465,"username":"u","caps":[]}`); w.Code != http.StatusBadRequest ||
		!strings.Contains(problemOf(t, w).Detail, "give it again") {
		t.Errorf("move without the password: %d %s", w.Code, w.Body.String())
	}
	if w := put("bad", `{"revision":0,"host":"smtp.example.net","port":465}`); w.Code != http.StatusBadRequest {
		t.Errorf("no caps: %d", w.Code)
	}
	if w := put("bad", `{"revision":0,"host":"smtp.example.net","port":465,"caps":["teleport"]}`); w.Code != http.StatusBadRequest {
		t.Errorf("unknown cap: %d", w.Code)
	}

	// Testing opens the password for the destination the row names.
	w = h.do(http.MethodPost, "/api/v1/admin/transports/relay/test", "", s)
	if p := decode[TransportProbe](t, w); w.Code != http.StatusOK || !p.OK || len(h.probes.seen) != 1 || h.probes.seen[0].Password != pw {
		t.Errorf("test: %d %s", w.Code, w.Body.String())
	}
	h.transport("down", "refuses.example.net")
	if p := decode[TransportProbe](t, h.do(http.MethodPost, "/api/v1/admin/transports/down/test", "", s)); p.OK || !strings.Contains(p.Error, "refused") {
		t.Errorf("a refused test: %+v", p)
	}

	// Removing tightens: no elevation, no alert; the row stays.
	plain := h.tokenFor(h.human, auth.AllScopes...)
	w = h.do(http.MethodPost, "/api/v1/admin/transports/relay/disable", `{"revision":`+rev+`}`, plain)
	if w.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", w.Code, w.Body.String())
	}
	list := decode[TransportList](t, h.get("/api/v1/admin/transports", plain))
	for _, x := range list.Transports {
		if x.Name == "relay" && (x.State != "removed" || x.Secret != nil) {
			t.Errorf("removed: %+v", x)
		}
	}
	if a := h.alerts.take(); len(a) != 0 {
		t.Errorf("a tightening alerted: %v", a)
	}

	// Without the key: reads work, a password cannot be set (503).
	locked := New(h.spy, localRaw{h.local}, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler(h.svc)
	lh := *h
	lh.h = locked
	if l := decode[TransportList](t, lh.get("/api/v1/admin/transports", s)); !l.Locked {
		t.Error("locked list says unlocked")
	}
	if w := lh.do(http.MethodPut, "/api/v1/admin/transports/new", `{"revision":0,"host":"smtp.example.net","port":465,"username":"u","password":"x","caps":[]}`, s); w.Code != http.StatusServiceUnavailable ||
		problemOf(t, w).Code != openapi.CodeSecretsLocked {
		t.Errorf("locked add: %d %s", w.Code, w.Body.String())
	}
	st := decode[SecretsStatus](t, lh.get("/api/v1/admin/secrets", s))
	if !st.Locked {
		t.Errorf("status: %+v", st)
	}
}

func TestChainSettings(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.transport("relay", "smtp.example.net")
	i := h.identity("chain@example.org", h.human)
	plain := h.tokenFor(h.human, auth.AllScopes...)
	put := func(body, as string) *httptest.ResponseRecorder {
		return h.do(http.MethodPut, "/api/v1/admin/identities/"+i.ID+"/transports", body, as)
	}
	// A fallback lets mail go somewhere new: elevation first.
	if w := put(`{"transports":["cf","relay"]}`, plain); w.Code != http.StatusForbidden || problemOf(t, w).Code != openapi.CodeElevationRequired ||
		!strings.Contains(problemOf(t, w).Detail, "yuuka identity set chain@example.org --transports cf,relay") {
		t.Errorf("widening unelevated: %d %s", w.Code, w.Body.String())
	}
	if w := put(`{"transports":["cf","relay"]}`, h.elevated(h.human)); w.Code != http.StatusOK {
		t.Fatalf("widening: %d %s", w.Code, w.Body.String())
	} else if m := decode[ManagedIdentity](t, w); strings.Join(m.Transports, ",") != "cf,relay" {
		t.Errorf("chain: %v", m.Transports)
	}
	if a := h.alerts.takeFull(); len(a) != 1 || a[0].Subject != h.human.ID {
		t.Errorf("alerts: %+v", a)
	}
	if actor, _, via := h.audit("identity.route"); actor != h.human.ID || via != "api" {
		t.Errorf("recorded as %s via %s", actor, via)
	}
	// Taking the fallback away tightens: one step, no alert.
	if w := put(`{"transports":["cf"]}`, plain); w.Code != http.StatusOK {
		t.Errorf("tightening: %d %s", w.Code, w.Body.String())
	}
	if a := h.alerts.take(); len(a) != 0 {
		t.Errorf("a tightening alerted: %v", a)
	}
	if w := put(`{"transports":["nope"]}`, h.elevated(h.human)); w.Code != http.StatusBadRequest {
		t.Errorf("unknown transport: %d %s", w.Code, w.Body.String())
	}
}

func TestChannelSettings(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	s := h.elevated(h.human)
	const token = "4242:SECRETSECRETSECRETSECRET"
	w := h.do(http.MethodPut, "/api/v1/admin/channels/alerts", `{"revision":0,"token":"`+token+`"}`, s)
	c := decode[ManagedChannel](t, w)
	if w.Code != http.StatusOK || c.BotID != "4242" || c.BotUsername != "acl_4242_bot" || c.Secret == nil || c.Secret.State != "ok" {
		t.Fatalf("add: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "SECRETSECRET") || h.actionsMention("SECRETSECRET") != 0 {
		t.Error("the token was answered or recorded")
	}
	if a := h.alerts.takeFull(); len(a) != 1 || !strings.Contains(a[0].Text, "acl_4242_bot") {
		t.Errorf("alerts: %+v", a)
	}
	// Another bot under the same enabled channel, a refused token and
	// junk are refused.
	rev := strconv.FormatInt(c.Revision, 10)
	if w := h.do(http.MethodPut, "/api/v1/admin/channels/alerts", `{"revision":`+rev+`,"token":"4343:SECRETSECRETSECRETSECRET"}`, s); w.Code != http.StatusConflict {
		t.Errorf("another bot: %d %s", w.Code, w.Body.String())
	}
	if w := h.do(http.MethodPut, "/api/v1/admin/channels/x", `{"revision":0,"token":"4444:SECRETSECRETSECRETrefused"}`, s); w.Code != http.StatusBadGateway ||
		strings.Contains(w.Body.String(), "SECRETSECRET") {
		t.Errorf("refused token: %d %s", w.Code, w.Body.String())
	}
	if w := h.do(http.MethodPut, "/api/v1/admin/channels/x", `{"revision":0,"token":"not a token"}`, s); w.Code != http.StatusBadRequest {
		t.Errorf("junk: %d %s", w.Code, w.Body.String())
	}
	plain := h.tokenFor(h.human, auth.AllScopes...)
	if r := decode[ChannelTestResult](t, h.do(http.MethodPost, "/api/v1/admin/channels/alerts/test", "", plain)); r.Sent != 1 {
		t.Errorf("test: %+v", r)
	}
	if w := h.do(http.MethodPost, "/api/v1/admin/channels/alerts/disable", `{"revision":`+rev+`}`, plain); w.Code != http.StatusNoContent {
		t.Errorf("disable: %d %s", w.Code, w.Body.String())
	}
	l := decode[ChannelList](t, h.get("/api/v1/admin/channels", plain))
	if len(l.Channels) != 1 || l.Channels[0].DisabledAt == nil || l.Channels[0].Secret != nil {
		t.Errorf("list: %+v", l)
	}
}

func TestBindFlow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c := h.channel("alerts", 4242)
	s := h.elevated(h.human)
	// The person's binding elsewhere, which hears of the new one.
	earlier := h.binding(h.human, c, "1001")

	w := h.do(http.MethodPost, "/api/v1/bind-codes", `{"channel":"alerts"}`, s)
	start := decode[BindStart](t, w)
	if w.Code != http.StatusCreated || start.Code == "" || start.Link != "https://t.me/acl_4242_bot?start="+start.Code ||
		start.BindCode.State != "waiting" {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	if h.actionsMention(start.Code) != 0 {
		t.Error("the code was recorded")
	}
	code := "/api/v1/bind-codes/" + start.BindCode.ID
	plain := h.tokenFor(h.human, auth.AllScopes...)
	if w := h.do(http.MethodPost, code+"/confirm", `{}`, s); w.Code != http.StatusConflict {
		t.Errorf("confirm before /start: %d %s", w.Code, w.Body.String())
	}
	// The bot hears /start <code>, as serve's poller would record it.
	if _, err := h.db.RedeemBindCode(t.Context(), c.ID, start.Code, store.Seen{
		Address: "2002", ActorID: "778", Label: "Operator (@operator)", ChatKind: "private",
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	v := decode[BindCodeView](t, h.get(code, plain))
	if v.State != "redeemed" || v.Seen == nil || v.Seen.ChatID != "2002" || v.Seen.UserID != "778" || v.Seen.ChatKind != "private" {
		t.Errorf("poll: %+v", v)
	}
	// Someone else's code is not there for anyone else.
	other := principalNamed(t, h, "human", "someone")
	if w := h.get(code, h.elevated(other)); w.Code != http.StatusNotFound {
		t.Errorf("another person's code: %d", w.Code)
	}
	if w := h.do(http.MethodPost, code+"/confirm", `{}`, plain); w.Code != http.StatusForbidden || problemOf(t, w).Code != openapi.CodeElevationRequired {
		t.Errorf("confirm unelevated: %d %s", w.Code, w.Body.String())
	}
	w = h.do(http.MethodPost, code+"/confirm", `{"events":["mail.new","send.held"]}`, s)
	b := decode[MyBinding](t, w)
	if w.Code != http.StatusOK || b.Chat != "2002" || b.ButtonUser != "778" || len(b.Events) != 2 {
		t.Fatalf("confirm: %d %s", w.Code, w.Body.String())
	}
	if a := h.alerts.takeFull(); len(a) != 1 || a[0].Subject != h.human.ID || a[0].Except != b.ID {
		t.Errorf("alerts: %+v (the new binding %s is left out, %s is not)", a, b.ID, earlier.ID)
	}
	if got := h.tg.take(); len(got) != 1 || got[0] != "alerts/"+b.ID {
		t.Errorf("told the chat: %v", got)
	}
	if actor, _, via := h.audit("channel.bind.confirm"); actor != h.human.ID || via != "api" {
		t.Errorf("recorded as %s via %s", actor, via)
	}

	// Own bindings: listed, subscriptions changed, revoked; another
	// person's are not there.
	l := decode[BindingList](t, h.get("/api/v1/bindings", plain))
	if len(l.Bindings) != 2 || len(l.Channels) != 1 || l.Channels[0].BotUsername != "acl_4242_bot" {
		t.Errorf("list: %+v", l)
	}
	if got := decode[MyBinding](t, h.do(http.MethodPut, "/api/v1/bindings/"+b.ID+"/events", `{"events":["grant.suspended"]}`, plain)); len(got.Events) != 1 ||
		got.Events[0] != "grant.suspended" {
		t.Errorf("events: %+v", got)
	}
	if w := h.do(http.MethodPost, "/api/v1/bindings/"+b.ID+"/revoke", "", h.elevated(other)); w.Code != http.StatusNotFound {
		t.Errorf("revoke another's: %d", w.Code)
	}
	if w := h.do(http.MethodPost, "/api/v1/bindings/"+b.ID+"/revoke", "", plain); w.Code != http.StatusNoContent {
		t.Errorf("revoke: %d %s", w.Code, w.Body.String())
	}
	if a := h.alerts.take(); len(a) != 0 {
		t.Errorf("tightenings alerted: %v", a)
	}

	// Declining drops the code: a /start after it finds nothing.
	start = decode[BindStart](t, h.do(http.MethodPost, "/api/v1/bind-codes", `{"channel":"alerts"}`, s))
	if w := h.do(http.MethodPost, "/api/v1/bind-codes/"+start.BindCode.ID+"/decline", "", plain); w.Code != http.StatusNoContent {
		t.Errorf("decline: %d %s", w.Code, w.Body.String())
	}
	if _, err := h.db.RedeemBindCode(t.Context(), c.ID, start.Code, store.Seen{Address: "3003", ActorID: "9"}, time.Now()); !errors.Is(err, store.ErrBindCode) {
		t.Errorf("redeem after decline: %v", err)
	}
}
