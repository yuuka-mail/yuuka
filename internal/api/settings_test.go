package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/openapi"
	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// credential stores a credential of kind for p with exactly scopes and
// returns its secret, bypassing the issuing rules: a token for a human or
// a session for an agent, which nothing issues, are what an attacker who
// edits the table, or a future bug, would hold.
func (h *harness) credential(p store.Principal, kind string, scopes ...auth.Scope) string {
	h.t.Helper()
	prefix := map[string]string{auth.KindSession: auth.PrefixSession, auth.KindToken: auth.PrefixToken}[kind]
	now := time.Now()
	expires := now.Add(time.Hour)
	sec := auth.NewSecret(prefix, ulid.New(now))
	if err := h.db.CreateCredential(h.t.Context(), store.Credential{
		ID: sec.ID, PrincipalID: p.ID, Kind: kind, Label: "forged", SecretHash: sec.Hash[:],
		Scopes: auth.Strings(scopes), CreatedAt: now, ExpiresAt: &expires,
	}, store.Action{ID: ulid.New(now), Actor: store.SystemPrincipalID, Tool: "test", At: now}); err != nil {
		h.t.Fatal(err)
	}
	return sec.Text
}

// identity adds addr, owned by owner, as `yuuka identity add` does.
func (h *harness) identity(addr string, owner store.Principal) store.Identity {
	h.t.Helper()
	ctx := h.t.Context()
	account, err := h.db.EnsureCloudflareAccount(ctx, "devseed")
	if err != nil {
		h.t.Fatal(err)
	}
	zone, prefix, err := store.SplitIdentityAddress(addr)
	if err != nil {
		h.t.Fatal(err)
	}
	at := time.Now()
	i := store.Identity{ID: ulid.New(at), AccountID: account, Zone: zone, Prefix: prefix, Transport: "cf", OwnerID: owner.ID, CreatedAt: at}
	if err := h.db.AddIdentity(ctx, i, store.Action{ID: ulid.New(at), Actor: store.SystemPrincipalID, Tool: "identity.add", At: at}); err != nil {
		h.t.Fatal(err)
	}
	return i
}

// identityAt is the enabled identity at addr.
func (h *harness) identityAt(addr string) store.Identity {
	h.t.Helper()
	zone, prefix, _ := store.SplitIdentityAddress(addr)
	i, err := h.db.IdentityAt(h.t.Context(), zone, prefix)
	if err != nil {
		h.t.Fatal(err)
	}
	return i
}

// grant writes g as `yuuka grant set` does.
func (h *harness) grant(g store.Grant) {
	h.t.Helper()
	at := time.Now()
	if g.Allowlist == nil {
		g.Allowlist = []string{}
	}
	if err := h.db.SetGrant(h.t.Context(), g, store.Action{ID: ulid.New(at), Actor: store.SystemPrincipalID, Tool: "grant.set", At: at}); err != nil {
		h.t.Fatal(err)
	}
}

// claim claims prefix@example.com as `yuuka prefix claim` does.
func (h *harness) claim(prefix string) {
	h.t.Helper()
	at := time.Now()
	if _, err := h.db.SetPrefixState(h.t.Context(), "example.com", prefix, store.PrefixClaimed,
		store.Action{ID: ulid.New(at), Actor: store.SystemPrincipalID, Tool: "prefix.claim", At: at}); err != nil {
		h.t.Fatal(err)
	}
}

// audit is the newest action of tool: who, through which credential,
// and via what.
func (h *harness) audit(tool string) (actor, credential, via string) {
	h.t.Helper()
	err := h.pool.QueryRow(h.t.Context(), `
		SELECT coalesce(actor, ''), coalesce(credential_id, ''), coalesce(args->>'via', '')
		FROM actions WHERE tool = $1 ORDER BY id DESC LIMIT 1`, tool).Scan(&actor, &credential, &via)
	if err != nil {
		h.t.Fatalf("no %s action: %v", tool, err)
	}
	return actor, credential, via
}

