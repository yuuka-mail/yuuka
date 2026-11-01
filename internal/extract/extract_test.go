package extract

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/mimeparse"
)

var update = flag.Bool("update", false, "rewrite golden files")

// receivedAt is when every corpus message reached yuuka in these tests.
var receivedAt = time.Date(2026, 10, 31, 1, 41, 38, 0, time.UTC)

type golden struct {
	Kind       Kind       `json:"kind"`
	Value      string     `json:"value"`
	Confidence int        `json:"confidence"`
	Source     Source     `json:"source"`
	Expires    *time.Time `json:"expires,omitempty"`
}

// The corpus is synthetic: testdata/extract holds mail written for the
// matcher, Chinese and Japanese in their legacy charsets included, and
// the shared testdata/eml must yield what it yields too. Each file gets
// a golden JSON; run go test -update and review the diff.
func TestGolden(t *testing.T) {
	for _, dir := range []string{"extract", "eml"} {
		files, err := filepath.Glob(filepath.Join("..", "..", "testdata", dir, "*.eml"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no corpus in %s: %v", dir, err)
		}
		for _, f := range files {
			name := dir + "-" + strings.TrimSuffix(filepath.Base(f), ".eml")
			t.Run(name, func(t *testing.T) {
				raw, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				out := []golden{}
				for _, x := range Message(mimeparse.Parse(raw), receivedAt) {
					g := golden{Kind: x.Kind, Value: x.Value, Confidence: x.Confidence, Source: x.Source}
					if !x.Expires.IsZero() {
						g.Expires = &x.Expires
					}
					out = append(out, g)
				}
				got, err := json.MarshalIndent(out, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, '\n')
				path := filepath.Join("testdata", name+".golden")
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
					t.Errorf("extractions differ from %s (run go test -update and review the diff)\ngot:\n%s", path, got)
				}
			})
		}
	}
}

// codes runs the matcher over a subject and body and returns the codes.
func codes(subject, body string) []string {
	var out []string
	for _, x := range Of(Input{Subject: subject, Body: body, ReceivedAt: receivedAt}) {
		if x.Kind == OTP {
			out = append(out, fmt.Sprintf("%s@%d", x.Value, x.Confidence))
		}
	}
	return out
}

// The precision cases, one line each: what is a code and what is not.
func TestCodes(t *testing.T) {
	cases := []struct {
		subject, body string
		want          string // value@confidence, comma separated
	}{
		// Taken.
		{"Your verification code is 482913", "", "482913@95"},
		{"482913 is your Example code", "", "482913@85"},
		{"", "验证码：739201，请勿告诉他人。", "739201@95"},
		{"", "您的验证码是739201", "739201@95"},
		{"", "739201（验证码）", "739201@95"},
		{"", "Your one-time passcode:\n\n  305817  \n", "305817@95"},
		{"", "Use this code for Example: 305817", "305817@80"},
		{"", "Your verification code for example.com is 305817", "305817@65"},
		{"", "Your security code\n\nPlease enter it on the sign-in page.\n\n305817\n", "305817@80"},
		{"Your sign-in code", "Hello,\n\nWe are glad you are here.\n\n305817\n", "305817@60"},
		{"", "If you can't click on the link, copy the verification code into your browser:\n\nTR7Y9CZ7TD\n", "TR7Y9CZ7TD@90"},
		{"", "Verification code: 594-216", "594216@95"},
		{"", "Your code: 4829 1374", "48291374@85"},
		{"", "Your OTP is H7K2QX", "H7K2QX@95"},
		{"", "G-482913 is your Example verification code.", "482913@95"},
		{"", "PIN: 2024", "2024@80"},
		{"", "認証コード：482019", "482019@95"},
		{"", "인증번호 [482019]", "482019@95"},
		{"", "Votre code de vérification est 482019", "482019@95"},

		// Not taken.
		{"", "Your order #4417283 has shipped.", ""},
		{"", "Verification code: 2026-11-01", ""},
		{"", "Your code expires at 10:30. Order total: $1299.", ""},
		{"", "Use promo code SAVE20 or discount code 4420.", ""},
		{"", "Confirmation code: K7QX2P", ""},
		{"", "Confirmation code:\n\nK7QX2P\n", ""},
		{"", "Your verification code was sent. Booking reference:\n\nK7QX2P\n", ""},
		{"", "Use the security code from your app. Your ticket:\n\nK7QX2P 12\n", ""},
		{"", "Your verification code: see the app. Order number 12345678.", ""},
		{"", "验证码已发送。如有疑问请致电 95555。", ""},
		{"", "验证码请勿告诉他人，客服电话 400-820-1234。", ""},
		{"", "Your code is below.\nCall +1 (555) 123-4567 for help.", ""},
		{"", "Your code expires in 10 minutes.", ""},
		{"", "Code 1234567890123", ""},
		{"", "Your verification code is in https://example.com/v?c=482913", ""},
		{"", "Your verification code expires; your account ending 4821 was charged.", ""},
		{"Statement for account ending 4821 is ready", "Your verification code is below.", ""},
		{"", "Your verification code is 48.2913", ""},
		{"", "取件码 5821，请于 3 天内取件。验证码见短信。", ""},
		{"", "The code is 2026 years old.", ""},
		{"", "your code: abcdef12", ""},
		{"", "ABCD-1234-EFGH is your license code", ""},
		{"", "Our code of conduct changed in 2026.", ""},
		{"Order code 12345678 confirmed", "", ""},
	}
	for _, c := range cases {
		got := strings.Join(codes(c.subject, c.body), ",")
		if got != c.want {
			t.Errorf("codes(%q, %q) = %q, want %q", c.subject, c.body, got, c.want)
		}
	}
}

