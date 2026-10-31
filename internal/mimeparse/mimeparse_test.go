package mimeparse

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

// The corpus is shared with other packages (authres, ingest); it is all
// synthetic. Each file gets a golden JSON of the parse result.
const corpusDir = "../../testdata/eml"

func TestGolden(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(corpusDir, "*.eml"))
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
			got, err := json.MarshalIndent(Parse(raw), "", "  ")
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
				t.Errorf("parse result differs from %s (run go test -update and review the diff)\ngot:\n%s", golden, got)
			}
		})
	}
}

// Parsing is a pure function of the bytes: reparse relies on it (§4.2).
func TestDeterministic(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(corpusDir, "*.eml"))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(Parse(raw))
		b, _ := json.Marshal(Parse(raw))
		if !bytes.Equal(a, b) {
			t.Errorf("%s: two parses differ", f)
		}
	}
}

func TestNeverPanics(t *testing.T) {
	inputs := []string{
		"",
		"\r\n",
		" leading continuation\r\n\r\nbody",
		"Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\n",
		"Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\n",
		"From: <>\r\nSubject: =?utf-8?B?####?=\r\n\r\n",
		"Content-Type: text/html\r\n\r\n<a href=\"x>unterminated <script>never closed",
	}
	for _, in := range inputs {
		_ = Parse([]byte(in))
	}
}

// To, Cc and Reply-To say who is on a thread (§8.9). What does not parse
// is skipped, never guessed; nothing here may fail the parse.
func TestAddresses(t *testing.T) {
	cases := []struct {
		name, header string
		want         []string
	}{
		{"plain", "A <A@Example.com>, b@example.com", []string{"A@Example.com", "b@example.com"}},
		{"undisclosed", "undisclosed-recipients:;", []string{}},
		{
			"group", "Team: a@example.com, B <b@example.com>;, c@example.com",
			[]string{"a@example.com", "b@example.com", "c@example.com"},
		},
		{"quoted comma", `"Doe, John" <j@example.com>`, []string{"j@example.com"}},
		{"encoded name", "=?utf-8?q?=E5=BC=A0?= <z@example.com>", []string{"z@example.com"}},
		{
			"one broken", `a@example.com, not an address, "x, y" <b@example.com>`,
			[]string{"a@example.com", "b@example.com"},
		},
		{"all broken", "<@route:a@example.com>, nobody", []string{}},
		{"undeclared 8-bit name", "\xd5\xc5 <g@example.com>, broken", []string{"g@example.com"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := "From: s@example.net\r\nTo: " + c.header + "\r\nCc: " + c.header + "\r\nReply-To: " + c.header +
				"\r\nContent-Type: text/plain; charset=gbk\r\n\r\nbody\r\n"
			m := Parse([]byte(raw))
			for field, got := range map[string][]string{"To": m.To, "Cc": m.Cc, "Reply-To": m.ReplyTo} {
				if !slices.Equal(got, c.want) || got == nil {
					t.Errorf("%s = %#v, want %q", field, got, c.want)
				}
			}
			if m.Problem != "" {
				t.Errorf("problem %q", m.Problem)
			}
		})
	}

	none := Parse([]byte("From: s@example.net\r\n\r\nbody\r\n"))
	if none.To == nil || none.Cc == nil || none.ReplyTo == nil || len(none.To)+len(none.Cc)+len(none.ReplyTo) != 0 {
		t.Errorf("no fields: %#v %#v %#v", none.To, none.Cc, none.ReplyTo)
	}
	var long strings.Builder
	for i := range maxAddrs + 1 {
		fmt.Fprintf(&long, "a%d@example.com, ", i)
	}
	capped := Parse([]byte("To: " + long.String() + "z@example.com\r\n\r\nbody\r\n"))
	if len(capped.To) != maxAddrs || !capped.AddrsCapped || capped.Problem != "" {
		t.Errorf("%d addresses, capped %v, problem %q: want %d, capped, no problem",
			len(capped.To), capped.AddrsCapped, capped.Problem, maxAddrs)
	}
}

// ParseRecipients reads the same addresses as Parse, with their names.
func TestParseRecipients(t *testing.T) {
	raw := "From: s@example.net\r\n" +
		"To: \"Doe, John\" <j@example.com>, =?utf-8?q?=E5=BC=A0?= <z@example.com>, broken, b@example.com\r\n" +
		"Cc: Team: \xd5\xc5 <g@example.com>;\r\n" +
		"Content-Type: text/plain; charset=gbk\r\n\r\nbody\r\n"
	got := ParseRecipients([]byte(raw))
	want := Recipients{
		To:      []Mailbox{{"Doe, John", "j@example.com"}, {"张", "z@example.com"}, {"", "b@example.com"}},
		Cc:      []Mailbox{{"张", "g@example.com"}},
		ReplyTo: []Mailbox{},
	}
	if !slices.Equal(got.To, want.To) || !slices.Equal(got.Cc, want.Cc) || got.ReplyTo == nil || len(got.ReplyTo) != 0 {
		t.Errorf("ParseRecipients = %#v, want %#v", got, want)
	}
	m := Parse([]byte(raw))
	for i, a := range m.To {
		if got.To[i].Addr != a {
			t.Errorf("To[%d] = %q, Parse has %q", i, got.To[i].Addr, a)
		}
	}
	if empty := ParseRecipients([]byte("not a message")); empty.To == nil || empty.Cc == nil || empty.ReplyTo == nil {
		t.Errorf("ParseRecipients of junk: %#v", empty)
	}
}

func TestHTMLToText(t *testing.T) {
	cases := map[string]string{
		"<p>a</p><p>b</p>":                          "a\nb",
		"x &lt; y &amp;&amp; <b>z</b>":              "x < y && z",
		"<style>p{}</style>keep<script>no</script>": "keep",
		"1 < 2 but 3>2":                             "1 < 2 but 3>2",
		"<a title=\"a>b\">link</a>":                 "link",
		"<!-- c -->t":                               "t",
	}
	for in, want := range cases {
		if got := htmlToText(in); got != want {
			t.Errorf("htmlToText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSnippet(t *testing.T) {
	cases := map[string]string{
		"Hi Bob\n\n-- \nOperator\n":            "Hi Bob",
		"> quoted\nNew text\r\n-- \r\nSig\r\n": "New text",
		"-- \nOnly a signature\n":              "-- Only a signature",
		"Dashes -- in text\n--\nnot a delim\n": "Dashes -- in text -- not a delim",
	}
	for in, want := range cases {
		if got := snippet(in); got != want {
			t.Errorf("snippet(%q) = %q, want %q", in, got, want)
		}
	}
}
