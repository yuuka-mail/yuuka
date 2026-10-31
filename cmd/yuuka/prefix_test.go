package main

import (
	"strings"
	"testing"

	"github.com/yuuka-mail/yuuka/internal/store"
)

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