func TestLinks(t *testing.T) {
	link := func(href, text, context string) mimeparse.Link {
		return mimeparse.Link{Href: href, Text: text, Context: context}
	}
	cases := []struct {
		subject, body string
		links         []mimeparse.Link
		want          string
	}{
		{"", "", []mimeparse.Link{link("https://example.com/verify?t=8f3a91c2e7", "Verify email", "")}, "https://example.com/verify?t=8f3a91c2e7@90"},
		{"", "", []mimeparse.Link{link("https://example.com/auth/email/alice@example.com/tr7y9cz7td", "Log in to Example", "")}, "https://example.com/auth/email/alice@example.com/tr7y9cz7td@90"},
		{"", "", []mimeparse.Link{link("https://click.example.net/c/Q2xpY2s0MjE", "Sign in", "")}, "https://click.example.net/c/Q2xpY2s0MjE@85"},
		{"", "", []mimeparse.Link{link("https://example.com/a/7Kq2mX9pZt", "here", "To confirm your address, click here:")}, "https://example.com/a/7Kq2mX9pZt@80"},
		{"", "Click the link below to sign in:\nhttps://example.com/magic?token=d41d8cd98f00.\n", nil, "https://example.com/magic?token=d41d8cd98f00@85"},
		{"Sign in to Example", "https://example.com/auth/cb?code=482913", nil, "https://example.com/auth/cb?code=482913@65"},
		{"", "点击下方链接完成验证：https://example.cn/verify?t=x82kd03jq7", nil, "https://example.cn/verify?t=x82kd03jq7@85"},

		{"", "", []mimeparse.Link{link("https://example.com/unsubscribe?u=1", "Unsubscribe", "Click here to verify")}, ""},
		{"", "", []mimeparse.Link{link("https://evil.example.net/login", "https://bank.example.com/login", "Please confirm at")}, ""},
		{"", "", []mimeparse.Link{link("javascript:alert(1)", "Verify", "")}, ""},
		{"", "", []mimeparse.Link{link("https://user:pw@example.com/verify", "Verify", "")}, ""},
		{"", "", []mimeparse.Link{link("https://example.com/verify.png", "Verify", "")}, ""},
		{"", "", []mimeparse.Link{link("https://example.com/shop", "Shop now", "Verify your taste")}, ""},
		{"", "Read our blog: https://example.com/blog", nil, ""},
		{"", "View this email in your browser: https://example.com/view/1", nil, ""},
		// Pages that are the same for every reader: no token in them.
		{"", "", []mimeparse.Link{link("https://example.com", "Example", "If you can't click on the link, copy the verification code into your browser:")}, ""},
		{"", "", []mimeparse.Link{link("https://example.com/login", "Sign in to Example", "")}, ""},
		{"", "", []mimeparse.Link{link("https://example.com/en/wizard/HelpWithLogin", "reset your password now", "If not, please")}, ""},
		{"", "", []mimeparse.Link{link("https://console.example.com/billing/?utm_medium=email&utm_campaign=invoice2026", "Log In", "")}, ""},
		{"Verify your email", "https://example.com/blog/2026-11-01/verify-your-email", nil, ""},
	}
	for _, c := range cases {
		var got []string
		for _, x := range Of(Input{Subject: c.subject, Body: c.body, Links: c.links, ReceivedAt: receivedAt}) {
			if x.Kind == Link {
				got = append(got, fmt.Sprintf("%s@%d", x.Value, x.Confidence))
			}
		}
		if s := strings.Join(got, ","); s != c.want {
			t.Errorf("links(%q, %q, %v) = %q, want %q", c.subject, c.body, c.links, s, c.want)
		}
	}
	if h := Host("https://Click.Example.NET:8443/x"); h != "click.example.net" {
		t.Errorf("Host = %q", h)
	}
	if h := Host("mailto:a@example.com"); h != "" {
		t.Errorf("Host of mailto = %q", h)
	}
}

