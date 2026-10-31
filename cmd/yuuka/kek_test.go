package main

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yuuka-mail/yuuka/internal/secret"
)

func TestLoadSealer(t *testing.T) {
	dir := t.TempDir()
	kek := func(name string, fill byte) string {
		k := make([]byte, 32)
		for i := range k {
			k[i] = fill + byte(i)
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(base64.StdEncoding.EncodeToString(k)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	oldKEK, newKEK := kek("old", 1), kek("new", 101)
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	aud, err := secret.SMTPS("u", "smtp.example.com", 465)
	if err != nil {
		t.Fatal(err)
	}

	s, err := loadSealer(env(nil))
	if err != nil || s != nil {
		t.Fatalf("unset: %v, %v; want the locked nil sealer", s, err)
	}
	if _, err := s.Seal(ctx, "01J0000000000000000000000A", "smtp_password", aud, []byte("pw")); !errors.Is(err, secret.ErrLocked) {
		t.Fatalf("locked seal: %v", err)
	}

	for name, e := range map[string]map[string]string{
		"malformed":         {"YUUKA_KEK_FILE": bad},
		"missing":           {"YUUKA_KEK_FILE": filepath.Join(dir, "nope")},
		"bad previous":      {"YUUKA_KEK_FILE": newKEK, "YUUKA_KEK_PREVIOUS_FILES": bad},
		"previous, no kek":  {"YUUKA_KEK_PREVIOUS_FILES": oldKEK},
		"same key previous": {"YUUKA_KEK_FILE": newKEK, "YUUKA_KEK_PREVIOUS_FILES": newKEK},
	} {
		if _, err := loadSealer(env(e)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}

	// A record sealed under the old KEK opens after a rotation that keeps
	// it as a previous file.
	old, err := loadSealer(env(map[string]string{"YUUKA_KEK_FILE": oldKEK}))
	if err != nil {
		t.Fatal(err)
	}
	r, err := old.Seal(ctx, "01J0000000000000000000000A", "smtp_password", aud, []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := loadSealer(env(map[string]string{"YUUKA_KEK_FILE": newKEK, "YUUKA_KEK_PREVIOUS_FILES": " , " + oldKEK + ","}))
	if err != nil {
		t.Fatal(err)
	}
	v, err := rotated.Open(ctx, r, aud)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if string(v.Reveal()) != "pw" {
		t.Fatal("opened to something else")
	}
}
