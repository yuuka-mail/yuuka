package main

import (
	"strings"
	"testing"
)

// check runs `secret check` and returns its table and error.
func (e *hostEnv) check() (string, error) {
	e.t.Helper()
	out, _, err := e.runIn(nil, "secret", "check")
	return out, err
}

// resultOf is the RESULT column of the check row of transport name.
func resultOf(table, name string) string {
	for l := range strings.SplitSeq(table, "\n") {
		if strings.Contains(l, "transport "+name+" ") {
			_, res, _ := strings.Cut(l, "local:")
			_, res, _ = strings.Cut(res, " ")
			return strings.TrimSpace(res)
		}
	}
	return ""
}

func TestSecretCheck(t *testing.T) {
	t.Parallel()
	e := newHostEnv(t)
	ctx := t.Context()
	e.env["YUUKA_KEK_FILE"] = writeKEK(t, "kek", 3)
	pws := map[string]string{"a": "password of a", "b": "password of b", "c": "password of c"}
	for _, n := range []string{"a", "b", "c"} {
		e.mustIn(nil, "transport", "add", n, "--host", n+".example.com", "--username", "u",
			"--password-file", writeSecret(t, pws[n]), "--caps", "none")
	}

	out := e.mustIn(nil, "secret", "ls")
	for _, want := range []string{"transport a", "transport b", "smtps://u@c.example.com:465", "smtp_password", "local"} {
		if !strings.Contains(out, want) {
			t.Errorf("ls lacks %q:\n%s", want, out)
		}
	}
	if out, err := e.check(); err != nil || strings.Count(out, " ok\n") != 3 {
		t.Fatalf("check: %v\n%s", err, out)
	}

	// Someone with the database but not the KEK: points b at their own
	// server, and flips a byte of c's sealed payload.
	if _, err := e.pool.Exec(ctx, `UPDATE transports SET host = 'evil.example.net' WHERE name = 'b'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE secrets SET payload = set_byte(payload, 20, get_byte(payload, 20) # 1)
		WHERE id = (SELECT secret_id FROM transports WHERE name = 'c')`); err != nil {
		t.Fatal(err)
	}
	out, err := e.check()
	if err == nil || !strings.Contains(err.Error(), "2 of 3") {
		t.Errorf("check after tampering: %v", err)
	}
	for name, want := range map[string]string{"a": "ok", "b": "audience mismatch", "c": "tampered"} {
		if got := resultOf(out, name); !strings.HasPrefix(got, want) {
			t.Errorf("%s: %q, want %s\n%s", name, got, want, out)
		}
	}
	// Without the KEK every secret is locked.
	delete(e.env, "YUUKA_KEK_FILE")
	out, err = e.check()
	if err == nil || resultOf(out, "a") == "" || !strings.HasPrefix(resultOf(out, "a"), "locked") {
		t.Errorf("without a KEK: %v\n%s", err, out)
	}
	for _, pw := range pws {
		if strings.Contains(out, pw) {
			t.Errorf("check printed %q", pw)
		}
	}
	if _, _, err := e.runIn(nil, "secret", "rewrap"); err == nil || !strings.Contains(err.Error(), "YUUKA_KEK_FILE") {
		t.Errorf("rewrap without a KEK: %v", err)
	}
}

// A KEK rotation: the new KEK current, the old one previous; rewrap
// moves every secret and leaves current ones alone; then the old file
// can go.
func TestSecretRewrap(t *testing.T) {
	t.Parallel()
	e := newHostEnv(t)
	ctx := t.Context()
	oldKEK, newKEK := writeKEK(t, "old", 11), writeKEK(t, "new", 111)
	e.env["YUUKA_KEK_FILE"] = oldKEK
	for _, n := range []string{"a", "b"} {
		e.mustIn(nil, "transport", "add", n, "--host", n+".example.com", "--username", "u",
			"--password-file", writeSecret(t, "pw "+n), "--caps", "none")
	}
	before, err := e.db.Secrets(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// The new KEK alone cannot open what the old one wrapped.
	e.env["YUUKA_KEK_FILE"] = newKEK
	if out, err := e.check(); err == nil || !strings.HasPrefix(resultOf(out, "a"), "locked") {
		t.Errorf("new KEK alone: %v\n%s", err, out)
	}
	e.env["YUUKA_KEK_PREVIOUS_FILES"] = oldKEK
	if out := e.mustIn(nil, "secret", "rewrap"); !strings.Contains(out, "rewrapped 2, already current 0, failed 0") {
		t.Errorf("rewrap: %q", out)
	}
	if out := e.mustIn(nil, "secret", "rewrap"); !strings.Contains(out, "rewrapped 0, already current 2, failed 0") {
		t.Errorf("rewrap again: %q", out)
	}
	after, err := e.db.Secrets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for k := range after {
		b, a := before[k], after[k]
		if a.ID != b.ID || a.KeyRef == b.KeyRef || string(a.Payload) != string(b.Payload) || !a.SealedAt.Equal(b.SealedAt) ||
			a.Revision != b.Revision+1 {
			t.Errorf("secret %s: before %s rev %d, after %s rev %d", a.ID, b.KeyRef, b.Revision, a.KeyRef, a.Revision)
		}
	}
	_, _, args := e.lastAction("secret.rewrap")
	if args["from_key_ref"] != before[1].KeyRef || args["key_ref"] != after[1].KeyRef || args["via"] != "cli" ||
		args["owner"] != "transport b" {
		t.Errorf("secret.rewrap %v", args)
	}

	// The old file can go.
	delete(e.env, "YUUKA_KEK_PREVIOUS_FILES")
	if out, err := e.check(); err != nil {
		t.Errorf("after rewrap, new KEK alone: %v\n%s", err, out)
	}
	if e.opened("a") != "pw a" {
		t.Error("a opens to something else")
	}
	e.noPlaintext("pw a", "pw b")
}