func TestExpiry(t *testing.T) {
	date := receivedAt.Add(-2 * time.Minute)
	cases := []struct {
		body       string
		date       time.Time
		value      string
		from       time.Time
		confidence int
	}{
		{"Code: 482913. It expires in 10 minutes.", date, "PT10M", date, 85},
		{"验证码：482913，五分钟内有效。", time.Time{}, "PT5M", receivedAt, 85},
		{"验证码：482913，有效期为30分钟。", time.Time{}, "PT30M", receivedAt, 85},
		{"验证码：482913，十五分钟后失效。", time.Time{}, "PT15M", receivedAt, 85},
		{"Verification code: 482913\nValid for the next 2 hours.", time.Time{}, "PT2H", receivedAt, 85},
		{"Verification code: 482913\n" + strings.Repeat("Thanks for using Example. ", 20) + "\nValid for the next 2 hours.", time.Time{}, "PT2H", receivedAt, 70},
		{"Verification code: 482913. This code will be good for twenty-four hours.", time.Time{}, "PT24H", receivedAt, 85},
		{"Verification code: 482913. The link works for 3 days.", time.Time{}, "P3D", receivedAt, 85},
		{"認証コード：482019\n10分以内に入力してください。", time.Time{}, "PT10M", receivedAt, 85},
		// A Date in the future is the sender's claim: it does not
		// stretch the code's life.
		{"Code: 482913. It expires in 10 minutes.", receivedAt.Add(time.Hour), "PT10M", receivedAt, 85},
		// Nothing a code could last for, or no code at all.
		{"Code: 482913. It expires in 900 days.", time.Time{}, "", time.Time{}, 0},
		{"Your trial expires in 3 days.", time.Time{}, "", time.Time{}, 0},
	}
	for _, c := range cases {
		var got Extraction
		for _, x := range Of(Input{Body: c.body, Date: c.date, ReceivedAt: receivedAt}) {
			if x.Kind == Expiry {
				got = x
			}
		}
		if got.Value != c.value || got.Confidence != c.confidence || (c.value != "" && !got.Expires.Equal(c.from.Add(mustISO(t, c.value)))) {
			t.Errorf("expiry(%q) = %+v, want %s from %s at %d", c.body, got, c.value, c.from, c.confidence)
		}
	}
}

func mustISO(t *testing.T, s string) time.Duration {
	t.Helper()
	for _, d := range []time.Duration{5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 30 * time.Minute, 2 * time.Hour, 24 * time.Hour, 72 * time.Hour} {
		if iso(d) == s {
			return d
		}
	}
	t.Fatalf("no duration for %s", s)
	return 0
}

// Extraction is a pure function of its input: reparse relies on it.
func TestDeterministic(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("..", "..", "testdata", "*", "*.eml"))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		m := mimeparse.Parse(raw)
		a, _ := json.Marshal(Message(m, receivedAt))
		b, _ := json.Marshal(Message(m, receivedAt))
		if !bytes.Equal(a, b) {
			t.Errorf("%s: two runs differ", f)
		}
	}
}

func TestNeverPanics(t *testing.T) {
	inputs := []string{"", "code", "验证码", "code: ", "\xff\xfe code 123456 \xff", strings.Repeat("验证码 123456 ", 20000)}
	for _, in := range inputs {
		_ = Of(Input{Subject: in, Body: in, Links: []mimeparse.Link{{Href: in, Text: in, Context: in}}})
	}
}
