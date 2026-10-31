package mailnet

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"

	"github.com/pires/go-proxyproto"
)

// The PROXY protocol, versions 1 and 2
// (https://www.haproxy.org/download/2.9/doc/proxy-protocol.txt), parsed by
// go-proxyproto. It replaced a parser of our own: the wire format is
// small, but the library carries what only exposure teaches, such as room
// for a client certificate TLV, rejecting undefined family bytes even
// under LOCAL, and fixes for remotely triggerable panics. What stays here
// is policy: which errors mean what to the listener, and that only a TCP
// client address is ever accepted.

// errMalformed is wrapped by every parse error, so callers can tell a
// broken header from an I/O failure.
var errMalformed = errors.New("malformed PROXY header")

// header is what a PROXY header says about the connection.
type header struct {
	// local is set when the proxy reports no client: a v2 LOCAL command
	// (e.g. its own health check) or a v1 UNKNOWN. The spec says to use
	// the real connection endpoints then.
	local bool
	src   netip.AddrPort
}

// readHeader consumes one PROXY header from r and nothing beyond it.
// Input that does not start with a PROXY signature is ErrMissingHeader.
func readHeader(r *bufio.Reader) (header, error) {
	// A peer that closes or stalls before its first byte is an I/O
	// matter, not a missing header; the library would call it the latter.
	if _, err := r.Peek(1); err != nil {
		return header{}, err
	}
	h, err := proxyproto.Read(r)
	if err != nil {
		var ne net.Error
		switch {
		case errors.Is(err, proxyproto.ErrNoProxyProtocol):
			return header{}, ErrMissingHeader
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			return header{}, fmt.Errorf("truncated PROXY header: %w", io.ErrUnexpectedEOF)
		case errors.As(err, &ne):
			return header{}, err
		}
		return header{}, fmt.Errorf("%w: %w", errMalformed, err)
	}
	if h.Command.IsLocal() {
		return header{local: true}, nil
	}
	// The library also accepts UDP and AF_UNIX sources, which make no
	// sense in front of a TCP mail listener; refusing them beats guessing
	// an address. It already refuses PROXY with UNSPEC, which would
	// attribute the client to the proxy.
	ta, ok := h.SourceAddr.(*net.TCPAddr)
	if !ok || (h.TransportProtocol != proxyproto.TCPv4 && h.TransportProtocol != proxyproto.TCPv6) {
		return header{}, fmt.Errorf("%w: family/protocol 0x%02x", errMalformed, byte(h.TransportProtocol))
	}
	ap := ta.AddrPort()
	return header{src: netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())}, nil
}
