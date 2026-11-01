package imap

import (
	"encoding/base64"
	"errors"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// mutf7 is RFC 3501 §5.1.3's modified base64: "," in place of "/", no
// padding, and strict about the unused bits.
var mutf7 = base64.NewEncoding("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+,").
	WithPadding(base64.NoPadding).Strict()

var errMUTF7 = errors.New("imap: invalid modified UTF-7 mailbox name")

// EncodeMailbox encodes a UTF-8 mailbox name in modified UTF-7 (RFC 3501
// §5.1.3), the form IMAP4rev1 servers expect on the wire.
func EncodeMailbox(name string) (string, error) {
	if !utf8.ValidString(name) {
		return "", errors.New("imap: mailbox name is not valid UTF-8")
	}
	var b strings.Builder
	var pending []uint16
	flush := func() {
		if len(pending) == 0 {
			return
		}
		buf := make([]byte, 0, 2*len(pending))
		for _, u := range pending {
			buf = append(buf, byte(u>>8), byte(u))
		}
		b.WriteByte('&')
		b.WriteString(mutf7.EncodeToString(buf))
		b.WriteByte('-')
		pending = pending[:0]
	}
	for _, r := range name {
		if r >= 0x20 && r <= 0x7e {
			flush()
			if r == '&' {
				b.WriteString("&-")
			} else {
				b.WriteRune(r)
			}
			continue
		}
		pending = utf16.AppendRune(pending, r)
	}
	flush()
	return b.String(), nil
}

// DecodeMailbox decodes a modified UTF-7 mailbox name to UTF-8. It
// rejects what RFC 3501 forbids: 8-bit or control octets, broken base64,
// unpaired surrogates, and printable ASCII in encoded form (which would
// let two different names display the same).
func DecodeMailbox(name string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 0x20 || c > 0x7e {
			return "", errMUTF7
		}
		if c != '&' {
			b.WriteByte(c)
			continue
		}
		end := strings.IndexByte(name[i+1:], '-')
		if end < 0 {
			return "", errMUTF7
		}
		enc := name[i+1 : i+1+end]
		i += 1 + end
		if enc == "" {
			b.WriteByte('&')
			continue
		}
		raw, err := mutf7.DecodeString(enc)
		if err != nil || len(raw)%2 != 0 {
			return "", errMUTF7
		}
		units := make([]uint16, len(raw)/2)
		for j := range units {
			units[j] = uint16(raw[2*j])<<8 | uint16(raw[2*j+1])
		}
		for j := 0; j < len(units); j++ {
			u := units[j]
			var r rune
			switch {
			case utf16.IsSurrogate(rune(u)):
				if j+1 >= len(units) {
					return "", errMUTF7
				}
				r = utf16.DecodeRune(rune(u), rune(units[j+1]))
				if r == utf8.RuneError {
					return "", errMUTF7
				}
				j++
			case u >= 0x20 && u <= 0x7e:
				return "", errMUTF7
			default:
				r = rune(u)
			}
			b.WriteRune(r)
		}
	}
	return b.String(), nil
}