func TestPrefixDecisions(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tok := h.token(auth.PrefixManage, auth.PrefixRead)
	tokID, _, _ := auth.ParseSecret(auth.PrefixToken, tok)
	send := func(method, path, body string) *httptest.ResponseRecorder { return h.do(method, path, body, tok) }

	// Unclaiming github@ downgrades its mail again; the answer is the
	// prefix as every view now shows it.
	before := decode[PrefixDetail](t, h.get("/api/v1/prefixes/github@example.com", h.token(auth.PrefixRead, auth.MessageRead)))
	w := send(http.MethodPost, "/api/v1/prefixes/github@example.com/unclaim", "")
	d := decode[PrefixDecision](t, w)
	if w.Code != http.StatusOK || d.Outcome != "updated" || d.Prefix == nil || d.Prefix.Claimed || d.Prefix.Downgraded != d.Prefix.Messages ||
		d.Prefix.Messages != before.Messages {
		t.Errorf("unclaim: %d %+v", w.Code, d)
	}
	if actor, cred, via := h.audit("prefix.unclaim"); actor != h.agent.ID || cred != tokID || via != "api" {
		t.Errorf("recorded as %s with %s via %s", actor, cred, via)
	}
	if d := decode[PrefixDecision](t, send(http.MethodPost, "/api/v1/prefixes/github@example.com/unclaim", "")); d.Outcome != "unchanged" {
		t.Errorf("unclaim twice: %s", d.Outcome)
	}
	// Expecting needs a claimed prefix.
	if w := send(http.MethodPut, "/api/v1/prefixes/github@example.com/expected-domains", `{"domains":[]}`); w.Code != http.StatusConflict {
		t.Errorf("expect on a discovered prefix: %d %s", w.Code, w.Body.String())
	}
	if d := decode[PrefixDecision](t, send(http.MethodPost, "/api/v1/prefixes/GitHub@Example.com/claim", "")); d.Outcome != "updated" || !d.Prefix.Claimed {
		t.Errorf("claim: %+v", d)
	}

	// A new expectation judges the mail already there.
	w = send(http.MethodPut, "/api/v1/prefixes/github@example.com/expected-domains", `{"domains":["mail.example.org","example.net"]}`)
	x := decode[ExpectResult](t, w)
	if w.Code != http.StatusOK || !x.Changed || !slices.Equal(x.Expected, []string{"example.net", "example.org"}) ||
		!slices.Equal(x.CountedAs, []CountedAs{{Domain: "mail.example.org", As: "example.org"}}) ||
		x.Prefix == nil || !slices.Equal(x.Prefix.ExpectedDomains, x.Expected) {
		t.Errorf("expect: %d %+v", w.Code, x)
	}
	if x := decode[ExpectResult](t, send(http.MethodPut, "/api/v1/prefixes/github@example.com/expected-domains",
		`{"domains":["example.org","example.net"]}`)); x.Changed {
		t.Error("the same domains changed something")
	}
	for _, body := range []string{`{"domains":["com"]}`, `{"domains":["a@example.net"]}`} {
		if w := send(http.MethodPut, "/api/v1/prefixes/github@example.com/expected-domains", body); w.Code != http.StatusBadRequest ||
			problemOf(t, w).Code != openapi.CodeInvalidBody {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}

	// A prefix nobody has written to enters the registry and leaves it.
	if d := decode[PrefixDecision](t, send(http.MethodPost, "/api/v1/prefixes/new@example.com/claim", "")); d.Outcome != "created" || d.Prefix == nil || d.Prefix.Messages != 0 {
		t.Errorf("claim a new prefix: %+v", d)
	}
	if d := decode[PrefixDecision](t, send(http.MethodPost, "/api/v1/prefixes/new@example.com/unclaim", "")); d.Outcome != "removed" || d.Prefix != nil {
		t.Errorf("unclaim a new prefix: %+v", d)
	}
	if w := send(http.MethodPost, "/api/v1/prefixes/nobody@example.com/unclaim", ""); w.Code != http.StatusNotFound {
		t.Errorf("unclaim an unknown prefix: %d", w.Code)
	}
	for _, addr := range []string{"no-at-sign", "a%20b@example.com", "@example.com"} {
		if w := send(http.MethodPost, "/api/v1/prefixes/"+addr+"/claim", ""); w.Code != http.StatusBadRequest ||
			problemOf(t, w).Code != openapi.CodeInvalidParam {
			t.Errorf("claim %s: %d %s", addr, w.Code, w.Body.String())
		}
	}
}

func TestSettings(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	session := h.tokenFor(h.human, auth.AllScopes...)
	sessionID, _, _ := auth.ParseSecret(auth.PrefixSession, session)
	send := func(method, path, body string) *httptest.ResponseRecorder { return h.do(method, path, body, session) }
	refused := func(w *httptest.ResponseRecorder, cmd string) {
		t.Helper()
		if w.Code != http.StatusForbidden {
			t.Errorf("%d %s, want 403", w.Code, w.Body.String())
			return
		}
		if p := problemOf(t, w); p.Code != openapi.CodeWideningRequiresCLI || !strings.Contains(p.Detail, cmd) {
			t.Errorf("%s: %q, want the command %q", p.Code, p.Detail, cmd)
		}
	}
	arisuID, star := h.identityAt("arisu@example.com"), h.identityAt("*@example.com")
	n := 20
	h.grant(store.Grant{
		IdentityID: arisuID.ID, PrincipalID: h.agent.ID, Policy: sendpolicy.Auto,
		RecipientScope: sendpolicy.ScopeAllowlist, Allowlist: []string{"a@example.net", "example.org"}, MaxPerDay: &n,
	})
	h.grant(store.Grant{IdentityID: star.ID, PrincipalID: h.agent.ID, Policy: sendpolicy.Draft, RecipientScope: sendpolicy.ScopeThread})
	grantPath := "/api/v1/admin/grants/" + arisuID.ID + "/" + h.agent.ID

	// The list says what applies and what removing a row would do.
	list := decode[GrantAdminList](t, send(http.MethodGet, "/api/v1/admin/grants", ""))
	var own, onStar *ManagedGrant
	for i, g := range list.Grants {
		switch g.Identity.Address {
		case "arisu@example.com":
			own = &list.Grants[i]
		case "*@example.com":
			onStar = &list.Grants[i]
		}
	}
	if own == nil || onStar == nil {
		t.Fatalf("grants: %+v", list.Grants)
	}
	if own.Stored.Limits.MaxPerHour != nil || *own.Applied.Limits.MaxPerHour != 10 || *own.Applied.Limits.MaxPerDay != 20 ||
		own.Default.SendPolicy != "review" || !slices.Equal(own.RemovalWidens, []string{"max_per_day"}) || own.OwnerCapped ||
		own.Host != "yuuka grant set --identity arisu@example.com --principal agent:arisu" {
		t.Errorf("arisu's own grant: %+v", own)
	}
	if !onStar.OwnerCapped || onStar.Applied.SendPolicy != "draft" || len(onStar.RemovalWidens) != 0 ||
		!strings.Contains(onStar.Host, "--identity '*@example.com'") {
		t.Errorf("arisu on the operator's identity: %+v", onStar)
	}

	// Tightening: one step, recorded as the person with the session.
	w := send(http.MethodPost, grantPath+"/tighten", `{"send_policy":"review","allowlist":["example.org"],"max_per_hour":3}`)
	g := decode[ManagedGrant](t, w)
	if w.Code != http.StatusOK || g.Applied.SendPolicy != "review" || !slices.Equal(g.Applied.Allowlist, []string{"example.org"}) ||
		*g.Applied.Limits.MaxPerHour != 3 {
		t.Errorf("tighten: %d %s", w.Code, w.Body.String())
	}
	if actor, cred, via := h.audit("grant.tighten"); actor != h.human.ID || cred != sessionID || via != "api" {
		t.Errorf("recorded as %s with %s via %s", actor, cred, via)
	}
	// Widening is refused and named as the host command that does it.
	base := "yuuka grant set --identity arisu@example.com --principal agent:arisu"
	refused(send(http.MethodPost, grantPath+"/tighten", `{"send_policy":"auto"}`), base+" --policy auto")
	refused(send(http.MethodPost, grantPath+"/tighten", `{"recipient_scope":"any"}`), base+" --recipients any")
	refused(send(http.MethodPost, grantPath+"/tighten", `{"allowlist":["example.org","b@example.net"]}`), base+" --allow example.org,b@example.net")
	refused(send(http.MethodPost, grantPath+"/tighten", `{"max_per_hour":4,"max_per_day":21}`), base+" --max-per-hour 4 --max-per-day 21")
	refused(send(http.MethodPost, grantPath+"/tighten", `{"max_new_recipients_per_day":6}`), base+" --max-new-recipients-per-day 6")
	if w := send(http.MethodPost, grantPath+"/tighten", `{"suspended":true}`); w.Code != http.StatusOK || !decode[ManagedGrant](t, w).Applied.Suspended {
		t.Errorf("suspend: %d %s", w.Code, w.Body.String())
	}
	refused(send(http.MethodPost, grantPath+"/tighten", `{"suspended":false}`), base+" --resume")
	for _, body := range []string{`{"max_per_hour":-1}`, `{"send_policy":"sometimes"}`, `{"hold_seconds":600}`} {
		if w := send(http.MethodPost, grantPath+"/tighten", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	// The owner cap holds here as on the host.
	if w := send(http.MethodPost, "/api/v1/admin/grants/"+star.ID+"/"+h.agent.ID+"/tighten", `{"send_policy":"auto"}`); w.Code != http.StatusConflict {
		t.Errorf("auto on the operator's identity: %d %s", w.Code, w.Body.String())
	}
	if g, _ := h.db.GrantFor(t.Context(), star.ID, h.agent.ID); g.Policy != sendpolicy.Draft {
		t.Errorf("the capped grant became %s", g.Policy)
	}

	// Removing: allowed where the default is no wider.
	refused(send(http.MethodDelete, grantPath, ""), "yuuka grant rm --identity arisu@example.com --principal agent:arisu")
	if w := send(http.MethodDelete, "/api/v1/admin/grants/"+star.ID+"/"+h.agent.ID, ""); w.Code != http.StatusNoContent {
		t.Errorf("remove: %d %s", w.Code, w.Body.String())
	}
	if w := send(http.MethodDelete, "/api/v1/admin/grants/"+star.ID+"/"+h.agent.ID, ""); w.Code != http.StatusNotFound {
		t.Errorf("remove twice: %d", w.Code)
	}
	if w := send(http.MethodPost, "/api/v1/admin/grants/"+star.ID+"/"+h.agent.ID+"/tighten", `{"send_policy":"none"}`); w.Code != http.StatusNotFound {
		t.Errorf("tighten a default: %d", w.Code)
	}

	// Credentials: listed without secrets, revoked at once.
	tok := h.token(auth.MessageRead)
	tokID, _, _ := auth.ParseSecret(auth.PrefixToken, tok)
	w = send(http.MethodGet, "/api/v1/admin/credentials", "")
	if strings.Contains(w.Body.String(), strings.TrimPrefix(tok, auth.PrefixToken+"_"+tokID)) || strings.Contains(w.Body.String(), "hash") {
		t.Error("a secret in the list")
	}
	creds := decode[CredentialList](t, w).Credentials
	i := slices.IndexFunc(creds, func(c ManagedCredential) bool { return c.ID == tokID })
	if i < 0 || creds[i].Kind != "token" || creds[i].Principal.Name != "arisu" || !slices.Equal(creds[i].Scopes, []auth.Scope{auth.MessageRead}) ||
		creds[i].RevokedAt != nil {
		t.Fatalf("credentials: %+v", creds)
	}
	if slices.ContainsFunc(creds, func(c ManagedCredential) bool { return c.ID == sessionID }) {
		t.Error("a session listed")
	}
	if w := send(http.MethodPost, "/api/v1/admin/credentials/"+tokID+"/revoke", ""); w.Code != http.StatusNoContent {
		t.Errorf("revoke: %d %s", w.Code, w.Body.String())
	}
	if actor, cred, _ := h.audit("auth.credential.revoke"); actor != h.human.ID || cred != sessionID {
		t.Errorf("revocation recorded as %s with %s", actor, cred)
	}
	if w := h.get("/api/v1/labels", tok); w.Code != http.StatusUnauthorized {
		t.Errorf("a revoked token: %d", w.Code)
	}
	for _, id := range []string{tokID, sessionID} {
		if w := send(http.MethodPost, "/api/v1/admin/credentials/"+id+"/revoke", ""); w.Code != http.StatusNotFound {
			t.Errorf("revoke %s: %d", id, w.Code)
		}
	}

	// Identities: an address the zone's pattern would take over is the
	// host's to disable.
	ops := h.identity("ops@example.com", h.human)
	bot := h.identity("bot@example.org", h.agent)
	ids := decode[IdentityAdminList](t, send(http.MethodGet, "/api/v1/admin/identities", "")).Identities
	byAddr := map[string]ManagedIdentity{}
	for _, i := range ids {
		byAddr[i.Address] = i
	}
	if byAddr["ops@example.com"].TakenOverBy != "*@example.com" || byAddr["bot@example.org"].TakenOverBy != "" ||
		byAddr["*@example.com"].TakenOverBy != "" || byAddr["*@example.com"].Host != "yuuka identity rm '*@example.com'" {
		t.Errorf("identities: %+v", byAddr)
	}
	refused(send(http.MethodPost, "/api/v1/admin/identities/"+ops.ID+"/disable", ""), "yuuka identity rm ops@example.com")
	if w := send(http.MethodPost, "/api/v1/admin/identities/"+bot.ID+"/disable", ""); w.Code != http.StatusNoContent {
		t.Errorf("disable: %d %s", w.Code, w.Body.String())
	}
	if actor, cred, _ := h.audit("identity.disable"); actor != h.human.ID || cred != sessionID {
		t.Errorf("disabling recorded as %s with %s", actor, cred)
	}
	if w := send(http.MethodPost, "/api/v1/admin/identities/"+bot.ID+"/disable", ""); w.Code != http.StatusNotFound {
		t.Errorf("disable twice: %d", w.Code)
	}
}
