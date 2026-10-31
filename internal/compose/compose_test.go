package compose

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

var update = flag.Bool("update", false, "rewrite golden files")

var date = time.Date(2026, 10, 31, 9, 30, 0, 0, time.FixedZone("", 8*3600))

// A 1x1 PNG; the bytes only have to survive the round trip.
var png = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89" +
	"\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

func longReferences() []string {
	var refs []string
	for i := range 30 {
		refs = append(refs, fmt.Sprintf("msg%02d@example.com", i))
	}
	// Junk a hostile parent could carry: dropped, never written.
	refs = append(refs, "evil\r\nBcc: victim@example.com", "<nested@example.com>", "msg05@example.com")
	return refs
}

// cases are the golden messages. Everything is synthetic.
var cases = map[string]*Message{
	"basic": {
		From:      Address{Name: "Operator", Addr: "operator@example.com"},
		To:        []Address{{Name: "Alice Example", Addr: "alice@example.com"}},
		Cc:        []Address{{Addr: "bob@example.com"}},
		Bcc:       []Address{{Addr: "hidden@example.com"}},
		ReplyTo:   []Address{{Addr: "replies@example.com"}},
		Subject:   "Quarterly numbers",
		Date:      date,
		MessageID: "01JBXM0000000000000000000A@example.com",
		Body: "Hi Alice,\n\nThe **numbers** are in. See the [report](https://example.com/report) " +
			"or https://example.com/raw for the raw data.\n\n" +
			"> Can you send them by Friday?\n\n" +
			"- revenue up\n- costs `flat`\n\n" +
			"| Quarter | Revenue |\n|:--|--:|\n| Q3 | 120 |\n| Q4 | 135 |\n\n" +
			"```go\nfmt.Println(\"<ok>\")\n```\n\n---\n\nThanks,\nOperator\n",
	},
	"chinese": {
		From:      Address{Name: "运维者", Addr: "operator@example.com"},
		To:        []Address{{Name: "张三（财务部）", Addr: "zhangsan@example.com"}},
		Subject:   "关于第三季度报销单的补充材料，请在周五之前确认并回复",
		Date:      date,
		MessageID: "01JBXM0000000000000000000B@example.com",
		Body:      "张三你好：\n\n附件是**第三季度**的报销单，请查收。\n\n> 原文：请补充发票。\n\n谢谢！\n",
		Attachments: []Attachment{
			{Filename: "第三季度报销单.csv", ContentType: "text/csv", Data: []byte("项目,金额\r\n差旅,1200\r\n")},
		},
	},
	"reply": {
		From:    Address{Name: "Arisu", Addr: "arisu@example.com"},
		To:      []Address{{Addr: "monitoring@example.com"}},
		Subject: "Re: disk usage alert",
		Date:    date,
		Parent: &Parent{
			MessageID:  "parent@example.com",
			References: longReferences(),
		},
		MessageID:     "01JBXM0000000000000000000C@example.com",
		AutoSubmitted: "auto-generated",
		Body:          "Acknowledged; cleanup is scheduled.\n",
	},
	"attachments": {
		From:      Address{Addr: "operator@example.com"},
		To:        []Address{{Addr: "alice@example.com"}},
		Subject:   "Logo and contract",
		Date:      date,
		MessageID: "01JBXM0000000000000000000D@example.com",
		Body:      "The new logo:\n\n![logo](cid:logo@example.com)\n\nThe contract is attached.\n",
		Attachments: []Attachment{
			{Filename: "logo.png", ContentType: "image/png", ContentID: "logo@example.com", Data: png},
			{Filename: "contract draft.pdf", ContentType: "application/pdf", Data: bytes.Repeat([]byte("%PDF-1.7 synthetic\n"), 20)},
		},
	},
	"hostile": {
		From:      Address{Name: "Arisu\r\nBcc: victim@example.com", Addr: "arisu@example.com"},
		To:        []Address{{Addr: "alice@example.com"}},
		Subject:   "Hello\r\nBcc: victim@example.com",
		Date:      date,
		MessageID: "01JBXM0000000000000000000E@example.com",
		Body: "Summary of the mail you asked about.\n\n" +
			"<script>alert('x')</script>\n\n" +
			"Inline <img src=x onerror=\"alert(1)\"> and <b onclick=\"steal()\">bold</b>.\n\n" +
			"[click](javascript:alert(1)) ![pixel](https://tracker.example.com/p.gif)\n\n" +
			"<style>body{display:none}</style>\n",
	},
	"strip-markup": {
		From:        Address{Name: "Operator", Addr: "operator@example.com"},
		To:          []Address{{Addr: "security@example.com"}},
		Subject:     "Vulnerability report",
		Date:        date,
		MessageID:   "01JBXM0000000000000000000F@example.com",
		StripMarkup: true,
		Body: "# Summary\n\nThe **login form** accepts `admin\\*` as a wildcard; see " +
			"[the advisory](https://example.com/advisory) &amp; ~~old~~ notes.\n\n" +
			"1. Open the page\n2. Submit:\n\n       curl -d 'u=admin*' https://example.com/login\n\n" +
			"> quoted line\n> second line\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n" +
			"Regards,  \nOperator\n",
	},
	"no-message-id": {
		From:    Address{Addr: "operator@example.com"},
		To:      []Address{{Addr: "alice@example.com"}},
		Subject: "Sent through a transport that sets its own Message-ID",
		Date:    date,
		Body:    "Short.",
	},
}

