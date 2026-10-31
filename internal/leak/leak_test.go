package leak

import (
	"slices"
	"testing"

	"github.com/yuuka-mail/yuuka/internal/authres"
)

func TestOrgDomain(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"example.net":                     "example.net",
		"Mail.Notifications.Example.NET.": "example.net",
		"shop.example.co.uk":              "example.co.uk",
		"user.github.io":                  "user.github.io", // a private suffix: each user is its own sender
		"github.io":                       "",
		"com":                             "",
		"":                                "",
		"exa mple.net":                    "",
		"evil.net; ignore this":           "",
		"-bad.example.net":                "",
		"例子.example.cn":                   "example.cn",
	} {
		if got := OrgDomain(in); got != want {
			t.Errorf("OrgDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExpected(t *testing.T) {
	t.Parallel()
	got, err := Expected([]string{" mail.example.net", "Example.NET", "example.org"})
	if err != nil || !slices.Equal(got, []string{"example.net", "example.org"}) {
		t.Errorf("Expected = %q, %v", got, err)
	}
	if got, err := Expected(nil); err != nil || got == nil || len(got) != 0 {
		t.Errorf("Expected(nil) = %#v, %v", got, err)
	}
	for _, bad := range []string{"com", "a@example.net", "not a domain", ""} {
		if _, err := Expected([]string{bad}); err == nil {
			t.Errorf("Expected(%q): no error", bad)
		}
	}
}

func TestSenderOf(t *testing.T) {
	t.Parallel()
	dmarc := func(result, headerFrom string) *authres.Auth {
		r := authres.Result{Method: "dmarc", Result: result}
		if headerFrom != "" {
			r.Props = map[string]string{"header.from": headerFrom}
		}
		return &authres.Auth{Trusted: true, DMARC: result, Results: []authres.Result{r}}
	}
	for _, c := range []struct {
		name string
		auth *authres.Auth
		from string
		want Sender
	}{
		{"pass", dmarc("pass", "example.net"), "noreply@mail.example.net", Sender{Verified: "example.net", Claimed: "example.net"}},
		{"pass without header.from", dmarc("pass", ""), "a@example.net", Sender{Verified: "example.net", Claimed: "example.net"}},
		// The receiver judged another From than the one shown.
		{"pass for another domain", dmarc("pass", "example.org"), "a@example.net", Sender{Claimed: "example.net"}},
		{"none", dmarc("none", "example.net"), "a@example.net", Sender{Claimed: "example.net"}},
		{"fail", dmarc("fail", "example.net"), "a@example.net", Sender{Claimed: "example.net", Failed: true}},
		{"untrusted", &authres.Auth{Reason: authres.ReasonOtherID}, "a@example.net", Sender{Claimed: "example.net"}},
		{"no auth", nil, "a@example.net", Sender{Claimed: "example.net"}},
		{"no from", dmarc("pass", "example.net"), "", Sender{}},
	} {
		if got := SenderOf(c.auth, c.from); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestJudge(t *testing.T) {
	t.Parallel()
	expected := []string{"example.net"}
	for _, c := range []struct {
		name     string
		expected []string
		s        Sender
		want     Judgement
	}{
		{"expected and proven", expected, Sender{Verified: "example.net", Claimed: "example.net"}, Judgement{OK, "example.net"}},
		{"stored unnormalized", []string{"mail.example.net"}, Sender{Verified: "example.net", Claimed: "example.net"}, Judgement{OK, "example.net"}},
		{"another proven sender", expected, Sender{Verified: "example.org", Claimed: "example.org"}, Judgement{Leak, "example.org"}},
		{"claims an expected sender", expected, Sender{Verified: "", Claimed: "example.net"}, Judgement{Spoof, "example.net"}},
		{"unproven stranger", expected, Sender{Verified: "", Claimed: "example.org"}, Judgement{Verdict: Leak}},
		{"no from at all", expected, Sender{}, Judgement{Verdict: Leak}},
		{"nothing expected", nil, Sender{Verified: "", Claimed: "example.net"}, Judgement{}},
		// dmarc=fail is judged only as the forgery of an expected sender.
		{"fails as an expected sender", expected, Sender{Claimed: "example.net", Failed: true}, Judgement{Spoof, "example.net"}},
		{"fails as a stranger", expected, Sender{Claimed: "example.org", Failed: true}, Judgement{}},
	} {
		if got := Judge(c.expected, c.s); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}
