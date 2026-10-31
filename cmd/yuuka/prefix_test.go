package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/yuuka-mail/yuuka/internal/store"
)

func TestPrefixExpect(t *testing.T) {
	t.Parallel()
	e := newHostEnv(t)
	e.must("prefix", "claim", "github@example.com")
	out, msg, err := e.run("prefix", "expect", "GitHub@example.com", "--domains", "mail.example.net, example.org")
	if err != nil || !strings.Contains(out, "github@example.com expects example.net, example.org") ||
		!strings.Contains(msg, "mail.example.net counts as example.net") {
		t.Errorf("expect: %q %q %v", out, msg, err)
	}
	_, _, args := e.lastAction("prefix.expect")
	if args["via"] != "cli" || args["prefix"] != "github" || fmt.Sprint(args["expected_domains"]) != "[example.net example.org]" {
		t.Errorf("recorded %v", args)
	}
	// Flags first works too; the same list changes nothing.
	if out := e.must("prefix", "expect", "--domains", "example.org,example.net", "github@example.com"); !strings.Contains(out, "already expects") {
		t.Errorf("expect again: %q", out)
	}
	if out := e.must("prefix", "ls"); !strings.Contains(out, "EXPECTED") || !strings.Contains(out, "example.net,example.org") {
		t.Errorf("ls:\n%s", out)
	}
	if out := e.must("prefix", "expect", "github@example.com", "--domains", ""); !strings.Contains(out, "expects nothing") {
		t.Errorf("clear: %q", out)
	}
	if _, err := e.pool.Exec(t.Context(), `INSERT INTO prefixes (zone, prefix, first_seen, last_seen) VALUES ('example.com', 'random', now(), now())`); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []struct {
		args []string
		want string
	}{
		{[]string{"expect", "random@example.com", "--domains", "example.net"}, "not claimed"},
		{[]string{"expect", "nobody@example.com", "--domains", "example.net"}, "not in the prefix registry"},
		{[]string{"expect", "github@example.com"}, "--domains is required"},
		{[]string{"expect", "github@example.com", "--domains", "com"}, "public suffix"},
		{[]string{"expect", "--domains", "example.net"}, "usage"},
	} {
		if _, _, err := e.run("prefix", bad.args...); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("prefix %v: %v, want %q", bad.args, err, bad.want)
		}
	}
}

func TestPrefixCommands(t *testing.T) {
	t.Parallel()
	e := newHostEnv(t)
	// Case folds the way ingest keys an envelope recipient.
	if out := e.must("prefix", "claim", "GitHub@Example.COM"); !strings.Contains(out, "claimed github@example.com") {
		t.Errorf("claim: %q", out)
	}
	actor, cred, args := e.lastAction("prefix.claim")
	if actor != store.SystemPrincipalID || cred != nil || args["via"] != "cli" || args["host_user"] != "tester" ||
		args["prefix"] != "github" || args["zone"] != "example.com" {
		t.Errorf("recorded %s %v %v", actor, cred, args)
	}
	if out := e.must("prefix", "claim", "github@example.com"); !strings.Contains(out, "already claimed") {
		t.Errorf("claim again: %q", out)
	}
	e.must("prefix", "claim", "shop@example.org")
	if _, err := e.pool.Exec(t.Context(), `INSERT INTO prefixes (zone, prefix, first_seen, last_seen)
		VALUES ('example.com', E'x\tz\x1b[31m', now(), now())`); err != nil {
		t.Fatal(err)
	}

	out := e.must("prefix", "ls")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "ZONE") || !strings.Contains(out, `x\tz\x1b[31m`) || strings.Contains(out, "\x1b") {
		t.Errorf("ls:\n%s", out)
	}
	if out := e.must("prefix", "ls", "--state", "claimed", "--zone", "EXAMPLE.com"); !strings.Contains(out, "github") ||
		strings.Contains(out, "shop") || strings.Contains(out, "discovered") || strings.Count(out, "\n") != 2 {
		t.Errorf("ls claimed in example.com:\n%s", out)
	}

	if out := e.must("prefix", "unclaim", "shop@example.org"); !strings.Contains(out, "left the registry") {
		t.Errorf("unclaim: %q", out)
	}
	if _, _, err := e.run("prefix", "unclaim", "shop@example.org"); err == nil || !strings.Contains(err.Error(), "not in the prefix registry") {
		t.Errorf("unclaim twice: %v", err)
	}
	for _, bad := range [][]string{
		{"claim", "nozone"},
		{"claim", "@example.com"},
		{"claim", "a@"},
		{"claim", "a b@example.com"},
		{"claim"},
		{"claim", "a@example.com", "b@example.com"},
		{"ls", "--state", "maybe"},
		{"label"},
	} {
		if _, _, err := e.run("prefix", bad...); err == nil {
			t.Errorf("prefix %v: no error", bad)
		}
	}
}