func TestGolden(t *testing.T) {
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Compose(m)
			if err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join("testdata", name+".eml")
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
				t.Errorf("output differs from %s (run go test -update and review the diff)\ngot:\n%s", golden, got)
			}
		})
	}
}

func TestDeterministic(t *testing.T) {
	for name, m := range cases {
		a, _ := Compose(m)
		b, _ := Compose(m)
		if !bytes.Equal(a, b) {
			t.Errorf("%s: two compositions differ", name)
		}
	}
}

// Wire-format invariants every message must hold.
func TestWireFormat(t *testing.T) {
	for name, m := range cases {
		raw, err := Compose(m)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(bytes.ReplaceAll(raw, []byte("\r\n"), nil), []byte("\n")) ||
			bytes.Contains(bytes.ReplaceAll(raw, []byte("\r\n"), nil), []byte("\r")) {
			t.Errorf("%s: bare CR or LF", name)
		}
		for i, l := range bytes.Split(raw, []byte("\r\n")) {
			if len(l) > 998 {
				t.Errorf("%s: line %d is %d octets", name, i+1, len(l))
			}
			for _, c := range l {
				if c >= 0x80 {
					t.Errorf("%s: line %d is not 7-bit", name, i+1)
					break
				}
			}
		}
		head, _, _ := bytes.Cut(raw, []byte("\r\n\r\n"))
		lower := strings.ToLower(string(head))
		for _, bad := range []string{"x-mailer", "yuuka", "\nbcc:", "user-agent"} {
			if strings.Contains(lower, bad) {
				t.Errorf("%s: header contains %q", name, bad)
			}
		}
		for _, a := range m.Bcc {
			if bytes.Contains(raw, []byte(a.Addr)) {
				t.Errorf("%s: Bcc address %s appears in the message", name, a.Addr)
			}
		}
		if regexp.MustCompile(`(?mi)^bcc:`).Match(raw) {
			t.Errorf("%s: header injection", name)
		}
	}
}

