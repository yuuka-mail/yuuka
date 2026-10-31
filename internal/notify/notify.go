// Package notify pushes a one-line summary of new mail to an IM channel.
package notify

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/mail"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding/htmlindex"
)

// maxField bounds From and Subject in the summary; a hostile sender can
// make either arbitrarily long.
const maxField = 200

// Summary renders "[zone] prefix@ ← From: Subject" from the raw message.
// Only the header block is read; headers are untrusted, so control
// characters are flattened and the output is plain text, never markup.
func Summary(zone, prefix string, raw []byte) string {
	from, subject := "?", ""
	if msg, err := mail.ReadMessage(bytes.NewReader(raw)); err == nil {
		from = formatFrom(msg.Header.Get("From"))
		subject = decode(msg.Header.Get("Subject"))
	}
	return fmt.Sprintf("[%s] %s@ ← %s: %s", clean(zone), clean(prefix), clean(from), clean(subject))
}

var decoder = &mime.WordDecoder{CharsetReader: charsetReader}

// charsetReader covers encoded words in legacy charsets (GB2312, GBK,
// Shift_JIS...) that mime.WordDecoder does not handle on its own.
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	enc, err := htmlindex.Get(charset)
	if err != nil {
		return nil, fmt.Errorf("unsupported charset %q: %w", charset, err)
	}
	return enc.NewDecoder().Reader(input), nil
}

func decode(s string) string {
	if d, err := decoder.DecodeHeader(s); err == nil {
		return d
	}
	return s
}

func formatFrom(s string) string {
	p := mail.AddressParser{WordDecoder: decoder}
	a, err := p.Parse(s)
	if err != nil {
		if s == "" {
			return "?"
		}
		return decode(s)
	}
	if a.Name == "" {
		return a.Address
	}
	return a.Name + " <" + a.Address + ">"
}

// clean collapses whitespace and control characters to single spaces and
// truncates to maxField runes.
func clean(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	var b strings.Builder
	n := 0
	space := false
	for _, r := range strings.TrimSpace(s) {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
			n++
		}
		space = false
		if n >= maxField {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
