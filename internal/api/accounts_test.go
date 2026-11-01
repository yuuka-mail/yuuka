package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/imap"
	"github.com/yuuka-mail/yuuka/internal/manage"
	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// Which accounts a viewer reads goes through the store with the viewer;
// the rest is configuration, read without one and written as the actor.

func (s *spy) VisibleAccounts(ctx context.Context, v *auth.Viewer) ([]store.VisibleAccount, error) {
	s.saw("VisibleAccounts", v)
	return s.DB.VisibleAccounts(ctx, v)
}

func (s *spy) AccountByName(ctx context.Context, name string) (store.Account, error) {
	s.saw("AccountByName", configRead)
	return s.DB.AccountByName(ctx, name)
}

func (s *spy) AccountAccess(ctx context.Context, name string) ([]store.AccessRow, error) {
	s.saw("AccountAccess", configRead)
	return s.DB.AccountAccess(ctx, name)
}

func (s *spy) SetAccountAccess(ctx context.Context, name, principalID string, defaultRead bool, was, canRead *bool, a store.Action) error {
	s.saw("SetAccountAccess", actor(a))
	return s.DB.SetAccountAccess(ctx, name, principalID, defaultRead, was, canRead, a)
}

func (s *spy) SetAccountDefaultRead(ctx context.Context, name string, was, defaultRead bool, a store.Action) error {
	s.saw("SetAccountDefaultRead", actor(a))
	return s.DB.SetAccountDefaultRead(ctx, name, was, defaultRead, a)
}

func (s *spy) SourceByName(ctx context.Context, name string) (store.Source, error) {
	s.saw("SourceByName", configRead)
	return s.DB.SourceByName(ctx, name)
}

func (s *spy) AddPullAccount(ctx context.Context, name string, c store.SourceConfig, sealed secret.Record, reader string, a store.Action) (store.Source, error) {
	s.saw("AddPullAccount", actor(a))
	return s.DB.AddPullAccount(ctx, name, c, sealed, reader, a)
}

func (s *spy) ConfigureSource(ctx context.Context, name string, c store.SourceConfig, revision int64, sealed *secret.Record, a store.Action) (store.Source, error) {
	s.saw("ConfigureSource", actor(a))
	return s.DB.ConfigureSource(ctx, name, c, revision, sealed, a)
}

func (s *spy) DisableSource(ctx context.Context, name string, revision int64, a store.Action) error {
	s.saw("DisableSource", actor(a))
	return s.DB.DisableSource(ctx, name, revision, a)
}

func (s *spy) PauseSource(ctx context.Context, name string, revision int64, reason string, a store.Action) error {
	s.saw("PauseSource", actor(a))
	return s.DB.PauseSource(ctx, name, revision, reason, a)
}

func (s *spy) ResumeSource(ctx context.Context, name string, revision int64, a store.Action) error {
	s.saw("ResumeSource", actor(a))
	return s.DB.ResumeSource(ctx, name, revision, a)
}

// RequestSync writes no action: like `yuuka account sync`, asking for a
// round changes nothing anyone decided.
func (s *spy) RequestSync(ctx context.Context, name string, retryFailed bool, at time.Time) error {
	s.saw("RequestSync", configRead)
	return s.DB.RequestSync(ctx, name, retryFailed, at)
}

// ProbeSource stands in for an IMAP server: the user "refused" is
// refused, anyone else logs in and finds one folder.
func (p *probeSpy) ProbeSource(_ context.Context, s store.Source, password string) (*manage.SourceProbe, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sources = append(p.sources, s.Username+":"+password)
	if s.Username == "refused@example.net" {
		return nil, &imap.Error{Kind: imap.KindAuth, Op: "AUTHENTICATE"}
	}
	return &manage.SourceProbe{Folders: []manage.FolderStatus{{
		Name: "[Gmail]/All Mail", SpecialUse: `\All`, MailboxStatus: imap.MailboxStatus{Messages: 12, UIDNext: 13, UIDValidity: 7},
	}}}, nil
}

