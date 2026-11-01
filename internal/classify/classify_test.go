package classify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuuka-mail/yuuka/internal/mimeparse"
)

func TestCorpus(t *testing.T) {
	want := map[string]Class{
		"alternative-reply":     Human,   // In-Reply-To
		"plain-otp":             Machine, // noreply sender
		"gb2312-otp":            Machine, // 验证码是 739201
		"html-only-newsletter":  Machine, // List-Unsubscribe
		"dsn-report":            Machine, // multipart/report
		"lf-no-mime":            Machine, // Auto-Submitted
		"long-headers":          Machine, // List-Id, despite being a reply
		"gbk-8bit-header":       Unknown,
		"untrusted-receiver":    Unknown,
		"forged-auth":           Unknown,
		"inline-image":          Unknown,
		"multipart-truncated":   Unknown,
		"malformed-header":      Unknown,
		"multipart-no-boundary": Unknown,
		"bad-base64":            Unknown,
		"unknown-charset":       Unknown,
		"html-reply-tracking":   Human, // In-Reply-To
		"plain-quoted-list":     Human, // In-Reply-To
		"phish-dmarc-fail":      Unknown,

		// The extraction corpus: a code or sign-in link the extractor is
		// sure of makes a notification, whoever the sender claims to be.
		"en-alnum-otp":         Machine, // verification code: H7K2QX
		"en-reset-password":    Machine, // "Reset password" link
		"en-unverified-otp":    Machine, // login code is 771204
		"zh-gbk-otp":           Machine, // 动态码为 573920
		"en-device-code":       Machine, // noreply sender
		"en-html-otp":          Machine, // no-reply sender
		"en-magic-link":        Machine, // List-Unsubscribe
		"en-promo-code":        Machine, // List-Unsubscribe
		"ja-otp":               Machine, // no-reply sender
		"zh-subject-otp":       Machine, // noreply sender
		"en-grouped-code":      Unknown, // "code to finish signing in", a line away: not sure enough
		"en-booking":           Unknown, // a booking reference is no code
		"en-invoice-account":   Unknown,
		"en-order-shipped":     Unknown,
		"en-security-alert":    Unknown,
		"zh-order-negative":    Unknown,
		"en-letter-code-below": Machine, // verification code, a line below
		"en-receipt":           Machine, // receipts+…@ sender
		"en-invoice-login":     Unknown, // "Log In" to a page is no sign-in link
	}
	files, _ := filepath.Glob("../../testdata/*/*.eml")
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".eml")
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		w, ok := want[name]
		if !ok {
			t.Errorf("%s: add an expectation", name)
			continue
		}
		if got := Of(mimeparse.Parse(raw), []string{"alice"}); got != w {
			t.Errorf("%s: %v, want %v", name, got, w)
		}
	}
}

func TestSignals(t *testing.T) {
	base := mimeparse.Message{FromAddr: "carol@example.org", Subject: "hello"}
	cases := []struct {
		name     string
		edit     func(*mimeparse.Message)
		prefixes []string
		want     Class
	}{
		{"plain", func(*mimeparse.Message) {}, nil, Unknown},
		{"service prefix", func(*mimeparse.Message) {}, []string{"alice", "svc-shop"}, Machine},
		{"gh prefix", func(*mimeparse.Message) {}, []string{"gh-ci"}, Machine},
		{"auto-submitted no", func(m *mimeparse.Message) { m.AutoSubmitted = "no" }, nil, Unknown},
		{"precedence list", func(m *mimeparse.Message) { m.Precedence = "list" }, nil, Machine},
		{"chinese reply", func(m *mimeparse.Message) { m.Subject = "回复：周末安排" }, nil, Human},
		{"fwd", func(m *mimeparse.Message) { m.Subject = "Fwd: photos" }, nil, Human},
		{"code in a reply", func(m *mimeparse.Message) {
			m.InReplyTo = "x@example.org"
			m.Subject = "Re: the code 1234 you asked about"
		}, nil, Human},
		{"year is not a code", func(m *mimeparse.Message) { m.Subject = "plans for 2027" }, nil, Unknown},
		{"letters and digits code", func(m *mimeparse.Message) { m.BodyText = "Your verification code is: H7K2QX" }, nil, Machine},
		{"sign-in link", func(m *mimeparse.Message) {
			m.Links = []mimeparse.Link{{Href: "https://example.org/login?t=4f9c2e81ab", Text: "Sign in"}}
		}, nil, Machine},
		{"a sign-in page", func(m *mimeparse.Message) {
			m.Links = []mimeparse.Link{{Href: "https://example.org/login", Text: "Sign in"}}
		}, nil, Unknown},
		{"receipts sender", func(m *mimeparse.Message) { m.FromAddr = "receipts+acct_1A2b3C@example.net" }, nil, Machine},
		{"a reply quoting a code", func(m *mimeparse.Message) {
			m.InReplyTo = "x@example.org"
			m.BodyText = "It worked, thanks.\n\n> Your verification code is 482913"
		}, nil, Human},
		{"a reply by References only", func(m *mimeparse.Message) {
			m.References = []string{"x@example.org"}
			m.BodyText = "Here it is: your verification code is 482913"
		}, nil, Human},
		{"a forwarded code", func(m *mimeparse.Message) { m.Subject = "Fwd: Your verification code is 482913" }, nil, Human},
		{"a door code from a person", func(m *mimeparse.Message) { m.BodyText = "The door code is 4821, see you at 7." }, nil, Unknown},
		{"an order number", func(m *mimeparse.Message) { m.Subject = "Order code 12345678 confirmed" }, nil, Unknown},
	}
	for _, c := range cases {
		m := base
		c.edit(&m)
		if got := Of(m, c.prefixes); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
