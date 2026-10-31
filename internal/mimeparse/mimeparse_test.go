package mimeparse

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
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
