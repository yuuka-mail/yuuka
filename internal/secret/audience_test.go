package secret

import (
	"strings"
	"testing"
)

type audienceCase struct {
	name string
	f    func() (string, error)
}

func smtps(user, host string, port int) audienceCase {
	return audienceCase{"smtps " + user + " " + host, func() (string, error) { return SMTPS(user, host, port) }}
}

func imaps(user, host string, port int) audienceCase {
	return audienceCase{"imaps " + user + " " + host, func() (string, error) { return IMAPS(user, host, port) }}
}

func oauth(u, client string) audienceCase {
	return audienceCase{"oauth " + u + " " + client, func() (string, error) { return OAuth(u, client) }}
}

func bot(id int64) audienceCase {
	return audienceCase{"telegram", func() (string, error) { return TelegramBot(id) }}
}

// TestAudienceSpellings freezes the exact output. Changing it locks every
// secret sealed under the old spelling (see audience.go).
func TestAudienceSpellings(t *testing.T) {
	for want, c := range map[string]audienceCase{
		"smtps://agent%40example.com@smtp.example.com:465":                    smtps("agent@example.com", "smtp.example.com", 465),
		"smtps://u%3Ap%25x@192.0.2.1:587":                                     smtps("u:p%x", "192.0.2.1", 587),
		"smtps://u@[2001:db8::1]:465":                                         smtps("u", "2001:db8::1", 465),
		"smtps://u@xn--r8jz45g.jp:465":                                        smtps("u", "例え.jp", 465),
		"imaps://agent%40example.com@imap.example.com:993":                    imaps("agent@example.com", "imap.example.com", 993),
		"telegram:bot123456789":                                               bot(123456789),
		"oauth:https://oauth.example.com:443/token#client.example":            oauth("https://oauth.example.com/token", "client.example"),
		"oauth:https://login.example.com:443/t1/oauth2/v2.0/token?x=1#c%20id": oauth("https://login.example.com/t1/oauth2/v2.0/token?x=1", "c id"),
	} {
		got, err := c.f()
		if err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, want)
		}
	}
}

// TestAudienceEquivalents: spellings of one destination share an audience.
func TestAudienceEquivalents(t *testing.T) {
	groups := [][]audienceCase{
		{
			smtps("agent@example.com", "smtp.example.com", 465),
			smtps("agent@example.com", "SMTP.Example.COM", 465),
			smtps("agent@example.com", "ｓｍｔｐ．ｅｘａｍｐｌｅ．ｃｏｍ", 465),
		},
		{
			smtps("u", "例え.jp", 465),
			smtps("u", "xn--r8jz45g.jp", 465),
			smtps("u", "XN--R8JZ45G.JP", 465),
		},
		{
			smtps("u", "2001:db8::1", 465),
			smtps("u", "[2001:DB8:0:0::1]", 465),
		},
		{
			smtps("u", "192.0.2.1", 465),
			smtps("u", "::ffff:192.0.2.1", 465),
		},
		{
			oauth("https://oauth.example.com/token", "c"),
			oauth("HTTPS://OAuth.Example.com:443/token", "c"),
		},
		{
			oauth("https://oauth.example.com", "c"),
			oauth("https://oauth.example.com/", "c"),
		},
	}
	for _, g := range groups {
		first, err := g[0].f()
		if err != nil {
			t.Fatalf("%s: %v", g[0].name, err)
		}
		for _, c := range g[1:] {
			if got, err := c.f(); err != nil || got != first {
				t.Errorf("%s: got %q, %v; want %q", c.name, got, err, first)
			}
		}
	}
}

// TestAudienceDistinct: anything that changes where a connection goes, or
// who it logs in as, changes the audience.
func TestAudienceDistinct(t *testing.T) {
	cases := []audienceCase{
		smtps("agent@example.com", "smtp.example.com", 465),
		smtps("agent@example.com", "smtp.example.com", 587),
		smtps("agent@example.com", "smtp.example.net", 465),
		smtps("agent@example.com", "smtp.example.com.evil.example", 465),
		smtps("Agent@example.com", "smtp.example.com", 465),
		smtps("agent@example.com@smtp.example.com", "evil.example", 465),
		smtps("agent%40example.com", "smtp.example.com", 465),
		smtps("agent@example.com", "192.0.2.1", 465),
		smtps("agent@example.com", "192.0.2.10", 465),
		smtps("agent@example.com", "2001:db8::1", 465),
		smtps("agent@example.com", "2001:db8::1:465", 465),
		imaps("agent@example.com", "smtp.example.com", 465),
		bot(1),
		bot(12),
		bot(123456789),
		oauth("https://oauth.example.com/token", "c"),
		oauth("https://oauth.example.com/token/", "c"),
		oauth("https://oauth.example.com/Token", "c"),
		oauth("https://oauth.example.com:8443/token", "c"),
		oauth("https://oauth.example.com/token?tenant=a", "c"),
		oauth("https://oauth.example.com/token?tenant=b", "c"),
		oauth("https://oauth.example.com/token", "d"),
		oauth("https://oauth.example.com/token", "c#d"),
		oauth("https://oauth.example.com/token%23d", "c"),
		oauth("https://evil.example/token", "c"),
	}
	seen := map[string]string{}
	for _, c := range cases {
		a, err := c.f()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if prev, dup := seen[a]; dup {
			t.Errorf("%s and %s share audience %q", prev, c.name, a)
		}
		seen[a] = c.name
		if len(a) > MaxAudience {
			t.Errorf("%s: audience too long for the column", c.name)
		}
	}
}

func TestAudienceRejects(t *testing.T) {
	for _, c := range []audienceCase{
		smtps("", "smtp.example.com", 465),
		smtps("u", "", 465),
		smtps("u", "smtp.example.com", 0),
		smtps("u", "smtp.example.com", 65536),
		smtps("u", "smtp.example.com.", 465),
		smtps("u", "smtp..example.com", 465),
		smtps("u", "smtp_relay.example.com", 465),
		smtps("u", "smtp example.com", 465),
		smtps("u", "smtp.example.com:465", 465),
		smtps("u", "[192.0.2.1]", 465),
		smtps("u", "fe80::1%eth0", 465),
		smtps("u", "[smtp.example.com]", 465),
		smtps("agent", "example.com@smtp.example.com", 465),
		smtps("u", strings.Repeat("a", 64)+".example", 465),
		bot(0),
		bot(-5),
		oauth("https://oauth.example.com/token", ""),
		oauth("http://oauth.example.com/token", "c"),
		oauth("https://user:pw@oauth.example.com/token", "c"),
		oauth("https://oauth.example.com/token#frag", "c"),
		oauth("https://oauth.example.com/token#", "c"),
		oauth("https:///token", "c"),
		oauth("https:oauth.example.com", "c"),
		oauth("https://oauth.example.com:0/token", "c"),
		oauth("https://oauth.example.com:99999/token", "c"),
		oauth("https://oauth_x.example.com/token", "c"),
		oauth("::not a url", "c"),
	} {
		if a, err := c.f(); err == nil {
			t.Errorf("%s: accepted as %q", c.name, a)
		}
	}
}

func TestCanonicalHostDialable(t *testing.T) {
	// The canonical form is what the transport dials, so IPv6 comes back
	// bare for net.JoinHostPort, not bracketed.
	for in, want := range map[string]string{
		"[2001:db8::1]":    "2001:db8::1",
		"SMTP.Example.com": "smtp.example.com",
		"192.0.2.1":        "192.0.2.1",
	} {
		if got, err := CanonicalHost(in); err != nil || got != want {
			t.Errorf("%s: %q, %v", in, got, err)
		}
	}
}
