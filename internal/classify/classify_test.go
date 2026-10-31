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
		"gb2312-otp":            Machine, // 验证码 + digits
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
	}
	files, _ := filepath.Glob("../../testdata/eml/*.eml")
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
	}
	for _, c := range cases {
		m := base
		c.edit(&m)
		if got := Of(m, c.prefixes); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
