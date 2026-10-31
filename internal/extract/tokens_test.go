package extract

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/yuuka-mail/yuuka/internal/mimeparse"
)

func TestTokens(t *testing.T) {
	cases := []struct {
		text      string
		want, not []string
	}{
		{"Your code is 740219.", []string{"740219"}, nil},
		{"code: ab12cd", []string{"ab12cd", "AB12CD", "code"}, nil},
		{"7-4-0-2-1-9", []string{"740219", "7402", "40219"}, []string{"7"}},
		{"740 219 and 4829.1374", []string{"740219", "48291374"}, nil},
		{"7 4 0 2", []string{"7402"}, nil},
		{"hidden in x740219x", []string{"740219", "x740219x", "X740219X"}, nil},
		{"７４０２１９", []string{"740219"}, nil},
		// A stretch longer than 8 digits is no code; a shorter one inside
		// it still is.
		{"1234 5678 9", []string{"12345678", "56789"}, []string{"123456789"}},
		{"too long 12345678901", nil, []string{"12345678901", "1234567890"}},
		{"see https://example.net/verify?t=abc.", []string{"https://example.net/verify?t=abc.", "https://example.net/verify?t=abc"}, nil},
		{"[here](https://example.net/x)", []string{"https://example.net/x"}, nil},
		{"abc 12 3", nil, []string{"abc", "123"}},
	}
	for _, c := range cases {
		got := Tokens(c.text)
		for _, w := range c.want {
			if !slices.Contains(got, w) {
				t.Errorf("Tokens(%q) = %q, missing %q", c.text, got, w)
			}
		}
		for _, n := range c.not {
			if slices.Contains(got, n) {
				t.Errorf("Tokens(%q) = %q, has %q", c.text, got, n)
			}
		}
		if len(got) != len(slices.Compact(slices.Sorted(slices.Values(got)))) {
			t.Errorf("Tokens(%q) = %q has duplicates", c.text, got)
		}
	}
}

// Every otp and link value the extractor stores from the corpus is among
// the tokens of the same message's subject, body and hrefs: Tokens is the
// inverse of Value. When this fails, the matcher changed how it writes a
// value; change Tokens with it.
func TestTokensCoverExtractions(t *testing.T) {
	var n int
	for _, dir := range []string{"extract", "eml"} {
		files, err := filepath.Glob(filepath.Join("..", "..", "testdata", dir, "*.eml"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no corpus in %s: %v", dir, err)
		}
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			m := mimeparse.Parse(raw)
			toks := append(Tokens(m.Subject), Tokens(m.BodyText)...)
			for _, l := range m.Links {
				toks = append(toks, l.Href)
			}
			for _, x := range Message(m, receivedAt) {
				if x.Kind != OTP && x.Kind != Link {
					continue
				}
				n++
				if !slices.Contains(toks, x.Value) {
					t.Errorf("%s: %s %q is not a token", filepath.Base(f), x.Kind, x.Value)
				}
			}
		}
	}
	if n < 10 {
		t.Fatalf("only %d values in the corpus; the test proves little", n)
	}
}
