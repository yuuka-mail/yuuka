package secret

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseKey(t *testing.T) {
	want := mustKey(t, kekA).ID()
	for _, ok := range []string{kekA, kekA + "\n", kekA + "\r\n"} {
		k, err := ParseKey([]byte(ok))
		if err != nil {
			t.Errorf("%q: %v", ok, err)
			continue
		}
		if k.ID() != want {
			t.Errorf("%q: different key", ok)
		}
	}

	bad := map[string]string{
		"empty":                   "",
		"newline only":            "\n",
		"leading space":           " " + kekA,
		"trailing space":          kekA + " ",
		"two newlines":            kekA + "\n\n",
		"second line":             kekA + "\n" + kekB + "\n",
		"missing padding":         strings.TrimSuffix(kekA, "="),
		"newline inside":          kekA[:20] + "\n" + kekA[21:],
		"url alphabet":            base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb, 0xff}, 16)),
		"33 bytes":                base64.StdEncoding.EncodeToString(make([]byte, 33)),
		"31 bytes":                base64.StdEncoding.EncodeToString(append(make([]byte, 30), 1)),
		"non-canonical pad bits":  kekA[:42] + "9=", // same key, nonzero padding bits
		"all zero":                base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"hex instead of base64":   strings.Repeat("ab", 32),
		"raw 32 bytes":            string(make([]byte, 31)) + "\x01",
		"base64 with a bad char":  kekA[:10] + "*" + kekA[11:],
		"standard but unpadded+1": kekA[:43] + "A",
	}
	for name, text := range bad {
		_, err := ParseKey([]byte(text))
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if len(text) > 4 && strings.Contains(err.Error(), strings.TrimSpace(text)[:8]) {
			t.Errorf("%s: error quotes the key: %v", name, err)
		}
	}
}

func TestLoadKey(t *testing.T) {
	dir := t.TempDir()
	write := func(name, s string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(s), 0o400); err != nil {
			t.Fatal(err)
		}
		return p
	}
	k, err := LoadKey(write("kek", kekA+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if k.ID() != mustKey(t, kekA).ID() {
		t.Fatal("file and string parse differently")
	}

	nearMiss := kekB[:43] + "!"
	_, err = LoadKey(write("bad", nearMiss+"\n"))
	if err == nil || !strings.Contains(err.Error(), filepath.Join(dir, "bad")) {
		t.Fatalf("bad file: %v, want an error naming the file", err)
	}
	if strings.Contains(err.Error(), nearMiss[:20]) {
		t.Fatalf("error echoes the file: %v", err)
	}
	if _, err := LoadKey(write("big", strings.Repeat(kekA+"\n", 10))); err == nil {
		t.Fatal("accepted a long file")
	}
	if _, err := LoadKey(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("accepted a missing file")
	}
}

func TestKeyring(t *testing.T) {
	dir := t.TempDir()
	paths := map[string]string{}
	for name, s := range map[string]string{"a": kekA, "b": kekB, "c": kekC} {
		paths[name] = filepath.Join(dir, name)
		if err := os.WriteFile(paths[name], []byte(s+"\n"), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	r, err := LoadKeyring(paths["a"], []string{paths["b"], paths["c"]})
	if err != nil {
		t.Fatal(err)
	}
	a, b, c := mustKey(t, kekA), mustKey(t, kekB), mustKey(t, kekC)
	if r.Current().ID() != a.ID() {
		t.Error("current is not the first file")
	}
	for _, k := range []*Key{a, b, c} {
		if got, ok := r.Lookup(k.ID()); !ok || got.ID() != k.ID() {
			t.Errorf("%s not found", k)
		}
	}
	if _, ok := r.Lookup(KeyID{1}); ok {
		t.Error("found a key that was never loaded")
	}
	// Previous keys unwrap but never wrap.
	if rec := sealTest(t, mustSealer(t, r)); rec.KeyRef != a.ID().String() {
		t.Error("wrapped with a previous key")
	}

	if _, err := LoadKeyring(paths["a"], []string{paths["b"], paths["a"]}); err == nil {
		t.Error("accepted the current key again as a previous one")
	}
	if _, err := NewKeyring(a, b, b); err == nil {
		t.Error("accepted a previous key twice")
	}
	if _, err := NewKeyring(nil); err == nil {
		t.Error("accepted a keyring with no current key")
	}
	if _, err := LoadKeyring(paths["a"], []string{filepath.Join(dir, "missing")}); err == nil {
		t.Error("ignored a missing previous file")
	}
}

func TestKeyRef(t *testing.T) {
	id := mustKey(t, kekA).ID()
	ref := id.String()
	if got, ok := parseKeyRef(ref); !ok || got != id {
		t.Fatalf("round trip: %v %v", got, ok)
	}
	if len(ref) != 16 || strings.ToLower(ref) != ref {
		t.Errorf("kek_id prints as %q", ref)
	}
	for _, bad := range []string{"", ref[:15], ref + "0", strings.ToUpper(ref), "g" + ref[1:], " " + ref[1:]} {
		if _, ok := parseKeyRef(bad); ok {
			t.Errorf("accepted key_ref %q", bad)
		}
	}
}
