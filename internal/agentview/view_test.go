package agentview

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/classify"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/triage/score"
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

// Extracted values are the sender's: they render inside the fence, at
// every level, and the JSON carries them under content.
func TestExtractedInsideTheFence(t *testing.T) {
	in := input(t, "plain-otp")
	until := time.Date(2026, 10, 31, 1, 51, 30, 0, time.UTC)
	in.Extracted = []Extraction{
		{Kind: "otp", Value: "482913", Confidence: 95, Source: "subject"},
		{Kind: "link", Value: "https://example.net/v?t=1", Confidence: 85, Source: "html", Host: "example.net"},
		{Kind: "expiry", Value: "PT10M", Confidence: 85, Source: "body", ExpiresAt: &until},
	}
	for _, level := range []Level{LevelSnippet, LevelPart} {
		v, err := Select(Build(in), level, Anchor{})
		if err != nil {
			t.Fatal(err)
		}
		if len(v.Content.Extracted) != 3 {
			t.Errorf("%s: extracted %+v", level, v.Content.Extracted)
		}
		text := v.Text()
		open := fenceOpen.FindStringIndex(text)
		for _, want := range []string{
			"otp: 482913 (confidence 95, from the subject)",
			"link: <https://example.net/v?t=1> (host example.net", "expiry: PT10M, until 2026-10-31T01:51:30Z",
		} {
			if i := strings.Index(text, want); open == nil || i < open[1] {
				t.Errorf("%s: %q missing or outside the fence:\n%s", level, want, text)
			}
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

// The accounts the deliveries came through are delivery facts (§9.4):
// shown, and withheld without delivery:read. Mail pulled header only
// says its body is not there, whoever reads it (§4.4).
func TestAccountsAndHeaderOnly(t *testing.T) {
	in := input(t, "plain-otp")
	in.Accounts = []string{"cf", "gmail"}
	v := Build(in)
	if !slices.Equal(v.Facts.Accounts, []string{"cf", "gmail"}) || !strings.Contains(v.Text(), "account: cf, gmail\n") {
		t.Errorf("accounts %v, text:\n%s", v.Facts.Accounts, v.Text())
	}
	in.WithholdDelivery = true
	if v := Build(in); v.Facts.Accounts != nil || strings.Contains(v.Text(), "gmail") {
		t.Errorf("withheld: accounts %v, text:\n%s", v.Facts.Accounts, v.Text())
	}

	head := Input{ID: testID, Raw: []byte("From: a@example.net\r\nSubject: big\r\n\r\n"), Accounts: []string{"gmail"}, Omitted: "too_large"}
	for _, withhold := range []bool{false, true} {
		head.WithholdDelivery = withhold
		v := Build(head)
		if v.Facts.Omitted != "too_large" || !slices.ContainsFunc(v.Facts.Warnings, func(w string) bool { return strings.HasPrefix(w, "header only:") }) {
			t.Errorf("header only (withheld %v): %+v", withhold, v.Facts)
		}
	}
}

// The store's leak verdicts are facts with a warning each, and delivery
// facts like the rest: withheld without delivery:read.
func TestLeakChecks(t *testing.T) {
	in := input(t, "untrusted-receiver")
	in.LeakChecks = []LeakCheck{
		{To: "github@example.com", Verdict: VerdictSpoof, Domain: "example.net"},
		{To: "shop@example.com", Verdict: VerdictLeak},
		{To: "news@example.com", Verdict: VerdictLeak, Domain: "example.org"},
		{To: "me@example.com", Verdict: VerdictOK, Domain: "example.net"},
	}
	v := Build(in)
	var keys []string
	for _, w := range v.Facts.Warnings {
		k, _, _ := strings.Cut(w, ":")
		keys = append(keys, k)
	}
	if len(v.Facts.LeakChecks) != 4 || strings.Join(keys, " ") != "unverified spoof leak leak" {
		t.Errorf("facts %+v", v.Facts)
	}
	text := v.Text()
	for _, line := range []string{
		"leak check: github@example.com spoof (example.net)\n", "leak check: shop@example.com leak\n",
		"leak check: me@example.com ok (example.net)\n", "warning: leak: news@example.com was not given to example.org",
	} {
		if !strings.Contains(text, line) {
			t.Errorf("text lacks %q:\n%s", line, text)
		}
	}
	in.WithholdDelivery = true
	if v := Build(in); v.Facts.LeakChecks != nil || strings.Contains(v.Text(), "leak") {
		t.Errorf("withheld: %+v\n%s", v.Facts, v.Text())
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

// ownLetter is a synthetic message this system sent from me@example.com.
const ownLetter = "From: Me <me@example.com>\r\nTo: %s\r\nSubject: Lunch on Friday\r\n" +
	"Date: Sat, 31 Oct 2026 12:00:00 +0000\r\nMessage-ID: <lunch.1@example.com>\r\n" +
	"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nShall we meet at noon?\r\n"

// sentDetail is message testID as the store returns mail we sent: an
// outbox row's sent copy, plus the deliveries of it the viewer can see.
func sentDetail(deliveries ...store.DeliveryFact) store.MessageDetail {
	m := store.MessageDetail{Deliveries: deliveries}
	m.ID = testID
	m.ReceivedAt = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	m.Class = classify.Unknown
	m.Outgoing = &store.Outgoing{OutboxID: "01JC0000000000000000000001", State: "sent", From: "me@example.com"}
	return m
}

var reader = &auth.Viewer{PrincipalKind: "agent", PrincipalName: "arisu", Scopes: []auth.Scope{auth.MessageRead, auth.DeliveryRead}}

// Our own mail says so, and carries no receiver verdict: there is none
// for a sent-only message, and mail we sent to ourselves keeps the
// verdict of its delivery.
func TestSentGolden(t *testing.T) {
	sentOnly := []byte(fmt.Sprintf(ownLetter, "bob@example.net"))
	self := []byte("Authentication-Results: mx.cloudflare.net; spf=pass smtp.mailfrom=example.com; " +
		"dkim=pass header.d=example.com; dmarc=pass header.from=example.com\r\n" + fmt.Sprintf(ownLetter, "inbox@example.com"))
	selfAuth := authres.Extract(self, "mx.cloudflare.net")
	for name, in := range map[string]Input{
		"sent-only": InputOf(reader, sentDetail(), sentOnly),
		"self-sent": InputOf(reader, sentDetail(store.DeliveryFact{EnvelopeTo: "inbox@example.com", Account: "cf", Auth: &selfAuth}), self),
	} {
		t.Run(name, func(t *testing.T) {
			v := Build(in)
			j, err := json.MarshalIndent(v, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden(t, name+".json", append(j, '\n'))
			golden(t, name+".txt", []byte(v.Text()))
		})
	}

	v := Build(InputOf(reader, sentDetail(), sentOnly))
	f := v.Facts
	if f.Sent == nil || f.Sent.From != "me@example.com" || f.Sent.State != "sent" || !f.Auth.NotReceived ||
		f.Auth.Verified || f.Auth.Reason != "" || f.Auth.DMARC != "" || len(f.Warnings) != 0 || f.Downgraded {
		t.Errorf("sent-only facts = %+v", f)
	}
	if text := v.Text(); strings.Contains(text, "unverified") || strings.Contains(text, "warning:") {
		t.Errorf("sent-only text casts doubt on our own mail:\n%s", text)
	}
	f = Build(InputOf(reader, sentDetail(store.DeliveryFact{EnvelopeTo: "inbox@example.com", Auth: &selfAuth}), self)).Facts
	if f.Sent == nil || f.Auth.NotReceived || f.Auth.Line != "spf=pass dkim=pass dmarc=pass from_domain=example.com" {
		t.Errorf("self-sent facts = %+v", f)
	}
	// Whether a delivery exists is itself withheld without delivery:read.
	blind := &auth.Viewer{Scopes: []auth.Scope{auth.MessageRead}}
	f = Build(InputOf(blind, sentDetail(), sentOnly)).Facts
	if f.Sent == nil || !f.Auth.Withheld || f.Auth.NotReceived || len(f.Warnings) != 0 {
		t.Errorf("sent-only facts without delivery:read = %+v", f)
	}
}

// A score is listed with yuuka's facts as whose estimate it is, outside
// the sender's fence (§15): the JSON says so in triage.notice, the text
// before the probabilities. The input the scorer was shown is not in
// the view at all.
func TestTriageGolden(t *testing.T) {
	in := input(t, "phish-dmarc-fail")
	in.Triage = TriageOf(&store.ScoreRead{Score: score.Score{
		ID: testID, MessageID: testID, Profile: score.Profile{Scorer: "laya-ml-1", Questions: 1},
		Model: "laya-ml 0.3", ScoredAt: time.Date(2026, 10, 31, 1, 42, 0, 0, time.UTC),
		Answers: map[string]score.Answer{
			"phishing":    {P: map[string]float64{"yes": 0.93, "no": 0.07}},
			"kind":        {P: map[string]float64{"notification": 0.3, "conversation": 0.05, "marketing": 0.05, "spam": 0.6}},
			"needs_reply": {Abstain: true},
		},
		Input: score.Input{SHA256: strings.Repeat("ab", 32)},
	}})
	v := Build(in)
	j, err := json.MarshalIndent(v.Facts.Triage, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "triage.json", append(j, '\n'))
	text := v.Text()
	golden(t, "triage.txt", []byte(text))
	fence := strings.Index(text, "<"+fenceTag)
	if at := strings.Index(text, "triage: "); at < 0 || at > fence {
		t.Errorf("the score is not among yuuka's facts:\n%s", text)
	}
	if strings.Contains(string(j), "abab") || strings.Contains(text, "abab") {
		t.Error("the scorer's input is in the view")
	}
	if Build(input(t, "phish-dmarc-fail")).Facts.Triage != nil || TriageOf(nil) != nil {
		t.Error("a triage fact without a score")
	}
}