// Parse every message with the parser that will read it back from the
// Sent copy and check what has to survive.
func TestRoundTrip(t *testing.T) {
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			raw, err := Compose(m)
			if err != nil {
				t.Fatal(err)
			}
			got, tree := mimeparse.ParseTree(raw)
			if got.Problem != "" {
				t.Errorf("problem: %s", got.Problem)
			}
			// mimeparse collapses runs of white space.
			collapse := func(s string) string { return strings.Join(strings.Fields(oneLine(s)), " ") }
			if want := collapse(m.Subject); got.Subject != want {
				t.Errorf("subject = %q, want %q", got.Subject, want)
			}
			if got.FromAddr != m.From.Addr || got.FromName != collapse(m.From.Name) {
				t.Errorf("from = %q <%s>", got.FromName, got.FromAddr)
			}
			if got.MessageID != m.MessageID {
				t.Errorf("message id = %q, want %q", got.MessageID, m.MessageID)
			}
			if got.AutoSubmitted != m.AutoSubmitted {
				t.Errorf("auto-submitted = %q", got.AutoSubmitted)
			}
			if !got.Date.Equal(m.Date) {
				t.Errorf("date = %v", got.Date)
			}
			parent, refs := thread(m.Parent)
			if got.InReplyTo != parent || !slices.Equal(got.References, append([]string{}, refs...)) {
				t.Errorf("in-reply-to = %q, references = %q", got.InReplyTo, got.References)
			}

			var plain, htmlPart string
			var files []mimeparse.Entity
			walk(tree, func(e mimeparse.Entity) {
				switch {
				case e.Attachment || e.Filename != "":
					files = append(files, e)
				case e.MediaType == "text/plain":
					plain = e.Text
				case e.MediaType == "text/html":
					htmlPart = e.Text
				}
			})
			wantPlain := normalize(m.Body)
			if m.StripMarkup {
				wantPlain = plainText(parse(wantPlain))
			}
			if strings.ReplaceAll(plain, "\r\n", "\n") != wantPlain {
				t.Errorf("text/plain = %q, want %q", plain, wantPlain)
			}
			if htmlPart == "" {
				t.Error("no text/html part")
			}
			if len(files) != len(m.Attachments) {
				t.Fatalf("%d files, want %d", len(files), len(m.Attachments))
			}
			for i, a := range m.Attachments {
				if files[i].Filename != a.Filename {
					t.Errorf("filename = %q, want %q", files[i].Filename, a.Filename)
				}
				_, data, err := mimeparse.Attachment(raw, files[i].Section)
				if a.ContentID == "" && (err != nil || !bytes.Equal(data, a.Data)) {
					t.Errorf("attachment %s: %v, bytes equal: %v", a.Filename, err, bytes.Equal(data, a.Data))
				}
			}
		})
	}
}

func walk(e mimeparse.Entity, f func(mimeparse.Entity)) {
	if len(e.Children) == 0 {
		f(e)
	}
	for _, c := range e.Children {
		walk(c, f)
	}
}

// The HTML part must not carry anything active, whatever the Markdown
// says (§8.3): no script or style elements, no event handlers, no
// javascript: URLs.
func TestHTMLIsInert(t *testing.T) {
	for name, m := range cases {
		doc, src := parse(normalize(m.Body))
		out, err := renderHTML(doc, src)
		if err != nil {
			t.Fatal(err)
		}
		z := html.NewTokenizer(strings.NewReader(out))
		for {
			tt := z.Next()
			if tt == html.ErrorToken {
				break
			}
			tok := z.Token()
			if tok.Type != html.StartTagToken && tok.Type != html.SelfClosingTagToken {
				continue
			}
			if tok.Data == "script" || tok.Data == "style" || tok.Data == "iframe" || tok.Data == "link" {
				t.Errorf("%s: <%s> in HTML", name, tok.Data)
			}
			for _, a := range tok.Attr {
				if strings.HasPrefix(a.Key, "on") {
					t.Errorf("%s: <%s %s> in HTML", name, tok.Data, a.Key)
				}
				if (a.Key == "href" || a.Key == "src") && strings.HasPrefix(strings.ToLower(a.Val), "javascript:") {
					t.Errorf("%s: <%s %s=%q> in HTML", name, tok.Data, a.Key, a.Val)
				}
				if a.Key == "class" {
					t.Errorf("%s: class attribute on <%s>; mail clients strip class styles", name, tok.Data)
				}
			}
		}
	}
	// The hostile text is shown, not dropped.
	doc, src := parse(cases["hostile"].Body)
	out, _ := renderHTML(doc, src)
	if !strings.Contains(out, "&lt;script&gt;") || !strings.Contains(out, "&lt;img src=x onerror=") {
		t.Errorf("raw HTML not shown as text:\n%s", out)
	}
}

