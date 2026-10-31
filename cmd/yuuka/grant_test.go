package main

import (
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authn"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// The sending configuration from the host: identities, grants with the
// owner cap refused, SMTP passwords; every change in the audit log.
func TestIdentityAndGrant(t *testing.T) {
	t.Parallel()
	e := newHostEnv(t)
	ctx := t.Context()

	if _, _, err := e.run("identity", "add", "*@example.com", "--owner", "human:operator", "--transport", "cf"); err == nil ||
		!strings.Contains(err.Error(), "no account") {
		t.Errorf("before any account: %v", err)
	}
	storetest.Account(t, e.db)
	if _, _, err := e.run("identity", "add", "*@example.com", "--owner", "human:operator", "--transport", "cf"); err == nil ||
		!strings.Contains(err.Error(), "admin enroll") {
		t.Errorf("unknown human: %v", err)
	}
	now := time.Now()
	if _, err := e.db.EnsurePrincipal(ctx, store.Principal{ID: ulid.New(now), Kind: "human", Name: "operator", DisplayName: "operator", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]string{
		{"add", "*@example.com", "--owner", "human:operator"},
		{"add", "*@example.com", "--owner", "human:operator", "--transport", "c f"},
		// Neither in YUUKA_TRANSPORTS nor in the database: a typo does
		// not become a placeholder.
		{"add", "*@example.com", "--owner", "human:operator", "--transport", "nope"},
		{"add", "*@example.com", "--owner", "system:system", "--transport", "cf"},
		{"add", "example.com", "--owner", "human:operator", "--transport", "cf"},
		{"add", "*@example.com", "--owner", "human:operator", "--transport", "cf", "--account", "nope"},
		{"rm", "nobody@example.com"},
	} {
		if _, _, err := e.run("identity", bad...); err == nil {
			t.Errorf("identity %v: no error", bad)
		}
	}
	// Flags after the address work too.
	if out := e.must("identity", "add", "*@Example.com", "--owner", "human:operator", "--transport", "cf", "--display-name", "Operator"); !strings.Contains(out, "added *@example.com") {
		t.Errorf("add: %q", out)
	}
	if _, _, err := e.run("identity", "add", "*@example.com", "--owner", "human:operator", "--transport", "cf"); err == nil {
		t.Error("duplicate")
	}
	_, notes, err := e.run("identity", "add", "--owner", "agent:arisu", "--transport", "cf", "--signature", "-- arisu", "arisu@example.com")
	if err != nil || !strings.Contains(notes, "created principal agent:arisu") {
		t.Fatalf("add arisu@: %q %v", notes, err)
	}
	e.must("identity", "add", "alerts@example.com", "--owner", "human:operator", "--transport", "cf")
	e.must("identity", "add", "cron@example.com", "--owner", "service:scheduler", "--transport", "cf")
	ls := e.must("identity", "ls")
	if strings.Count(ls, "\n") != 5 || !strings.Contains(ls, "agent:arisu") || !strings.Contains(ls, "Operator") {
		t.Errorf("ls:\n%s", ls)
	}
	if _, _, args := e.lastAction("identity.add"); args["address"] != "cron@example.com" || args["owner"] != "service:scheduler" || args["via"] != "cli" {
		t.Errorf("identity.add recorded %v", args)
	}

	// The owner cap is refused.
	for _, bad := range [][]string{
		{"set", "--identity", "*@example.com", "--principal", "agent:arisu", "--policy", "auto"},
		{"set", "--identity", "alerts@example.com", "--principal", "service:monitoring", "--policy", "review"},
		{"set", "--identity", "nobody@example.com", "--principal", "agent:arisu", "--policy", "draft"},
		{"set", "--identity", "arisu@example.com", "--principal", "agent:arisu", "--policy", "sometimes"},
		{"set", "--identity", "arisu@example.com", "--principal", "agent:arisu", "--max-per-hour", "-1"},
		{"set", "--identity", "arisu@example.com", "--principal", "agent:arisu", "--allow", "not an address"},
		{"set", "--identity", "arisu@example.com", "--principal", "agent:arisu", "--suspend", "--resume"},
		{"rm", "--identity", "arisu@example.com", "--principal", "agent:arisu"},
	} {
		if _, _, err := e.run("grant", bad...); err == nil {
			t.Errorf("grant %v: no error", bad)
		}
	}
	if _, _, err := e.run("grant", "set", "--identity", "*@example.com", "--principal", "agent:arisu", "--policy", "auto"); err == nil ||
		!strings.Contains(err.Error(), "draft at most") {
		t.Errorf("cap message: %v", err)
	}
	if out := e.must("grant", "set", "--identity", "*@example.com", "--principal", "agent:arisu", "--policy", "draft"); !strings.Contains(out, "draft, recipients thread") {
		t.Errorf("draft grant: %q", out)
	}
	out := e.must("grant", "set", "--identity", "cron@example.com", "--principal", "service:scheduler", "--policy", "auto",
		"--recipients", "allowlist", "--allow", "Ops@example.net, example.org", "--max-per-hour", "100")
	if !strings.Contains(out, "auto, recipients allowlist [ops@example.net,example.org], 100/hour, 200 (default)/day") {
		t.Errorf("service grant: %q", out)
	}
	// set changes only what it is given.
	out = e.must("grant", "set", "--identity", "cron@example.com", "--principal", "service:scheduler", "--max-per-hour", "default", "--suspend")
	if !strings.Contains(out, "auto, recipients allowlist [ops@example.net,example.org], 30 (default)/hour") || !strings.Contains(out, "SUSPENDED") {
		t.Errorf("patched grant: %q", out)
	}
	if out := e.must("grant", "set", "--identity", "cron@example.com", "--principal", "service:scheduler", "--resume"); strings.Contains(out, "SUSPENDED") {
		t.Errorf("resumed grant: %q", out)
	}
	_, notes, err = e.run("grant", "set", "--identity", "arisu@example.com", "--principal", "agent:arisu", "--policy", "review")
	if err != nil || !strings.Contains(notes, "recall window") {
		t.Errorf("review note: %q %v", notes, err)
	}
	if _, _, args := e.lastAction("grant.set"); args["policy"] != "review" || args["identity"] != "arisu@example.com" || args["via"] != "cli" {
		t.Errorf("grant.set recorded %v", args)
	}
	// The recall window: the default until a number is given, 0 for none.
	for flag, want := range map[string]string{"0": ", hold 0s", "90": ", hold 90s", "default": ", hold 600s (default)"} {
		if out := e.must("grant", "set", "--identity", "arisu@example.com", "--principal", "agent:arisu", "--hold-seconds", flag); !strings.Contains(out, want) {
			t.Errorf("--hold-seconds %s: %q", flag, out)
		}
	}
	if _, _, err := e.run("grant", "set", "--identity", "arisu@example.com", "--principal", "agent:arisu", "--hold-seconds", "-1"); err == nil {
		t.Error("--hold-seconds -1 accepted")
	}
	ls = e.must("grant", "ls")
	if strings.Count(ls, "\n") != 4 {
		t.Errorf("grant ls:\n%s", ls)
	}
	if ls := e.must("grant", "ls", "--principal", "agent:arisu"); strings.Count(ls, "\n") != 3 {
		t.Errorf("grant ls --principal:\n%s", ls)
	}
	if out := e.must("grant", "rm", "--identity", "*@example.com", "--principal", "agent:arisu"); !strings.Contains(out, "default applies: draft") {
		t.Errorf("rm: %q", out)
	}
	if _, _, args := e.lastAction("grant.remove"); args["principal"] != "agent:arisu" {
		t.Errorf("grant.remove recorded %v", args)
	}

	// rm disables; the grant stays pointing at it.
	e.must("identity", "rm", "arisu@example.com")
	if ls := e.must("identity", "ls"); strings.Contains(ls, "arisu@") {
		t.Errorf("ls after rm:\n%s", ls)
	}
	if ls := e.must("identity", "ls", "--all"); !strings.Contains(ls, "arisu@") {
		t.Errorf("ls --all after rm:\n%s", ls)
	}
	if _, _, err := e.run("identity", "rm", "arisu@example.com"); err == nil {
		t.Error("rm twice")
	}
}

func TestSMTPPasswordIssue(t *testing.T) {
	t.Parallel()
	e := newHostEnv(t)
	for _, bad := range [][]string{
		{"issue", "--kind", "smtp", "--principal", "service:monitoring", "--scope", "message:read"},
		{"issue", "--kind", "smtp", "--principal", "human:operator"},
		{"issue", "--kind", "imap", "--principal", "service:monitoring"},
	} {
		if _, _, err := e.run("token", bad...); err == nil {
			t.Errorf("token %v: no error", bad)
		}
	}
	pw, notes, err := e.run("token", "issue", "--kind", "smtp", "--principal", "service:monitoring", "--label", "nas")
	if err != nil {
		t.Fatal(err)
	}
	pw = strings.TrimSpace(pw)
	user, _, ok := auth.ParseSecret(auth.PrefixSMTP, pw)
	if !ok || !strings.Contains(notes, "SMTP user name:\n  "+user+"\n") {
		t.Fatalf("issue: %q %q", pw, notes)
	}
	if _, err := authn.NewBearer(e.db, quiet, nil).AuthenticateSMTP(t.Context(), user, pw, "test"); err != nil {
		t.Errorf("authenticate: %v", err)
	}
	if ls := strings.Join(strings.Fields(e.must("token", "ls")), " "); !strings.Contains(ls, user+" smtp service:monitoring nas submission:send") {
		t.Errorf("ls:\n%s", ls)
	}
	if _, _, args := e.lastAction("auth.smtp_password.issue"); args["via"] != "cli" {
		t.Errorf("recorded %v", args)
	}
	e.must("token", "revoke", user)
	if _, err := authn.NewBearer(e.db, quiet, nil).AuthenticateSMTP(t.Context(), user, pw, "test"); err == nil {
		t.Error("revoked password works")
	}
}
