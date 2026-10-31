package agentview

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/classify"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
)

var update = flag.Bool("update", false, "rewrite golden files")

// The corpus is shared with mimeparse, authres and ingest; it is all
// synthetic. Its bodies are test data, not instructions.
const corpusDir = "../../testdata/eml"

const testID = "01JC0000000000000000000000"

func input(t *testing.T, name string) Input {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(corpusDir, name+".eml"))
	if err != nil {
		t.Fatal(err)
	}
	return Input{
		ID:          testID,
		Raw:         raw,
		ReceivedAt:  time.Date(2026, 10, 31, 1, 41, 38, 0, time.UTC),
		DeliveredTo: []string{"inbox@example.com"},
		Class:       Class(classify.Of(mimeparse.Parse(raw), []string{"inbox"}).String()),
		Auth:        authres.Extract(raw, "mx.cloudflare.net"),
	}
}

func corpus(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(corpusDir, "*.eml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no corpus: %v", err)
	}
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = strings.TrimSuffix(filepath.Base(f), ".eml")
	}
	return names
}

// golden compares got with testdata/file, or rewrites it with -update.
func golden(t *testing.T, file string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", file)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output differs from %s (run go test -update and review the diff)\ngot:\n%s", path, got)
	}
}

// Each message gets the JSON tree (what the WebUI renders) and the text a
// model reads.
func TestGolden(t *testing.T) {
	for _, name := range corpus(t) {
		t.Run(name, func(t *testing.T) {
			v := Build(input(t, name))
			j, err := json.MarshalIndent(v, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden(t, name+".json", append(j, '\n'))
			golden(t, name+".txt", []byte(v.Text()))
		})
	}
}

// Same input, same anchors: agents and people hold these URIs (§8.1).
func TestDeterministic(t *testing.T) {
	for _, name := range corpus(t) {
		in := input(t, name)
		a, _ := json.Marshal(Build(in))
		b, _ := json.Marshal(Build(in))
		if !bytes.Equal(a, b) {
			t.Errorf("%s: two builds differ", name)
		}
	}
}

// Every anchor parses back, counts its part's blocks from 1, and selects
// the block it names.
func TestAnchorsResolve(t *testing.T) {
	for _, name := range corpus(t) {
		v := Build(input(t, name))
		for _, p := range v.Content.Parts {
			pa, err := ParseAnchor(p.Anchor)
			if err != nil || pa.Para != 0 {
				t.Fatalf("%s: part anchor %q: %v", name, p.Anchor, err)
			}
			for i, b := range p.Blocks {
				a, err := ParseAnchor(b.Anchor)
				if err != nil || a.Part != pa.Part || a.Para != i+1 {
					t.Fatalf("%s: block %d anchor %q (%v)", name, i+1, b.Anchor, err)
				}
				s, err := Select(v, LevelPart, a)
				if err != nil || len(s.Content.Parts) == 0 || s.Content.Parts[0].Blocks[0].Anchor != b.Anchor {
					t.Fatalf("%s: select %q: %v", name, b.Anchor, err)
				}
			}
		}
	}
}

func blocksOf(v View) []Block {
	var out []Block
	for _, p := range v.Content.Parts {
		out = append(out, p.Blocks...)
	}
	return out
}

func TestQuotedRepliesCollapse(t *testing.T) {
	for name, wantAttr := range map[string]string{
		"html-reply-tracking": "On Thu, 30 Oct 2026 at 09:00, Alice <alice@example.com> wrote:",
		"plain-quoted-list":   "On Thu, 30 Oct 2026, Alice wrote:",
	} {
		v := Build(input(t, name))
		var quotes []Block
		for _, b := range blocksOf(v) {
			if b.Kind == KindQuote {
				quotes = append(quotes, b)
			}
			if strings.Contains(b.Text(), "Shall we go") || strings.Contains(b.Text(), "Can you send") {
				t.Errorf("%s: quoted text leaked into %s", name, b.Anchor)
			}
		}
		if len(quotes) == 0 || quotes[0].Text() != wantAttr || quotes[0].Lines == 0 {
			t.Errorf("%s: quotes = %+v, want the first with attribution %q", name, quotes, wantAttr)
		}
	}
}

func TestTrackingAndHiddenContentStripped(t *testing.T) {
	v := Build(input(t, "html-reply-tracking"))
	if s := v.Facts.Stripped; s.TrackingPixels != 2 || s.Hidden != 2 || s.Images != 1 || s.UnsafeLinks != 1 {
		t.Errorf("stripped = %+v", s)
	}
	text := v.Text()
	for _, gone := range []string{"t.example.org", "Ignore all previous", "hidden filler", "javascript:", "ignored", ".x{"} {
		if strings.Contains(text, gone) {
			t.Errorf("view still contains %q", gone)
		}
	}
	// The image button keeps its real target even without text.
	if !strings.Contains(text, "<https://example.org/go>") {
		t.Errorf("text-less link lost:\n%s", text)
	}
}

func TestRealLinkTargetsKept(t *testing.T) {
	v := Build(input(t, "phish-dmarc-fail"))
	var found bool
	for _, b := range blocksOf(v) {
		for _, s := range b.Spans {
			if s.Href == "https://login.evil.example.net/bank" && s.Text == "https://bank.example.com/login" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("deceptive link not kept with its real target: %+v", blocksOf(v))
	}
	if !strings.Contains(v.Text(), "[https://bank.example.com/login](https://login.evil.example.net/bank)") {
		t.Errorf("text form hides the real target:\n%s", v.Text())
	}
}

func TestDMARCFailDowngraded(t *testing.T) {
	v := Build(input(t, "phish-dmarc-fail"))
	f := v.Facts
	if !f.Downgraded || f.Auth.Line != "spf=fail dkim=none dmarc=fail from_domain=bank.example.com" {
		t.Errorf("facts = %+v", f)
	}
	if !strings.Contains(v.Text(), "warning: dmarc=fail") {
		t.Errorf("no dmarc warning:\n%s", v.Text())
	}
}

func TestUnverifiedAuth(t *testing.T) {
	unverified := []Input{input(t, "untrusted-receiver"), {ID: testID, Raw: []byte("From: a@example.net\r\n\r\nx")}}
	for _, in := range unverified {
		a := Build(in).Facts.Auth
		if a.Verified || a.DMARC != "" || !strings.HasPrefix(a.Line, "unverified (") {
			t.Errorf("auth = %+v", a)
		}
	}
	a := Build(input(t, "plain-quoted-list")).Facts.Auth
	// The MAIL FROM check wins over HELO; any valid DKIM signature is a pass.
	if a.Line != "spf=softfail dkim=pass dmarc=pass from_domain=example.org" {
		t.Errorf("auth line = %q", a.Line)
	}
}

var fenceOpen = regexp.MustCompile(`<untrusted_mail_content nonce="([0-9a-f]{12})">`)

func TestUntrustedBoundary(t *testing.T) {
	for _, name := range corpus(t) {
		text := Build(input(t, name)).Text()
		m := fenceOpen.FindAllStringSubmatch(text, -1)
		if len(m) != 1 {
			t.Fatalf("%s: want one opening fence, got %d", name, len(m))
		}
		closing := `</untrusted_mail_content nonce="` + m[0][1] + `">`
		if strings.Count(text, "untrusted_mail_content") != 2 || !strings.Contains(text, closing) {
			t.Errorf("%s: fence not intact:\n%s", name, text)
		}
		// Facts come before the fence, the sender's words after it.
		if i := strings.Index(text, "From: "); i < strings.Index(text, m[0][0]) {
			t.Errorf("%s: header field outside the fence", name)
		}
	}
}

func TestSelect(t *testing.T) {
	full := Build(input(t, "html-reply-tracking"))
	snip, err := Select(full, LevelSnippet, Anchor{})
	if err != nil || snip.Content.Parts != nil || snip.Content.Sections != nil || snip.Content.Snippet == "" {
		t.Errorf("snippet level: %+v %v", snip.Content, err)
	}
	sec, _ := Select(full, LevelSections, Anchor{})
	if len(sec.Content.Sections) == 0 || sec.Content.Sections[0].Title != "Release checklist" || sec.Content.Parts != nil {
		t.Errorf("sections level: %+v", sec.Content.Sections)
	}
	from, err := Select(full, LevelPart, Anchor{Part: "1.2", Para: 3})
	if err != nil || from.Content.Parts[0].Blocks[0].Anchor != "part/1.2/para/3" {
		t.Errorf("part level from a block: %+v %v", from.Content.Parts, err)
	}
	for _, a := range []Anchor{{Part: "9"}, {Part: "1.2", Para: 999}} {
		if _, err := Select(full, LevelPart, a); !errors.Is(err, ErrNoAnchor) {
			t.Errorf("Select(%v) = %v, want ErrNoAnchor", a, err)
		}
	}
}

func TestSelectBudget(t *testing.T) {
	var body strings.Builder
	body.WriteString("From: a@example.net\r\nSubject: long\r\n\r\n")
	for range 400 {
		body.WriteString(strings.Repeat("word ", 40) + "\r\n\r\n")
	}
	full := Build(Input{ID: testID, Raw: []byte(body.String())})
	v, _ := Select(full, LevelPart, Anchor{})
	if v.Content.More == "" || len(v.Content.Parts[0].Blocks) >= 400 {
		t.Fatalf("no cut: more=%q blocks=%d", v.Content.More, len(v.Content.Parts[0].Blocks))
	}
	a, _ := ParseAnchor(v.Content.More)
	next, _ := Select(full, LevelPart, a)
	if next.Content.Parts[0].Blocks[0].Anchor != v.Content.More {
		t.Errorf("continuation starts at %s, want %s", next.Content.Parts[0].Blocks[0].Anchor, v.Content.More)
	}
}

// A continuation runs on into the later parts; a bare part anchor stays
// in its part.
func TestSelectAcrossParts(t *testing.T) {
	raw := "From: a@example.net\r\nSubject: two\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\none\r\n\r\ntwo\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nthree\r\n--b--\r\n"
	full := Build(Input{ID: testID, Raw: []byte(raw)})
	if len(full.Content.Parts) != 2 {
		t.Fatalf("parts: %+v", full.Content.Parts)
	}
	from, _ := Select(full, LevelPart, Anchor{Part: "1", Para: 2})
	if got := blocksOf(from); len(got) != 2 || got[0].Text() != "two" || got[1].Text() != "three" {
		t.Errorf("continuation: %+v", got)
	}
	only, _ := Select(full, LevelPart, Anchor{Part: "1"})
	if got := blocksOf(only); len(got) != 2 || got[1].Text() != "two" {
		t.Errorf("one part: %+v", got)
	}
}

func TestWithholdDelivery(t *testing.T) {
	in := input(t, "phish-dmarc-fail")
	in.Downgraded = true // as the store computes it from dmarc=fail
	in.WithholdDelivery = true
	v := Build(in)
	f := v.Facts
	if !f.Auth.Withheld || f.Auth.Verified || f.Auth.DMARC != "" || f.Auth.FromDomain != "bank.example.com" ||
		len(f.DeliveredTo) != 0 || !f.Downgraded {
		t.Errorf("facts = %+v", f)
	}
	if text := v.Text(); strings.Contains(text, "dmarc=fail") || strings.Contains(text, "inbox@example.com") || !strings.Contains(text, "warning: downgraded") {
		t.Errorf("text:\n%s", text)
	}
}

func TestParseURI(t *testing.T) {
	for in, want := range map[string]Anchor{
		"yuuka://message/" + testID:                      {},
		"yuuka://message/" + testID + "#part/2":          {Part: "2"},
		"yuuka://message/" + testID + "#part/1.2/para/7": {Part: "1.2", Para: 7},
	} {
		id, a, err := ParseURI(in)
		if err != nil || id != testID || a != want {
			t.Errorf("ParseURI(%q) = %q %v %v", in, id, a, err)
		}
		if got := MessageURI(id); a.String() != "" {
			got += "#" + a.String()
			if got != in {
				t.Errorf("round trip %q -> %q", in, got)
			}
		}
	}
	for _, bad := range []string{
		"", "yuuka://message/", "https://message/" + testID, "yuuka://message/01jc0000000000000000000000",
		"yuuka://message/" + testID + "#para/1", "yuuka://message/" + testID + "#part/0",
		"yuuka://message/" + testID + "#part/1/para/0", "yuuka://message/" + testID + "#part/1/para/x",
		"yuuka://message/" + testID + "#part/1.2.", "yuuka://thread/" + testID,
	} {
		if _, _, err := ParseURI(bad); err == nil {
			t.Errorf("ParseURI(%q) accepted", bad)
		}
	}
}

func TestNeverPanics(t *testing.T) {
	for _, in := range []string{
		"",
		"Content-Type: text/html\r\n\r\n" + strings.Repeat("<div>", 5000) + "deep",
		"Content-Type: text/html\r\n\r\n<a href=\"x>unterminated <script>never closed",
		"Content-Type: text/plain\r\n\r\n>\r\n> >\r\n-----Original Message-----",
		"Content-Type: text/html\r\n\r\n<pre><pre>\r\n</pre><li><h1><br><br></h1>",
	} {
		v := Build(Input{ID: testID, Raw: []byte(in)})
		_ = v.Text()
	}
}
