// Package clientip finds the address of the client an HTTP request came
// from, believing X-Forwarded-For only from configured reverse proxies.
//
// yuuka assumes nothing about what is in front of it (§2), so by default
// the client is the TCP peer and forwarding headers are ignored: anyone
// can write them. Behind a proxy that makes every client look like the
// proxy, which collapses per-client rate limits into one bucket and
// records only the proxy in the audit log. Naming the proxy fixes that
// without trusting anyone else.
package clientip

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Proxies is a set of proxy addresses. For HTTP it is the set whose
// X-Forwarded-For is believed; internal/mailnet uses a separate instance
// for the PROXY protocol, with the same syntax. The zero value believes
// nobody.
type Proxies struct {
	trusted []netip.Prefix
}

// ParseProxies reads a comma-separated list of CIDRs; a bare address
// stands for itself alone. Empty means no proxy is trusted.
func ParseProxies(s string) (Proxies, error) {
	var p Proxies
	for f := range strings.SplitSeq(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		pfx, err := netip.ParsePrefix(f)
		if err != nil {
			a, aerr := netip.ParseAddr(f)
			if aerr != nil || a.Zone() != "" {
				return Proxies{}, fmt.Errorf("trusted proxy %q: want a CIDR such as 127.0.0.1/32 or ::1/128", f)
			}
			pfx = netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen())
		}
		if pfx.Addr().Is4In6() {
			pfx = netip.PrefixFrom(pfx.Addr().Unmap(), max(pfx.Bits()-96, 0))
		}
		p.trusted = append(p.trusted, pfx.Masked())
	}
	return p, nil
}

// Empty reports whether no proxy is trusted.
func (p Proxies) Empty() bool { return len(p.trusted) == 0 }

// Trusts reports whether a is one of the proxies. A 4-in-6 address
// matches its IPv4 form; a zone is ignored.
func (p Proxies) Trusts(a netip.Addr) bool {
	a = a.WithZone("").Unmap()
	for _, pfx := range p.trusted {
		if pfx.Contains(a) {
			return true
		}
	}
	return false
}

// Client returns the client's address. If the TCP peer is a trusted
// proxy, X-Forwarded-For is walked from the right, past every trusted
// hop, and the first address that is not trusted is the client: entries
// to the left of it were written by that client and prove nothing. The
// leftmost entry is never taken blindly.
//
// When the header cannot be believed (an untrusted peer, no header, or
// a malformed entry before an untrusted address is reached) the answer
// is the peer itself, as if no proxy were configured: a client must not
// be able to pick its own identity by sending garbage.
func (p Proxies) Client(r *http.Request) string {
	peer, ok := parseHost(r.RemoteAddr)
	if !ok {
		return r.RemoteAddr
	}
	if !p.Trusts(peer) {
		return peer.String()
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	leftmost := peer
	for i := len(hops) - 1; i >= 0; i-- {
		a, ok := parseHop(hops[i])
		if !ok {
			return peer.String()
		}
		if !p.Trusts(a) {
			return a.String()
		}
		leftmost = a
	}
	// Every hop is a trusted proxy: the request started at one of them,
	// e.g. a health check.
	return leftmost.String()
}

// parseHost reads the host of a "host:port" peer address.
func parseHost(s string) (netip.Addr, bool) {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().WithZone("").Unmap(), true
	}
	host, _, err := net.SplitHostPort(s)
	if err != nil {
		host = s
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.WithZone("").Unmap(), true
}

// parseHop reads one X-Forwarded-For entry: an address, possibly with a
// port (some proxies add one), IPv6 possibly in brackets.
func parseHop(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if a, err := netip.ParseAddr(s); err == nil {
		return a.WithZone("").Unmap(), true
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().WithZone("").Unmap(), true
	}
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		if a, err := netip.ParseAddr(s[1 : len(s)-1]); err == nil {
			return a.WithZone("").Unmap(), true
		}
	}
	return netip.Addr{}, false
}
