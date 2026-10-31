package transport

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// Local checks that fail an attempt before any byte of the message is sent.
// Retrying the same bytes through the same exit fails the same way, so they
// count as permanent (StatusRejected).
var (
	// ErrInvalidMessage: the raw bytes are not valid SMTP data (bare CR or
	// LF, NUL, a line over 998 octets, empty).
	ErrInvalidMessage = errors.New("invalid message")
	// ErrHeaderNotAllowed: the message carries a header field the exit does
	// not let the caller set; see StripHeaders.
	ErrHeaderNotAllowed = errors.New("header field not allowed by this exit")
	// ErrInvalidEnvelope: an envelope address is empty or cannot be written
	// into a command line.
	ErrInvalidEnvelope = errors.New("invalid envelope")
	// ErrMessageTooLarge: the message exceeds the server's SIZE limit.
	ErrMessageTooLarge = errors.New("message exceeds the server's SIZE limit")
	// ErrUnsupported: the message needs an extension the server does not
	// offer (SMTPUTF8, 8BITMIME, AUTH PLAIN).
	ErrUnsupported = errors.New("server lacks a required extension")
)

// maxLine is the longest line RFC 5322 §2.1.1 and RFC 5321 §4.5.3.1.6
// allow, excluding the CRLF.
const maxLine = 998

// message is what Send needs to know about raw before talking to a server.
type message struct {
	header8bit bool // non-ASCII in the header section: needs SMTPUTF8 (RFC 6532)
	body8bit   bool // non-ASCII in the body: needs 8BITMIME (RFC 6152)
	size       int  // octets as the server counts them (RFC 1870)
}

// inspect validates raw as SMTP data.
//
// The transport validates rather than repairs. net/textproto's DotWriter
// would silently turn a bare LF into CRLF, so the exit would receive bytes
// that differ from the ones the outbox stored as the Sent copy; and a bare
// CR or a 999-octet line is either rejected or rewritten by some server
// further along. All three are composer bugs, and a failure at the first
// hop is the cheapest place to see them.
func inspect(raw []byte) (message, error) {
	if len(raw) == 0 {
		return message{}, fmt.Errorf("%w: empty", ErrInvalidMessage)
	}
	var m message
	inHeader := true
	for n, rest := 1, raw; len(rest) > 0; n++ {
		line := rest
		end := bytes.IndexByte(rest, '\n')
		if end >= 0 {
			line, rest = rest[:end], rest[end+1:]
			if len(line) == 0 || line[len(line)-1] != '\r' {
				return message{}, fmt.Errorf("%w: line %d: bare LF", ErrInvalidMessage, n)
			}
			line = line[:len(line)-1]
		} else {
			// The last line may lack its CRLF: the CRLF that starts the
			// end-of-data sequence terminates it (RFC 5321 §4.1.1.4).
			rest = nil
		}
		if len(line) > maxLine {
			return message{}, fmt.Errorf("%w: line %d: %d octets, limit %d", ErrInvalidMessage, n, len(line), maxLine)
		}
		for _, c := range line {
			switch {
			case c == '\r':
				return message{}, fmt.Errorf("%w: line %d: bare CR", ErrInvalidMessage, n)
			case c == 0:
				return message{}, fmt.Errorf("%w: line %d: NUL", ErrInvalidMessage, n)
			case c >= 0x80 && inHeader:
				m.header8bit = true
			case c >= 0x80:
				m.body8bit = true
			}
		}
		if len(line) == 0 {
			inHeader = false
		}
	}
	m.size = len(raw)
	if !bytes.HasSuffix(raw, []byte("\r\n")) {
		m.size += 2
	}
	return m, nil
}

// field is one header field of raw: its name and the span of all its lines.
type field struct {
	name       string
	start, end int
}

