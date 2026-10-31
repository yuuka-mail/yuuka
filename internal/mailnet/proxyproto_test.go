package mailnet

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net/netip"
	"strings"
	"testing"
)

// v2 builds a v2 header: verCmd and famProto bytes, then body, with the
// length field set to len(body).
func v2(verCmd, famProto byte, body []byte) []byte {
	b := append([]byte{}, v2Sig...)
	b = append(b, verCmd, famProto)
	b = binary.BigEndian.AppendUint16(b, uint16(len(body)))
	return append(b, body...)
}

func inet4Body(src, dst string, sport, dport uint16) []byte {
	s, d := netip.MustParseAddr(src).As4(), netip.MustParseAddr(dst).As4()
	b := append(s[:], d[:]...)
	b = binary.BigEndian.AppendUint16(b, sport)
	return binary.BigEndian.AppendUint16(b, dport)
}

func inet6Body(src, dst string, sport, dport uint16) []byte {
	s, d := netip.MustParseAddr(src).As16(), netip.MustParseAddr(dst).As16()
	b := append(s[:], d[:]...)
	b = binary.BigEndian.AppendUint16(b, sport)
	return binary.BigEndian.AppendUint16(b, dport)
}

func TestReadHeader(t *testing.T) {
	tlv := []byte{0x04, 0x00, 0x03, 'a', 'b', 'c'} // PP2_TYPE_NOOP, length 3
	tests := []struct {
		name  string
		in    []byte
		src   string // "" with local means LOCAL; ignored on error
		local bool
		err   error // nil: success
	}{
		{name: "v1 tcp4", in: []byte("PROXY TCP4 203.0.113.7 192.0.2.1 4242 465\r\n"), src: "203.0.113.7:4242"},
		{name: "v1 tcp6", in: []byte("PROXY TCP6 2001:db8::7 2001:db8::1 4242 465\r\n"), src: "[2001:db8::7]:4242"},
		{name: "v1 tcp6 mapped v4 is unmapped", in: []byte("PROXY TCP6 ::ffff:203.0.113.7 ::1 1 2\r\n"), src: "203.0.113.7:1"},
		{name: "v1 longest allowed", in: []byte("PROXY TCP6 ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff 65535 65535\r\n"), src: "[ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff]:65535"},
		{name: "v1 port zero", in: []byte("PROXY TCP4 203.0.113.7 192.0.2.1 0 465\r\n"), src: "203.0.113.7:0"},
		{name: "v1 unknown", in: []byte("PROXY UNKNOWN\r\n"), local: true},
		{name: "v1 unknown with junk", in: []byte("PROXY UNKNOWN ffff:: 1.2.3.4 9 9\r\n"), local: true},
		{name: "v2 tcp4", in: v2(0x21, 0x11, inet4Body("203.0.113.7", "192.0.2.1", 4242, 465)), src: "203.0.113.7:4242"},
		{name: "v2 tcp6", in: v2(0x21, 0x21, inet6Body("2001:db8::7", "2001:db8::1", 4242, 465)), src: "[2001:db8::7]:4242"},
		{name: "v2 tcp6 mapped v4 is unmapped", in: v2(0x21, 0x21, inet6Body("::ffff:203.0.113.7", "::1", 9, 465)), src: "203.0.113.7:9"},
		{name: "v2 tcp4 with tlv", in: v2(0x21, 0x11, append(inet4Body("203.0.113.7", "192.0.2.1", 1, 2), tlv...)), src: "203.0.113.7:1"},
		{name: "v2 tlv garbage is skipped", in: v2(0x21, 0x11, append(inet4Body("203.0.113.7", "192.0.2.1", 1, 2), 0xff, 0xff)), src: "203.0.113.7:1"},
		{name: "v2 local", in: v2(0x20, 0x00, nil), local: true},
		{name: "v2 local ignores family and body", in: v2(0x20, 0x11, inet4Body("203.0.113.7", "192.0.2.1", 1, 2)), local: true},
		{name: "v2 proxy unspec", in: v2(0x21, 0x00, nil), local: true},
		{name: "v2 max length", in: v2(0x21, 0x11, append(inet4Body("203.0.113.7", "192.0.2.1", 1, 2), make([]byte, v2MaxBody-12)...)), src: "203.0.113.7:1"},

		{name: "not a header", in: []byte{0x16, 0x03, 0x01}, err: ErrMissingHeader},
		{name: "smtp command", in: []byte("EHLO example.com\r\n"), err: ErrMissingHeader},
		{name: "empty", in: nil, err: io.EOF},

		{name: "v1 lowercase", in: []byte("proxy TCP4 1.2.3.4 5.6.7.8 1 2\r\n"), err: ErrMissingHeader},
		{name: "v1 no space", in: []byte("PROXYTCP4 1.2.3.4 5.6.7.8 1 2\r\n"), err: errMalformed},
		{name: "v1 bare LF", in: []byte("PROXY TCP4 1.2.3.4 5.6.7.8 1 2\n"), err: errMalformed},
		{name: "v1 CR inside", in: []byte("PROXY TCP4 1.2.3.4\r5.6.7.8 1 2\r\n"), err: errMalformed},
		{name: "v1 double space", in: []byte("PROXY TCP4  1.2.3.4 5.6.7.8 1 2\r\n"), err: errMalformed},
		{name: "v1 trailing space", in: []byte("PROXY TCP4 1.2.3.4 5.6.7.8 1 2 \r\n"), err: errMalformed},
		{name: "v1 too few fields", in: []byte("PROXY TCP4 1.2.3.4 5.6.7.8 1\r\n"), err: errMalformed},
		{name: "v1 bad protocol", in: []byte("PROXY UDP4 1.2.3.4 5.6.7.8 1 2\r\n"), err: errMalformed},
		{name: "v1 tcp4 with v6", in: []byte("PROXY TCP4 2001:db8::1 5.6.7.8 1 2\r\n"), err: errMalformed},
		{name: "v1 tcp4 with mapped v6", in: []byte("PROXY TCP4 ::ffff:1.2.3.4 5.6.7.8 1 2\r\n"), err: errMalformed},
		{name: "v1 tcp6 with v4", in: []byte("PROXY TCP6 1.2.3.4 ::1 1 2\r\n"), err: errMalformed},
		{name: "v1 zone", in: []byte("PROXY TCP6 fe80::1%eth0 ::1 1 2\r\n"), err: errMalformed},
		{name: "v1 bad dst", in: []byte("PROXY TCP4 1.2.3.4 example.com 1 2\r\n"), err: errMalformed},
		{name: "v1 leading zero ip", in: []byte("PROXY TCP4 01.2.3.4 5.6.7.8 1 2\r\n"), err: errMalformed},
		{name: "v1 leading zero port", in: []byte("PROXY TCP4 1.2.3.4 5.6.7.8 01 2\r\n"), err: errMalformed},
		{name: "v1 plus port", in: []byte("PROXY TCP4 1.2.3.4 5.6.7.8 +1 2\r\n"), err: errMalformed},
		{name: "v1 port too big", in: []byte("PROXY TCP4 1.2.3.4 5.6.7.8 65536 2\r\n"), err: errMalformed},
		{name: "v1 bad dst port", in: []byte("PROXY TCP4 1.2.3.4 5.6.7.8 1 x\r\n"), err: errMalformed},
		{name: "v1 oversized", in: []byte("PROXY UNKNOWN " + strings.Repeat("a", 200) + "\r\n"), err: errMalformed},
		{name: "v1 one byte over", in: []byte("PROXY UNKNOWN " + strings.Repeat("a", v1MaxLen-len("PROXY UNKNOWN ")-1) + "\r\n"), err: errMalformed},
		{name: "v1 truncated", in: []byte("PROXY TCP4 1.2.3.4"), err: io.ErrUnexpectedEOF},

		{name: "v2 bad signature", in: append([]byte("\r\n\r\nXXXXXXXX"), 0x21, 0x11, 0, 0), err: errMalformed},
		{name: "v2 short bad signature", in: []byte("\r\nEHLO"), err: errMalformed},
		{name: "v2 truncated fixed part", in: v2Sig[:8], err: io.ErrUnexpectedEOF},
		{name: "v2 version 1", in: v2(0x11, 0x11, inet4Body("1.2.3.4", "5.6.7.8", 1, 2)), err: errMalformed},
		{name: "v2 command 2", in: v2(0x22, 0x11, inet4Body("1.2.3.4", "5.6.7.8", 1, 2)), err: errMalformed},
		{name: "v2 udp", in: v2(0x21, 0x12, inet4Body("1.2.3.4", "5.6.7.8", 1, 2)), err: errMalformed},
		{name: "v2 unix", in: v2(0x21, 0x31, make([]byte, 216)), err: errMalformed},
		{name: "v2 family 4", in: v2(0x21, 0x41, make([]byte, 12)), err: errMalformed},
		{name: "v2 inet with unspec proto", in: v2(0x21, 0x10, inet4Body("1.2.3.4", "5.6.7.8", 1, 2)), err: errMalformed},
		{name: "v2 inet too short", in: v2(0x21, 0x11, make([]byte, 11)), err: errMalformed},
		{name: "v2 inet6 too short", in: v2(0x21, 0x21, inet4Body("1.2.3.4", "5.6.7.8", 1, 2)), err: errMalformed},
		{name: "v2 oversized", in: v2(0x21, 0x11, make([]byte, v2MaxBody+1)), err: errMalformed},
		{name: "v2 length beyond data", in: v2(0x21, 0x11, inet4Body("1.2.3.4", "5.6.7.8", 1, 2))[:20], err: io.ErrUnexpectedEOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// What follows the header must be left for the protocol.
			// Error cases get their input alone, so truncation is truncation.
			const after = "\x16after"
			in := tt.in
			if tt.err == nil {
				in = append(append([]byte{}, in...), after...)
			}
			r := bufio.NewReaderSize(bytes.NewReader(in), bufSize)
			h, err := readHeader(r)
			if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Fatalf("err = %v, want %v", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if h.local != tt.local {
				t.Errorf("local = %v, want %v", h.local, tt.local)
			}
			if !tt.local && h.src.String() != tt.src {
				t.Errorf("src = %v, want %v", h.src, tt.src)
			}
			if rest, _ := io.ReadAll(r); string(rest) != after {
				t.Errorf("left %q after the header, want %q", rest, after)
			}
		})
	}
}

