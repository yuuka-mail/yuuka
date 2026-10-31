package localraw

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPut(t *testing.T) {
	for _, prefix := range []string{"raw", "sent"} {
		t.Run(prefix, func(t *testing.T) {
			d := Dir{Root: t.TempDir()}
			key := prefix + "/2026/10/01M67SX66B38NKRKAYDXR834N3.eml"
			if err := d.Put(key, []byte("one")); err != nil {
				t.Fatal(err)
			}
			if err := d.Put(key, []byte("one")); err != nil {
				t.Fatalf("identical re-put: %v", err)
			}
			if err := d.Put(key, []byte("two")); err == nil {
				t.Fatal("overwrite with different content succeeded")
			}
			got, err := os.ReadFile(filepath.Join(d.Root, prefix, "2026", "10", "01M67SX66B38NKRKAYDXR834N3.eml"))
			if err != nil || string(got) != "one" {
				t.Fatalf("content %q, %v", got, err)
			}
			if got, err := d.Get(key); err != nil || string(got) != "one" {
				t.Fatalf("Get = %q, %v", got, err)
			}
			entries, _ := os.ReadDir(filepath.Join(d.Root, prefix, "2026", "10"))
			if len(entries) != 1 {
				t.Fatalf("temp files left behind: %v", entries)
			}
		})
	}
}

func TestPathRejectsEscapes(t *testing.T) {
	d := Dir{Root: t.TempDir()}
	for _, k := range []string{
		"../x", "raw/../../x", "/raw/x", "pending/x", "raw//x", "raw/./x",
		"sent/../raw/x", "sent/../../x", "/sent/x", "sent//x", "sent/./x", "sentx/y", "rawx/y", "x/sent/y",
	} {
		if _, err := d.Path(k); err == nil {
			t.Errorf("Path(%q) accepted", k)
		}
	}
}
