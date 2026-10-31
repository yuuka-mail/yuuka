package secret

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

// An audience names who a plaintext may be handed to. Each builder below
// returns the one canonical spelling for its kind of destination, so that
// equivalent inputs give the same audience and two destinations that a
// connection would tell apart never do. Two spellings of one destination
// that a builder does not unify (a trailing slash on a token URL, say)
// only cost a re-seal; the reverse would let a secret follow a changed
// destination, so ties are always broken toward "different".
//
// The guarantee holds only if the caller connects to exactly what the
// audience names: dial CanonicalHost(host), not the raw input, because
// the mapping below (case, full-width characters, IDNA) is not what the
// resolver does. Changing any builder's output for an existing input
// locks every secret sealed under the old spelling until it is set
// again, so the spellings are frozen like the sealed format.

// hostProfile is pinned rather than idna.Lookup, whose documented
// configuration "may change over time"; a silent change would re-spell
// hosts and lock their secrets. CheckHyphens stays off because real
// hostnames carry "--" in the third and fourth position.
var hostProfile = idna.New(
	idna.MapForLookup(),
	idna.Transitional(false),
	idna.BidiRule(),
	idna.CheckJoiners(true),
	idna.CheckHyphens(false),
	idna.VerifyDNSLength(true),
)

// CanonicalHost is the spelling of a host that audiences use and that the
// transport must dial. A domain name is mapped and lowercased per UTS #46
// and returned in its ASCII (punycode) form; an IP literal, bracketed or
// not, becomes its canonical text, with IPv4-mapped IPv6 unmapped.
// Zoned addresses, trailing dots and anything IDNA
// rejects are errors. IPv6 comes back without brackets, ready for
// net.JoinHostPort.
func CanonicalHost(host string) (string, error) {
	if host == "" {
		return "", errors.New("secret: empty host")
	}
	lit := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if a, err := netip.ParseAddr(lit); err == nil {
		if a.Zone() != "" {
			return "", errors.New("secret: zoned IP address cannot be an audience")
		}
		a = a.Unmap()
		if a.Is4() && lit != host {
			return "", fmt.Errorf("secret: bracketed IPv4 address %q", host)
		}
		return a.String(), nil
	}
	if strings.ContainsAny(host, "[]") {
		return "", fmt.Errorf("secret: host %q is not a valid IP literal", host)
	}
	if strings.HasSuffix(host, ".") {
		// Rooted and relative names can resolve differently; make the
		// operator pick one rather than guess.
		return "", fmt.Errorf("secret: host %q ends in a dot", host)
	}
	a, err := hostProfile.ToASCII(host)
	if err != nil {
		return "", fmt.Errorf("secret: host %q: %w", host, err)
	}
	return a, nil
}

// SMTPS is the audience of an SMTP submission password, "smtps://<user>@
// <host>:<port>": implicit TLS, which is the only mode the transport has.
// A STARTTLS mode, if one is ever added, needs its own scheme, since a
// downgrade must not inherit the secret. The username is compared byte
// for byte (servers differ on case) and percent-encoded so that '@' or
// ':' inside it stays unambiguous; the port is always written out.
func SMTPS(user, host string, port int) (string, error) {
	return serverAudience("smtps", user, host, port)
}

// IMAPS is the audience of an IMAP password, "imaps://<user>@<host>:
// <port>", with the same rules as SMTPS.
func IMAPS(user, host string, port int) (string, error) {
	return serverAudience("imaps", user, host, port)
}

func serverAudience(scheme, user, host string, port int) (string, error) {
	if user == "" {
		return "", fmt.Errorf("secret: %s audience needs a username", scheme)
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("secret: port %d out of range", port)
	}
	h, err := CanonicalHost(host)
	if err != nil {
		return "", err
	}
	return scheme + "://" + escape(user) + "@" + net.JoinHostPort(h, strconv.Itoa(port)), nil
}

// TelegramBot is the audience of a bot token, "telegram:bot<id>", with the
// bot's numeric id (the part of the token before the colon). The Bot API
// host is fixed in code; if a configurable one (a local Bot API server) is
// ever added, it has to become part of this audience.
func TelegramBot(botID int64) (string, error) {
	if botID <= 0 {
		return "", fmt.Errorf("secret: telegram bot id %d is not positive", botID)
	}
	return "telegram:bot" + strconv.FormatInt(botID, 10), nil
}

// OAuth is the audience of an OAuth client secret or refresh token,
// "oauth:<token-url>#<client-id>". The token URL must be https without
// user info or fragment; its scheme and host are canonicalised, the port
// is written out, and the path and query are kept exactly as they are
// escaped on the wire. The client id is percent-encoded like a username.
func OAuth(tokenURL, clientID string) (string, error) {
	if clientID == "" {
		return "", errors.New("secret: oauth audience needs a client id")
	}
	// Errors here do not quote the URL: one with user info would print a
	// password.
	u, err := url.Parse(tokenURL)
	switch {
	case err != nil:
		return "", errors.New("secret: token URL does not parse")
	case !strings.EqualFold(u.Scheme, "https"):
		return "", errors.New("secret: token URL is not https")
	case u.Opaque != "" || u.Host == "":
		return "", errors.New("secret: token URL has no host")
	case u.User != nil:
		return "", errors.New("secret: token URL carries user info")
	case strings.Contains(tokenURL, "#"):
		return "", errors.New("secret: token URL has a fragment")
	}
	h, err := CanonicalHost(u.Hostname())
	if err != nil {
		return "", err
	}
	port := 443
	if p := u.Port(); p != "" {
		if port, err = strconv.Atoi(p); err != nil || port < 1 || port > 65535 {
			return "", errors.New("secret: token URL has a bad port")
		}
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	a := "oauth:https://" + net.JoinHostPort(h, strconv.Itoa(port)) + path
	if u.RawQuery != "" || u.ForceQuery {
		a += "?" + u.RawQuery
	}
	return a + "#" + escape(clientID), nil
}

// escape percent-encodes every byte outside RFC 3986's unreserved set,
// with uppercase hex. Written out rather than borrowed from net/url so the
// spelling cannot drift with the standard library; it is injective because
// '%' itself is always encoded.
func escape(s string) string {
	const hexdig = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexdig[c>>4])
		b.WriteByte(hexdig[c&15])
	}
	return b.String()
}