// FuzzReadHeader checks that no input panics, that a header never
// consumes more than the limits allow, and that an accepted address
// survives being re-encoded as v2. Without -fuzz it runs only the seeds.
func FuzzReadHeader(f *testing.F) {
	for _, s := range [][]byte{
		[]byte("PROXY TCP4 203.0.113.7 192.0.2.1 4242 465\r\n"),
		[]byte("PROXY TCP6 2001:db8::7 2001:db8::1 4242 465\r\n"),
		[]byte("PROXY UNKNOWN\r\n"),
		[]byte("PROXY TCP4 1.2.3.4 5.6.7.8 01 2\r\n"),
		[]byte("PROXY " + strings.Repeat("A", 120)),
		v2(0x21, 0x11, inet4Body("203.0.113.7", "192.0.2.1", 4242, 465)),
		v2(0x21, 0x21, inet6Body("2001:db8::7", "2001:db8::1", 4242, 465)),
		v2(0x21, 0x11, append(inet4Body("203.0.113.7", "192.0.2.1", 1, 2), 0x04, 0x00, 0x01, 'x')),
		v2(0x20, 0x00, nil),
		v2(0x21, 0x31, make([]byte, 216)),
		v2Sig,
		{0x16, 0x03, 0x01, 0x00, 0x05},
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		br := bytes.NewReader(in)
		r := bufio.NewReaderSize(br, bufSize)
		h, err := readHeader(r)
		if err != nil {
			return
		}
		used := len(in) - r.Buffered() - br.Len()
		if used > v2HeaderLen+v2MaxBody || (in[0] == 'P' && used > v1MaxLen) {
			t.Fatalf("consumed %d bytes", used)
		}
		if h.local {
			return
		}
		if !h.src.Addr().IsValid() || h.src.Addr().Is4In6() || h.src.Addr().Zone() != "" {
			t.Fatalf("src = %v", h.src)
		}
		var body []byte
		fam := byte(0x21)
		if h.src.Addr().Is4() {
			body, fam = inet4Body(h.src.Addr().String(), "192.0.2.1", h.src.Port(), 465), 0x11
		} else {
			body = inet6Body(h.src.Addr().String(), "::1", h.src.Port(), 465)
		}
		h2, err := readHeader(bufio.NewReader(bytes.NewReader(v2(0x21, fam, body))))
		if err != nil || h2 != h {
			t.Fatalf("re-encoded %v as v2: got %v, %v", h, h2, err)
		}
	})
}