// headerFields splits the header section of raw into fields. It accepts
// CRLF and bare LF alike, so StripHeaders works on input that has not been
// validated yet.
func headerFields(raw []byte) []field {
	var fs []field
	for off := 0; off < len(raw); {
		end := bytes.IndexByte(raw[off:], '\n')
		if end < 0 {
			end = len(raw)
		} else {
			end += off + 1
		}
		line := raw[off:end]
		if len(bytes.TrimRight(line, "\r\n")) == 0 {
			break // the empty line that ends the header section
		}
		if (line[0] == ' ' || line[0] == '\t') && len(fs) > 0 {
			fs[len(fs)-1].end = end // folded continuation
		} else {
			name, _, _ := bytes.Cut(line, []byte(":"))
			fs = append(fs, field{name: string(bytes.TrimRight(name, " \t")), start: off, end: end})
		}
		off = end
	}
	return fs
}

// notAllowed lists the header fields an exit with these caps does not let
// the caller set. Return-Path is never the sender's to write: the final
// delivery MTA adds it from the envelope (RFC 5321 §4.4).
func (c Caps) notAllowed() []string {
	names := []string{"Return-Path"}
	if !c.CallerMessageID {
		names = append(names, "Message-ID")
	}
	if !c.CallerDKIM {
		names = append(names, "DKIM-Signature")
	}
	return names
}

func disallowed(f field, names []string) bool {
	for _, n := range names {
		if strings.EqualFold(f.name, n) {
			return true
		}
	}
	return false
}

// StripHeaders returns raw without the header fields an exit with caps does
// not let the caller set: Return-Path always, Message-ID unless
// CallerMessageID, DKIM-Signature unless CallerDKIM. Folded continuation
// lines go with their field; the body and every other byte are untouched.
//
// It lives here rather than in the composer because the rule is the
// exit's, and raw MIME also arrives from the submission listener, which
// composes nothing. The caller applies it before storing the bytes it
// will send, so the stored copy is what the exit receives; Send itself only
// checks, and refuses a message that still carries such a field.
func StripHeaders(raw []byte, caps Caps) []byte {
	names := caps.notAllowed()
	fs := headerFields(raw)
	out := make([]byte, 0, len(raw))
	prev := 0
	for _, f := range fs {
		if disallowed(f, names) {
			out = append(out, raw[prev:f.start]...)
			prev = f.end
		}
	}
	return append(out, raw[prev:]...)
}

// checkHeaders reports the first header field caps do not allow.
func checkHeaders(raw []byte, caps Caps) error {
	names := caps.notAllowed()
	for _, f := range headerFields(raw) {
		if disallowed(f, names) {
			return fmt.Errorf("%w: %s", ErrHeaderNotAllowed, f.name)
		}
	}
	return nil
}

// checkEnvelope rejects addresses that cannot be written into a MAIL or
// RCPT command. It does not judge address syntax beyond that; the server
// does, per recipient.
func checkEnvelope(env Envelope) error {
	if len(env.To) == 0 {
		return fmt.Errorf("%w: no recipients", ErrInvalidEnvelope)
	}
	if err := checkPath(env.From); err != nil {
		return fmt.Errorf("%w: from %q: %w", ErrInvalidEnvelope, env.From, err)
	}
	for _, to := range env.To {
		if to == "" {
			return fmt.Errorf("%w: empty recipient", ErrInvalidEnvelope)
		}
		if err := checkPath(to); err != nil {
			return fmt.Errorf("%w: recipient %q: %w", ErrInvalidEnvelope, to, err)
		}
	}
	return nil
}

func checkPath(addr string) error {
	if i := strings.IndexAny(addr, "\x00\r\n<>"); i >= 0 {
		return fmt.Errorf("contains %q", addr[i])
	}
	return nil
}

// needsUTF8 reports whether the envelope needs SMTPUTF8 (RFC 6531).
func (env Envelope) needsUTF8() bool {
	if !isASCII(env.From) {
		return true
	}
	for _, to := range env.To {
		if !isASCII(to) {
			return true
		}
	}
	return false
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
