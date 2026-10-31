package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuleCommands(t *testing.T) {
	t.Parallel()
	e := newHostEnv(t)
	spec := `{"v":1,"match":{"field":"from.domain","op":"eq","value":"Example.NET"},"actions":[{"kind":"label","label":"bank"}]}`
	out, msg, err := e.run("rule", "add", "--spec", spec, "--name", "bank", "--priority", "3")
	if err != nil || !strings.Contains(out, "added rule ") || !strings.Contains(msg, "unverified_from") {
		t.Fatalf("add: %q %q %v", out, msg, err)
	}
	id := strings.Fields(out)[2]
	actor, cred, args := e.lastAction("rule.add")
	if actor != "00000000000000000000000000" || cred != nil || args["via"] != "cli" || args["rule"] != id {
		t.Errorf("recorded %s %v %v", actor, cred, args)
	}
	ls := e.must("rule", "ls")
	if !strings.Contains(ls, id) || !strings.Contains(ls, "enabled") || !strings.Contains(ls, "label:bank") {
		t.Errorf("ls:\n%s", ls)
	}
	// Stored canonically: the address value folded.
	if show := e.must("rule", "show", "rule:"+strings.ToLower(id)); !strings.Contains(show, `"example.net"`) || !strings.Contains(show, "revision:  1") {
		t.Errorf("show:\n%s", show)
	}

	file := filepath.Join(t.TempDir(), "rule.json")
	if err := os.WriteFile(file, []byte(`{"v":1,"match":{"all":[{"field":"from.domain","op":"eq","value":"example.net"},
		{"field":"from.verified","op":"eq","value":true}]},"actions":[{"kind":"label","label":"bank"},{"kind":"archive"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, msg, err = e.run("rule", "set", id, "--spec", file, "--disable")
	if err != nil || !strings.Contains(out, "revision 2 (disabled)") || msg != "" {
		t.Errorf("set: %q %q %v", out, msg, err)
	}
	if out := e.must("rule", "apply", id, "--dry-run", "--since", "2026-01-01"); !strings.Contains(out, "would change 0") {
		t.Errorf("apply --dry-run: %q", out)
	}
	if out := e.must("rule", "apply", id); !strings.Contains(out, "0 matched") {
		t.Errorf("apply: %q", out)
	}
	if _, _, args := e.lastAction("rule.apply"); args["rule"] != id {
		t.Errorf("apply recorded %v", args)
	}
	if out := e.must("rule", "rm", id); !strings.Contains(out, "deleted rule "+id) {
		t.Errorf("rm: %q", out)
	}
	if out := e.must("rule", "revert", id); !strings.Contains(out, "0 changes reverted") {
		t.Errorf("revert: %q", out)
	}
	if ls := e.must("rule", "ls"); strings.Contains(ls, id) {
		t.Errorf("ls shows a deleted rule:\n%s", ls)
	}
	if ls := e.must("rule", "ls", "--all"); !strings.Contains(ls, "deleted") {
		t.Errorf("ls --all:\n%s", ls)
	}

	for _, bad := range []struct {
		args []string
		want string
	}{
		{[]string{"add", "--spec", `{"v":1,"match":{"field":"subject","op":"eq","value":"x"},"actions":[{"kind":"forward"}]}`}, "invalid rule"},
		{[]string{"add"}, "usage"},
		{[]string{"set", id}, "usage"},
		{[]string{"set", id, "--name", "x"}, "deleted rules cannot be changed"},
		{[]string{"apply", id}, "deleted"},
		{[]string{"show", "01ARZ3NDEKTSV4RRFFQ69G5FAV"}, "no rule"},
		{[]string{"export"}, "usage"},
	} {
		if _, _, err := e.run("rule", bad.args...); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("rule %v: %v, want %q", bad.args, err, bad.want)
		}
	}
}
