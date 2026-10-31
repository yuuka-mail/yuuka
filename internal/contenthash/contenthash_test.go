package contenthash

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/v1.golden")

// TestGoldenV1 freezes the v1 algorithm: any change to the output for the
// corpus is a v2, not an edit (§4.1). Only run -update when adding files.
func TestGoldenV1(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.eml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no corpus: %v", err)
	}
	slices.Sort(files)
	var b strings.Builder
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		sum := V1(raw)
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(sum[:]), filepath.Base(f))
	}
	golden := filepath.Join("testdata", "v1.golden")
	if *update {
		if err := os.WriteFile(golden, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if b.String() != string(want) {
		t.Fatalf("content hash v1 changed:\n%s\nwant\n%s", b.String(), want)
	}
}

// TestStrippedBytes checks the algorithm against hand-written expectations
// of exactly which bytes are hashed, independent of the golden file.
func TestStrippedBytes(t *testing.T) {
	cases := map[string]string{
		"cloudflare-a.eml": "From: Alice <alice@example.net>\r\nTo: github@example.com\r\n" +
			"Subject: Your code is 123456\r\nMessage-ID: <m1@example.net>\r\n" +
			"Date: Fri, 31 Oct 2026 01:41:30 +0000\r\nMIME-Version: 1.0\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
			"Your code is 123456.\r\nReceived: this line is body, not a header\r\n",
		"lf-oddities.eml": "ReceivedX: not a transport field\nFrom: bob@example.org\n" +
			"Subject: LF only,\n folded subject\nno colon here\n\nbody\n",
		"no-body.eml": "From: carol@example.org\nSubject: headers only, no blank line\n",
	}
	for name, kept := range cases {
		raw, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := V1(raw), sha256.Sum256([]byte(kept)); got != want {
			t.Errorf("%s: hash does not match the expected kept bytes", name)
		}
	}
}

func TestRedeliveryHashesEqual(t *testing.T) {
	a, _ := os.ReadFile(filepath.Join("testdata", "cloudflare-a.eml"))
	b, _ := os.ReadFile(filepath.Join("testdata", "cloudflare-a-redelivered.eml"))
	if sha256.Sum256(a) == sha256.Sum256(b) {
		t.Fatal("corpus broken: redelivery must differ byte-wise")
	}
	if V1(a) != V1(b) {
		t.Fatal("redelivery with new transport headers hashed differently")
	}
	changed := strings.Replace(string(a), "Your code is 123456.", "Your code is 654321.", 1)
	if V1([]byte(changed)) == V1(a) {
		t.Fatal("different body hashed the same")
	}
}