func TestThread(t *testing.T) {
	if p, refs := thread(nil); p != "" || refs != nil {
		t.Error("nil parent threads")
	}
	if p, _ := thread(&Parent{MessageID: "bad id@example.com"}); p != "" {
		t.Error("malformed parent id is written")
	}
	_, refs := thread(&Parent{MessageID: "p@example.com", InReplyTo: "gp@example.com"})
	if !slices.Equal(refs, []string{"gp@example.com", "p@example.com"}) {
		t.Errorf("in-reply-to fallback: %q", refs)
	}
	_, refs = thread(&Parent{MessageID: "p@example.com", References: []string{"a@x", "p@example.com", "b@x", "a@x"}})
	if !slices.Equal(refs, []string{"a@x", "b@x", "p@example.com"}) {
		t.Errorf("duplicates: %q", refs)
	}
	_, refs = thread(&Parent{MessageID: "parent@example.com", References: longReferences()})
	if len(refs) != maxReferences || refs[0] != "msg00@example.com" ||
		refs[len(refs)-1] != "parent@example.com" || refs[len(refs)-2] != "msg29@example.com" {
		t.Errorf("long chain: %q", refs)
	}
}

func TestNewMessageID(t *testing.T) {
	id := NewMessageID(date, "example.com")
	local, domain, ok := strings.Cut(id, "@")
	if !ok || domain != "example.com" || !ulid.Valid(local) || !validID(id) {
		t.Errorf("NewMessageID = %q", id)
	}
}

func TestRecipients(t *testing.T) {
	got := cases["basic"].Recipients()
	want := []string{"alice@example.com", "bob@example.com", "hidden@example.com"}
	if !slices.Equal(got, want) {
		t.Errorf("Recipients = %q", got)
	}
	m := &Message{To: []Address{{Addr: "A@example.com"}}, Bcc: []Address{{Addr: "a@example.com"}}}
	if got := m.Recipients(); len(got) != 1 {
		t.Errorf("duplicate recipient: %q", got)
	}
}

func TestInvalid(t *testing.T) {
	base := func() *Message {
		return &Message{
			From: Address{Addr: "operator@example.com"},
			To:   []Address{{Addr: "alice@example.com"}},
			Date: date,
		}
	}
	for name, mutate := range map[string]func(*Message){
		"no recipients":    func(m *Message) { m.To = nil },
		"no date":          func(m *Message) { m.Date = time.Time{} },
		"bad from":         func(m *Message) { m.From.Addr = "operator" },
		"injected to":      func(m *Message) { m.To[0].Addr = "alice@example.com>\r\nBcc: <x@example.com" },
		"non-ascii addr":   func(m *Message) { m.To[0].Addr = "张三@example.com" },
		"bad message id":   func(m *Message) { m.MessageID = "<x@example.com>" },
		"bad auto-sub":     func(m *Message) { m.AutoSubmitted = "auto-generated\r\nX: y" },
		"bad utf-8":        func(m *Message) { m.Subject = "\xff" },
		"unfoldable":       func(m *Message) { m.Subject = strings.Repeat("x", 1000) },
		"no filename":      func(m *Message) { m.Attachments = []Attachment{{Data: []byte("x")}} },
		"long filename":    func(m *Message) { m.Attachments = []Attachment{{Filename: strings.Repeat("长", 100)}} },
		"bad content type": func(m *Message) { m.Attachments = []Attachment{{Filename: "a", ContentType: "not a type"}} },
		"nested message":   func(m *Message) { m.Attachments = []Attachment{{Filename: "a.eml", ContentType: "message/rfc822"}} },
		"bad content id":   func(m *Message) { m.Attachments = []Attachment{{Filename: "a.png", ContentID: "a b"}} },
	} {
		m := base()
		if _, err := Compose(m); err != nil {
			t.Fatalf("base message: %v", err)
		}
		mutate(m)
		if _, err := Compose(m); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