// pullAccount adds pull account name for address as `yuuka account add`
// does, readable by the operator.
func (h *harness) pullAccount(name, address string) store.Source {
	h.t.Helper()
	c := manage.NewSourceConfig(store.PresetGmail)
	c.Address, c.Username = address, address
	sealed, err := manage.SealIMAPPassword(h.t.Context(), h.sealer, c, []byte("app-password"), time.Now())
	if err != nil {
		h.t.Fatal(err)
	}
	s, err := h.db.AddPullAccount(h.t.Context(), name, c, *sealed, h.human.ID, h.act("account.add"))
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

func TestAccountSettings(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()
	plain := h.tokenFor(h.human, auth.AllScopes...) // a session, not elevated
	s := h.elevated(h.human)
	const pw = "app-password-xyz"
	put := func(name, body, as string) *httptest.ResponseRecorder {
		return h.do(http.MethodPut, "/api/v1/admin/accounts/"+name, body, as)
	}

	// Adding always widens: an unelevated session is asked to elevate,
	// and nothing is written.
	add := `{"revision":0,"preset":"gmail","address":"Me@Gmail.example.com","password":"` + pw + `","backfill_since":"2026-10-01T00:00:00Z"}`
	if w := put("gmail", add, plain); w.Code != http.StatusForbidden || problemOf(t, w).Code != "elevation_required" {
		t.Fatalf("add unelevated: %d %s", w.Code, w.Body.String())
	}
	if _, err := h.db.SourceByName(ctx, "gmail"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unelevated add wrote: %v", err)
	}
	w := put("gmail", add, s)
	got := decode[ManagedAccount](t, w)
	src, accountID := got.Source, got.ID
	if w.Code != http.StatusOK || got.Kind != "imap" || got.DefaultRead || src == nil || src.State != "enabled" ||
		src.Host != manage.GmailHost || src.Port != 993 || src.Username != "me@gmail.example.com" || src.DailyByteBudget != manage.GmailDailyBudget ||
		src.Secret == nil || src.Secret.State != "ok" || src.BackfillSince == nil || len(got.Access) != 1 || got.Access[0].Principal != "human:operator" {
		t.Fatalf("add: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), pw) || h.actionsMention(pw) != 0 {
		t.Error("the password was answered or recorded")
	}
	if actor, cred, via := h.audit("account.add"); actor != h.human.ID || cred == "" || via != "api" {
		t.Errorf("recorded as %s with %s via %s", actor, cred, via)
	}
	if a := h.alerts.takeFull(); len(a) != 1 || a[0].Subject != h.human.ID || !strings.Contains(a[0].Text, "imap.gmail.com:993") {
		t.Errorf("alerts: %+v", a)
	}
	// Its address is claimed, as the CLI claims it.
	if n := h.count(`SELECT count(*) FROM prefixes WHERE zone = 'gmail.example.com' AND prefix = 'me' AND state = $1`, store.PrefixClaimed); n != 1 {
		t.Errorf("address claimed %d times", n)
	}
	// Its secret is listed with the others, never its value.
	sec := decode[SecretsStatus](t, h.get("/api/v1/admin/secrets", s))
	if len(sec.Secrets) != 1 || sec.Secrets[0].Owner != (SecretOwner{Kind: "account", Name: "gmail", Destination: "me@gmail.example.com@imap.gmail.com:993"}) ||
		sec.Secrets[0].State != "ok" {
		t.Errorf("secrets: %+v", sec)
	}

	body := func(rev int64, extra string) string {
		return `{"revision":` + strconv.FormatInt(rev, 10) + `,"preset":"gmail","address":"me@gmail.example.com","backfill_since":"2026-10-01T00:00:00Z"` + extra + `}`
	}
	// A name taken, and a change at an old revision, are refused.
	if w := put("gmail", add, s); w.Code != http.StatusConflict {
		t.Errorf("add over an existing one: %d %s", w.Code, w.Body.String())
	}
	if w := put("gmail", body(src.Revision+1, ""), s); w.Code != http.StatusConflict || !strings.Contains(problemOf(t, w).Detail, "changed since") {
		t.Errorf("stale: %d %s", w.Code, w.Body.String())
	}
	// Polling less often only tightens: no elevation, no alert.
	w = put("gmail", body(src.Revision, `,"poll_seconds":300`), plain)
	if w.Code != http.StatusOK || decode[ManagedAccount](t, w).Source.PollSeconds != 300 {
		t.Fatalf("tighten: %d %s", w.Code, w.Body.String())
	}
	if a := h.alerts.take(); len(a) != 0 {
		t.Errorf("a tightening alerted: %v", a)
	}
	src = decode[ManagedAccount](t, w).Source
	// Backfilling further, or trusting another receiver, widens.
	wider := body(src.Revision, `,"poll_seconds":300,"trusted_authserv_id":"mx.google.com"`)
	if w := put("gmail", wider, plain); w.Code != http.StatusForbidden || !strings.Contains(problemOf(t, w).Detail, "authserv-id") {
		t.Errorf("widen unelevated: %d %s", w.Code, w.Body.String())
	}
	if w := put("gmail", wider, s); w.Code != http.StatusOK {
		t.Fatalf("widen: %d %s", w.Code, w.Body.String())
	} else {
		src = decode[ManagedAccount](t, w).Source
	}
	if a := h.alerts.take(); len(a) != 1 || !strings.Contains(a[0], "authserv-id") {
		t.Errorf("widen alerts: %v", a)
	}
	// Another host needs the password again.
	if w := put("gmail", `{"revision":`+strconv.FormatInt(src.Revision, 10)+`,"address":"me@gmail.example.com","host":"imap.example.net"}`, s); w.Code != http.StatusBadRequest ||
		!strings.Contains(problemOf(t, w).Detail, "give it again") {
		t.Errorf("move without the password: %d %s", w.Code, w.Body.String())
	}

	// Testing opens the password for the destination the row names and
	// fetches nothing; it is not recorded, as on the host.
	before := h.count(`SELECT count(*) FROM actions`)
	probe := decode[AccountProbe](t, h.do(http.MethodPost, "/api/v1/admin/accounts/gmail/test", "", plain))
	if !probe.OK || len(probe.Folders) != 1 || probe.Folders[0].SpecialUse != `\All` || probe.LoginRefused {
		t.Errorf("test: %+v", probe)
	}
	if got := h.probes.sources; len(got) != 1 || got[0] != "me@gmail.example.com:"+"app-password-xyz" {
		t.Errorf("probed with %v", got)
	}
	if h.count(`SELECT count(*) FROM actions`) != before {
		t.Error("testing was recorded")
	}

	// Sync asks serve for a round; pause and removal tighten.
	if w := h.do(http.MethodPost, "/api/v1/admin/accounts/gmail/sync", `{"retry_failed":true}`, plain); w.Code != http.StatusNoContent {
		t.Errorf("sync: %d %s", w.Code, w.Body.String())
	}
	if req, err := h.db.TakeSyncRequest(ctx, accountID); err != nil || !req.Sync || !req.RetryFailed {
		t.Errorf("sync request %+v %v", req, err)
	}
	rev := `{"revision":` + strconv.FormatInt(src.Revision, 10) + `}`
	w = h.do(http.MethodPost, "/api/v1/admin/accounts/gmail/pause", rev, plain)
	if got := decode[ManagedAccount](t, w); w.Code != http.StatusOK || got.Source.State != "paused" {
		t.Fatalf("pause: %d %s", w.Code, w.Body.String())
	} else {
		src = got.Source
	}
	rev = `{"revision":` + strconv.FormatInt(src.Revision, 10) + `}`
	if w := h.do(http.MethodPost, "/api/v1/admin/accounts/gmail/resume", rev, plain); w.Code != http.StatusForbidden {
		t.Errorf("resume unelevated: %d", w.Code)
	}
	w = h.do(http.MethodPost, "/api/v1/admin/accounts/gmail/resume", rev, s)
	if got := decode[ManagedAccount](t, w); w.Code != http.StatusOK || got.Source.State != "enabled" {
		t.Fatalf("resume: %d %s", w.Code, w.Body.String())
	} else {
		src = got.Source
	}
	if a := h.alerts.take(); len(a) != 1 || !strings.Contains(a[0], "resumed") {
		t.Errorf("resume alerts: %v", a)
	}

	// Who reads it: letting the agent read widens; taking it back
	// tightens. The account's default the same.
	access := func(principal, setting, as string) *httptest.ResponseRecorder {
		return h.do(http.MethodPut, "/api/v1/admin/accounts/gmail/access/"+principal, `{"access":"`+setting+`"}`, as)
	}
	if w := access("agent:arisu", "read", plain); w.Code != http.StatusForbidden || problemOf(t, w).Code != "elevation_required" {
		t.Errorf("read unelevated: %d %s", w.Code, w.Body.String())
	}
	if w := access("agent:arisu", "read", s); w.Code != http.StatusOK {
		t.Fatalf("read: %d %s", w.Code, w.Body.String())
	}
	if a := h.alerts.take(); len(a) != 1 || !strings.Contains(a[0], "agent:arisu read account gmail") {
		t.Errorf("access alerts: %v", a)
	}
	readable := func(tok string) []ReadableAccount {
		return decode[AccountList](t, h.get("/api/v1/accounts", tok)).Accounts
	}
	if got := readable(h.token(auth.MessageRead)); len(got) != 2 || got[1] != (ReadableAccount{Name: "gmail", Kind: "imap", Address: "me@gmail.example.com"}) {
		t.Errorf("arisu reads %+v", got)
	}
	if w := access("agent:arisu", "no_read", plain); w.Code != http.StatusOK {
		t.Fatalf("no_read: %d %s", w.Code, w.Body.String())
	}
	if got := readable(h.token(auth.MessageRead)); len(got) != 1 || got[0].Name != "devseed" {
		t.Errorf("arisu reads %+v after no_read", got)
	}
	// A principal that does not exist yet gets no "default".
	if w := access("agent:nobody", "default", plain); w.Code != http.StatusNotFound {
		t.Errorf("default for nobody: %d", w.Code)
	}
	if w := h.do(http.MethodPut, "/api/v1/admin/accounts/gmail/default-read", `{"default_read":true}`, plain); w.Code != http.StatusForbidden {
		t.Errorf("default readable unelevated: %d", w.Code)
	}
	w = h.do(http.MethodPut, "/api/v1/admin/accounts/gmail/default-read", `{"default_read":true}`, s)
	if got := decode[ManagedAccount](t, w); w.Code != http.StatusOK || !got.DefaultRead {
		t.Errorf("default readable: %d %s", w.Code, w.Body.String())
	}
	if got := readable(h.token(auth.MessageRead)); len(got) != 1 {
		t.Errorf("arisu's own no_read outlasts the default: %+v", got)
	}
	h.alerts.take()
	if w := h.do(http.MethodPut, "/api/v1/admin/accounts/devseed/default-read", `{"default_read":false}`, plain); w.Code != http.StatusOK {
		t.Errorf("default unreadable: %d %s", w.Code, w.Body.String())
	}
	if a := h.alerts.take(); len(a) != 0 {
		t.Errorf("a tightening alerted: %v", a)
	}

	// Removing erases the password and keeps the account.
	w = h.do(http.MethodPost, "/api/v1/admin/accounts/gmail/disable", `{"revision":`+strconv.FormatInt(src.Revision, 10)+`}`, plain)
	if got := decode[ManagedAccount](t, w); w.Code != http.StatusOK || got.Source.State != "removed" || got.Source.Secret != nil {
		t.Fatalf("disable: %d %s", w.Code, w.Body.String())
	}
	if w := put("gmail", body(src.Revision+1, ""), s); w.Code != http.StatusConflict || !strings.Contains(problemOf(t, w).Detail, "removed") {
		t.Errorf("change a removed one: %d %s", w.Code, w.Body.String())
	}
	list := decode[AdminAccountList](t, h.get("/api/v1/admin/accounts", plain))
	if len(list.Accounts) != 2 || list.Accounts[0].Name != "devseed" || list.Accounts[0].Source != nil || list.Accounts[1].Source.State != "removed" {
		t.Errorf("list: %+v", list)
	}
}

// A refused login says so, so the settings can tell the person to check
// the password (Gmail wants an app password) before retrying.
func TestAccountTestRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.pullAccount("work", "refused@example.net")
	probe := decode[AccountProbe](t, h.do(http.MethodPost, "/api/v1/admin/accounts/work/test", "", h.tokenFor(h.human, auth.AllScopes...)))
	if probe.OK || !probe.LoginRefused || probe.Error == "" {
		t.Errorf("refused: %+v", probe)
	}
	if w := h.do(http.MethodPost, "/api/v1/admin/accounts/devseed/test", "", h.tokenFor(h.human, auth.AllScopes...)); w.Code != http.StatusNotFound {
		t.Errorf("a catch-all has no source to test: %d", w.Code)
	}
}

// Deliveries name their account (§9.4) wherever a delivery is shown.
func TestDeliveryAccount(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	page := decode[MessagePage](t, h.get("/api/v1/messages?limit=1", h.token(auth.MessageRead)))
	m := decode[Message](t, h.get("/api/v1/messages/"+page.Messages[0].ID, h.token(auth.MessageRead, auth.DeliveryRead)))
	if len(m.Deliveries) == 0 || m.Deliveries[0].Account != "devseed" || m.Deliveries[0].AccountKind != "cf_catchall" || m.Deliveries[0].Omitted != "" {
		t.Errorf("deliveries %+v", m.Deliveries)
	}
}
