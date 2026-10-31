package mailnet

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
)

// The PROXY protocol, versions 1 and 2, as specified by HAProxy
// (https://www.haproxy.org/download/2.9/doc/proxy-protocol.txt). Only
// what a TCP listener needs: the client's address. Destination
// addresses are validated but not kept, and v2 TLVs are skipped
// unread, since nothing here would act on them.

const (
	// v1MaxLen is the longest v1 line the spec allows, CRLF included.
	v1MaxLen = 107
	// v2MaxBody caps the v2 length field. The spec allows 65535, but the
	// largest address block is 216 bytes (AF_UNIX) and what follows is
	// TLVs nobody here reads; 1 KiB leaves room for any a proxy would
	// plausibly send while bounding what a peer can make us buffer.
	v2MaxBody = 1024
	// v2HeaderLen is the fixed part: signature, version/command,
	// family/protocol and length.
	v2HeaderLen = 16
)

var v2Sig = []byte("\r\n\r\n\x00\r\nQUIT\n")

// errMalformed is wrapped by every parse error, so callers can tell a
// broken header from an I/O failure.
var errMalformed = errors.New("malformed PROXY header")

// header is what a PROXY header says about the connection.
type header struct {
	// local is set when the proxy reports no client address: a v2 LOCAL
	// command (e.g. its own health check), a v1 UNKNOWN or a v2 UNSPEC.
	// The spec says to use the real connection endpoints then.
	local bool
	src   netip.AddrPort
}

func malformed(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errMalformed, fmt.Sprintf(format, args...))
}

// readHeader consumes one PROXY header from r and nothing beyond it.
// The first byte picks the version; anything else is ErrMissingHeader.
func readHeader(r *bufio.Reader) (header, error) {
	b, err := r.Peek(1)
	if err != nil {
		return header{}, err
	}
	switch b[0] {
	case 'P':
		return readV1(r)
	case v2Sig[0]:
		return readV2(r)
	}
	return header{}, ErrMissingHeader
}

// readV1 reads "PROXY TCP4|TCP6 <src> <dst> <sport> <dport>\r\n" or
// "PROXY UNKNOWN...\r\n", at most v1MaxLen bytes.
func readV1(r *bufio.Reader) (header, error) {
	var line []byte
	for {
		c, err := r.ReadByte()
		if err != nil {
			return header{}, unexpectedEOF(err)
		}
		line = append(line, c)
		if c == '\n' {
			break
		}
		if len(line) >= v1MaxLen {
			return header{}, malformed("v1 line longer than %d bytes", v1MaxLen)
		}
	}
	s, ok := strings.CutSuffix(string(line), "\r\n")
	if !ok || strings.ContainsAny(s, "\r\n") {
		return header{}, malformed("v1 line not terminated by CRLF")
	}
	rest, ok := strings.CutPrefix(s, "PROXY ")
	if !ok {
		return header{}, malformed("v1 line does not start with \"PROXY \"")
	}
	// The spec lets UNKNOWN be followed by anything, which the receiver
	// ignores.
	if rest == "UNKNOWN" || strings.HasPrefix(rest, "UNKNOWN ") {
		return header{local: true}, nil
	}
	f := strings.Split(rest, " ")
	if len(f) != 5 {
		return header{}, malformed("v1 line has %d fields after PROXY, want 5", len(f))
	}
	var want func(netip.Addr) bool
	switch f[0] {
	case "TCP4":
		want = netip.Addr.Is4
	case "TCP6":
		want = netip.Addr.Is6
	default:
		return header{}, malformed("v1 protocol %q", f[0])
	}
	src, err := v1Addr(f[1], want)
	if err != nil {
		return header{}, err
	}
	if _, err := v1Addr(f[2], want); err != nil {
		return header{}, err
	}
	sport, err := v1Port(f[3])
	if err != nil {
		return header{}, err
	}
	if _, err := v1Port(f[4]); err != nil {
		return header{}, err
	}
	return header{src: netip.AddrPortFrom(src.Unmap(), sport)}, nil
}

