package imap

import (
	"strings"
)

// ErrorKind says what a failure means for the caller.
type ErrorKind int

const (
	// KindConnection: the connection failed, timed out or was cancelled,
	// or the server said it is temporarily unavailable. Retry later with
	// backoff.
	KindConnection ErrorKind = iota + 1
	// KindAuth: the server rejected the credentials. Retrying with the
	// same ones will not help and may get the account locked; a person
	// has to act.
	KindAuth
	// KindLimit: the server enforces a usage limit, such as a daily
	// download quota or a cap on connections. Stop and resume after the
	// limit resets, without counting it as a failure.
	KindLimit
	// KindRefused: the server answered the command with NO or BAD, or the
	// client refused to send it. The connection is still usable.
	KindRefused
	// KindProtocol: the server sent something that is not IMAP. The
	// connection has been closed.
	KindProtocol
	// KindTooLarge: a response was over a bound, typically a literal over
	// the caller's limit. The connection has been closed without reading
	// the rest.
	KindTooLarge
)

func (k ErrorKind) String() string {
	switch k {
	case KindConnection:
		return "connection"
	case KindAuth:
		return "authentication"
	case KindLimit:
		return "limit"
	case KindRefused:
		return "refused"
	case KindProtocol:
		return "protocol"
	case KindTooLarge:
		return "too large"
	}
	return "unknown"
}

// Error is a failed command or connection.
type Error struct {
	Kind ErrorKind
	// Op is the command, such as "LOGIN" or "UID FETCH", or "connect" and
	// "greeting" before the first command.
	Op string
	// Resp is the server's NO, BAD or BYE, when there was one.
	Resp *StatusResponse
	// Err is the underlying error, when there was one. A cancelled or
	// expired context is reported as the context's error.
	Err error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("imap: ")
	b.WriteString(e.Op)
	b.WriteString(": ")
	b.WriteString(e.Kind.String())
	if e.Resp != nil {
		b.WriteString(": ")
		b.WriteString(e.Resp.String())
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// classify turns a server's NO, BAD or BYE into an error. auth is true for
// LOGIN and AUTHENTICATE, where a plain NO means the credentials were
// rejected (RFC 3501 §6.2.3).
func classify(op string, s *StatusResponse, auth bool) *Error {
	e := &Error{Op: op, Resp: s}
	switch {
	case s.Code == "UNAVAILABLE" || s.Code == "INUSE":
		// RFC 5530: a temporary failure on the server's side.
		e.Kind = KindConnection
	case s.Code == "AUTHENTICATIONFAILED" || s.Code == "AUTHORIZATIONFAILED" ||
		s.Code == "EXPIRED" || s.Code == "CONTACTADMIN" || s.Code == "PRIVACYREQUIRED":
		e.Kind = KindAuth
	case isLimit(s):
		e.Kind = KindLimit
	case s.Type == "BYE":
		e.Kind = KindConnection
	case auth && s.Type == "NO":
		e.Kind = KindAuth
	default:
		e.Kind = KindRefused
	}
	return e
}

// isLimit recognizes a server enforcing a usage limit. RFC 9208's
// OVERQUOTA and RFC 5530's LIMIT are the coded forms. Providers also say
// it in free text only, as Gmail does with a NO or BYE about "bandwidth
// limits" or "too many simultaneous connections", so the text is checked
// too. A false positive costs a pause until the next round; a missed
// limit costs retries against a server that keeps refusing.
func isLimit(s *StatusResponse) bool {
	if s.Code == "OVERQUOTA" || s.Code == "LIMIT" {
		return true
	}
	if s.Type != "NO" && s.Type != "BYE" {
		return false
	}
	t := strings.ToLower(s.Text)
	return strings.Contains(t, "bandwidth") || strings.Contains(t, "limit") ||
		strings.Contains(t, "too many") || strings.Contains(t, "quota")
}
