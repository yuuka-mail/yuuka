package clientip

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient(t *testing.T) {
	t.Parallel()
	// A host proxy on loopback, plus an internal network of proxies.
	const trusted = "127.0.0.1/32, ::1/128, 10.0.0.0/8"
	cases := []struct {
		name    string
		proxies string
		peer    string
		xff     []string // one element per header line
		want    string
	}{
		{"no proxies configured", "", "198.51.100.7:4000", []string{"203.0.113.9"}, "198.51.100.7"},
		{"no proxies, loopback peer", "", "127.0.0.1:4000", []string{"203.0.113.9"}, "127.0.0.1"},
		{"untrusted peer spoofs the header", trusted, "198.51.100.7:4000", []string{"203.0.113.9"}, "198.51.100.7"},
		{"untrusted peer claims to be a proxy", trusted, "198.51.100.7:4000", []string{"127.0.0.1"}, "198.51.100.7"},
		{"trusted peer, one hop", trusted, "127.0.0.1:4000", []string{"203.0.113.9"}, "203.0.113.9"},
		{"trusted peer, no header", trusted, "127.0.0.1:4000", nil, "127.0.0.1"},
		{"trusted peer, empty header", trusted, "127.0.0.1:4000", []string{""}, "127.0.0.1"},
		// The client wrote 192.0.2.66 itself; the proxy appended what it saw.
		{"client-supplied entries are skipped", trusted, "127.0.0.1:4000", []string{"192.0.2.66, 203.0.113.9"}, "203.0.113.9"},
		{"multiple trusted hops", trusted, "127.0.0.1:4000", []string{"192.0.2.66, 203.0.113.9, 10.1.2.3, 10.4.5.6"}, "203.0.113.9"},
		{"hops split over header lines", trusted, "127.0.0.1:4000", []string{"192.0.2.66, 203.0.113.9", "10.1.2.3"}, "203.0.113.9"},
		{"every hop trusted", trusted, "127.0.0.1:4000", []string{"10.1.2.3, 10.4.5.6"}, "10.1.2.3"},
		{"IPv6 peer and client", trusted, "[::1]:4000", []string{"2001:db8::1"}, "2001:db8::1"},
		{"IPv6 client in brackets with port", trusted, "[::1]:4000", []string{"[2001:db8::1]:5555"}, "2001:db8::1"},
		{"IPv4 client with port", trusted, "127.0.0.1:4000", []string{"203.0.113.9:5555"}, "203.0.113.9"},
		{"IPv6 client normalized", trusted, "[::1]:4000", []string{"2001:DB8:0:0::1"}, "2001:db8::1"},
		{"IPv4-mapped peer is trusted", trusted, "[::ffff:127.0.0.1]:4000", []string{"203.0.113.9"}, "203.0.113.9"},
		{"IPv4-mapped client unmapped", trusted, "127.0.0.1:4000", []string{"::ffff:203.0.113.9"}, "203.0.113.9"},
		{"untrusted IPv6 peer", trusted, "[2001:db8::7]:4000", []string{"203.0.113.9"}, "2001:db8::7"},
		{"malformed rightmost entry", trusted, "127.0.0.1:4000", []string{"203.0.113.9, not-an-ip"}, "127.0.0.1"},
		{"malformed entry between trusted hops", trusted, "127.0.0.1:4000", []string{"203.0.113.9, unknown, 10.1.2.3"}, "127.0.0.1"},
		{"malformed entry left of the client is ignored", trusted, "127.0.0.1:4000", []string{"garbage, 203.0.113.9"}, "203.0.113.9"},
		{"empty entry", trusted, "127.0.0.1:4000", []string{"203.0.113.9, "}, "127.0.0.1"},
		{"obfuscated identifier (RFC 7239 style)", trusted, "127.0.0.1:4000", []string{"_hidden"}, "127.0.0.1"},
		{"zone is dropped", trusted, "127.0.0.1:4000", []string{"fe80::1%eth0"}, "fe80::1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p, err := ParseProxies(c.proxies)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			r.RemoteAddr = c.peer
			for _, v := range c.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := p.Client(r); got != c.want {
				t.Errorf("Client = %q, want %q", got, c.want)
			}
		})
	}
}

func TestZeroValueTrustsNobody(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.RemoteAddr = "127.0.0.1:4000"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := (Proxies{}).Client(r); got != "127.0.0.1" {
		t.Errorf("zero Proxies: %q", got)
	}
}

func TestParseProxies(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"", " ", "127.0.0.1/32", "127.0.0.1/32, ::1/128", "10.0.0.0/8,", "192.0.2.1", "::1", "10.1.2.3/8", "::ffff:10.0.0.0/104"} {
		if _, err := ParseProxies(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"localhost", "10.0.0.0/33", "127.0.0.1/32;::1/128", "fe80::1%eth0", "*"} {
		if _, err := ParseProxies(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// A bare address trusts only itself; a 4-in-6 prefix matches IPv4.
	p, _ := ParseProxies("192.0.2.1, ::ffff:10.0.0.0/104")
	for addr, want := range map[string]bool{"192.0.2.1": true, "192.0.2.2": false, "10.9.9.9": true, "11.0.0.1": false} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		r.RemoteAddr = addr + ":1"
		r.Header.Set("X-Forwarded-For", "203.0.113.9")
		if got := p.Client(r) == "203.0.113.9"; got != want {
			t.Errorf("%s trusted = %v, want %v", addr, got, want)
		}
	}
}