func v1Addr(s string, want func(netip.Addr) bool) (netip.Addr, error) {
	a, err := netip.ParseAddr(s)
	if err != nil || a.Zone() != "" || !want(a) {
		return netip.Addr{}, malformed("v1 address %q", s)
	}
	return a, nil
}

// v1Port accepts what the spec allows: decimal 0–65535 without leading
// zeros or a sign.
func v1Port(s string) (uint16, error) {
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil || s[0] == '+' || (len(s) > 1 && s[0] == '0') {
		return 0, malformed("v1 port %q", s)
	}
	return uint16(n), nil
}

const (
	v2CmdLocal = 0x0
	v2CmdProxy = 0x1

	v2FamUnspec = 0x0
	v2FamInet   = 0x1
	v2FamInet6  = 0x2

	v2ProtoUnspec = 0x0
	v2ProtoStream = 0x1
)

// readV2 reads the binary header: 16 fixed bytes, then as many as its
// length field says, of which only the address block is looked at.
func readV2(r *bufio.Reader) (header, error) {
	fixed, err := r.Peek(v2HeaderLen)
	if err != nil {
		if len(fixed) > 0 && !bytes.HasPrefix(v2Sig, fixed[:min(len(fixed), len(v2Sig))]) {
			return header{}, malformed("v2 signature")
		}
		return header{}, unexpectedEOF(err)
	}
	if !bytes.Equal(fixed[:len(v2Sig)], v2Sig) {
		return header{}, malformed("v2 signature")
	}
	verCmd, famProto := fixed[12], fixed[13]
	n := int(binary.BigEndian.Uint16(fixed[14:16]))
	if verCmd>>4 != 2 {
		return header{}, malformed("v2 version %d", verCmd>>4)
	}
	if n > v2MaxBody {
		return header{}, malformed("v2 length %d exceeds %d", n, v2MaxBody)
	}
	cmd, fam, proto := verCmd&0xf, famProto>>4, famProto&0xf
	// Decide everything that does not need the body before waiting for
	// it, so a bad header fails without reading n more bytes.
	var need int
	switch cmd {
	case v2CmdLocal:
		// The spec says to ignore the family and address block.
	case v2CmdProxy:
		switch {
		case fam == v2FamUnspec && proto == v2ProtoUnspec:
		case fam == v2FamInet && proto == v2ProtoStream:
			need = 12
		case fam == v2FamInet6 && proto == v2ProtoStream:
			need = 36
		default:
			// AF_UNIX and datagram sources make no sense in front of a TCP
			// mail listener; refusing them beats guessing an address.
			return header{}, malformed("v2 family/protocol 0x%02x", famProto)
		}
	default:
		return header{}, malformed("v2 command %d", cmd)
	}
	if n < need {
		return header{}, malformed("v2 length %d too short for family 0x%x", n, fam)
	}
	buf, err := r.Peek(v2HeaderLen + n)
	if err != nil {
		return header{}, unexpectedEOF(err)
	}
	body := buf[v2HeaderLen:]
	var h header
	switch {
	case cmd == v2CmdLocal || fam == v2FamUnspec:
		h.local = true
	case fam == v2FamInet:
		h.src = netip.AddrPortFrom(netip.AddrFrom4([4]byte(body[0:4])), binary.BigEndian.Uint16(body[8:10]))
	case fam == v2FamInet6:
		src := netip.AddrFrom16([16]byte(body[0:16])).Unmap()
		h.src = netip.AddrPortFrom(src, binary.BigEndian.Uint16(body[32:34]))
	}
	if _, err := r.Discard(v2HeaderLen + n); err != nil {
		return header{}, err
	}
	return h, nil
}

// unexpectedEOF turns a clean EOF in the middle of a header into the
// error that says so.
func unexpectedEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return fmt.Errorf("truncated PROXY header: %w", io.ErrUnexpectedEOF)
	}
	return err
}
