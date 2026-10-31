package authres

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

// cloudflare is the authserv-id measured for Cloudflare Email Routing.
const cloudflare = "mx.cloudflare.net"

func TestGolden(t *testing.T) {
	files, err := filepath.Glob("../../testdata/eml/*.eml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no corpus: %v", err)
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".eml")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.MarshalIndent(Extract(raw, cloudflare), "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			golden := filepath.Join("testdata", name+".golden")
			if *update {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run go test -update)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("result differs from %s (run go test -update and review the diff)\ngot:\n%s", golden, got)
			}
		})
	}
}

// The sender pre-wrote a passing result under the receiver's name below
// the receiver's own failing one; only the topmost counts.
func TestForgedResultBelowIsIgnored(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/eml/forged-auth.eml")
	if err != nil {
		t.Fatal(err)
	}
	a := Extract(raw, cloudflare)
	if !a.Trusted || a.DMARC != "fail" || a.CFSpamHScore != "9" || a.RemoteIP != "203.0.113.66" {
		t.Errorf("got %+v", a)
	}
}

func TestUntrusted(t *testing.T) {
	cases := map[string]struct {
		raw, trusted, reason string
	}{
		"no config":     {"Authentication-Results: mx.cloudflare.net; dmarc=pass\r\n\r\n", "", ReasonNoTrustedID},
		"missing":       {"From: a@example.net\r\n\r\nbody", cloudflare, ReasonMissing},
		"other id":      {"Authentication-Results: mx.example.org; dmarc=pass\r\n\r\n", cloudflare, ReasonOtherID},
		"in body":       {"From: a@example.net\r\n\r\nAuthentication-Results: mx.cloudflare.net; dmarc=pass\r\n", cloudflare, ReasonMissing},
		"forged second": {"Authentication-Results: mx.example.org; dmarc=fail\r\nAuthentication-Results: mx.cloudflare.net; dmarc=pass\r\n\r\n", cloudflare, ReasonOtherID},
	}
	for name, c := range cases {
		a := Extract([]byte(c.raw), c.trusted)
		if a.Trusted || a.Reason != c.reason || a.DMARC != "" || a.Results != nil {
			t.Errorf("%s: got %+v", name, a)
		}
	}
}

func TestParse(t *testing.T) {
	id, rs := parse(`mx.Example.org 1; (comment; with semicolon) dkim=pass (good "sig") header.d=example.net header.b="ab;c";` +
		` spf=Neutral reason="x y"; DMARC/1=PASS header.from=example.net; none`)
	if id != "mx.Example.org" {
		t.Errorf("id %q", id)
	}
	if len(rs) != 3 {
		t.Fatalf("results %+v", rs)
	}
	if rs[0].Method != "dkim" || rs[0].Result != "pass" || rs[0].Props["header.b"] != "ab;c" || rs[0].Props["header.d"] != "example.net" {
		t.Errorf("dkim %+v", rs[0])
	}
	if rs[1].Method != "spf" || rs[1].Result != "neutral" {
		t.Errorf("spf %+v", rs[1])
	}
	if rs[2].Method != "dmarc" || rs[2].Result != "pass" {
		t.Errorf("dmarc %+v", rs[2])
	}
}

// Spacing and quoting that RFC 8601 allows and a whitespace split gets
// wrong: CFWS around "=" and "/", quoted values with spaces or escaped
// quotes, and token values that themselves contain "=" and "/".
func TestParseGrammar(t *testing.T) {
	id, rs := parse(`"mx.example.org"; dkim / 1 (v1) = pass header.d = example.net header.b=Ab/c+D=;` +
		` spf=neutral reason="x y" smtp.mailfrom=a@example.net; dmarc=fail reason="say \"no\""`)
	if id != "mx.example.org" {
		t.Errorf("id %q", id)
	}
	want := []Result{
		{Method: "dkim", Result: "pass", Props: map[string]string{"header.d": "example.net", "header.b": "Ab/c+D="}},
		{Method: "spf", Result: "neutral", Props: map[string]string{"reason": "x y", "smtp.mailfrom": "a@example.net"}},
		{Method: "dmarc", Result: "fail", Props: map[string]string{"reason": `say "no"`}},
	}
	if !reflect.DeepEqual(rs, want) {
		t.Errorf("got  %+v\nwant %+v", rs, want)
	}
}

// The value comes from the receiver, but a parser bug here would turn
// into a hang or panic on every delivery, so any input must terminate.
func FuzzParse(f *testing.F) {
	for _, s := range []string{
		`mx.example.org 1; dkim=pass header.b="ab;c"; none`,
		`"mx"; dkim / 1 = pass header.d = example.net header.b=Ab/c+D=`,
		`x; = ; / ; a/=b ; "unterminated`,
		`x; spf=pass (nested (comment) \) ) reason="q\"`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		_, rs := parse(v)
		for _, r := range rs {
			if r.Method == "" || r.Method != strings.ToLower(r.Method) {
				t.Errorf("method %q from %q", r.Method, v)
			}
		}
	})
}

func TestSpamScoreOnlyFromReceiverBlock(t *testing.T) {
	raw := "Received: from a by cloudflare-email.net; x\r\n" +
		"Authentication-Results: mx.cloudflare.net; dmarc=pass\r\n" +
		"Received: from b by mail.example.net; y\r\n" +
		"X-CF-SpamH-Score: 0\r\n\r\n"
	if a := Extract([]byte(raw), cloudflare); !a.Trusted || a.CFSpamHScore != "" {
		t.Errorf("sender's score taken: %+v", a)
	}
}
