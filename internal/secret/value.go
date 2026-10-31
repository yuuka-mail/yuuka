package secret

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
)

const redacted = "[redacted]"

var errMarshal = errors.New("secret: a secret value cannot be serialised")

// Value is a plaintext secret in process memory. It prints as [redacted]
// through fmt (every verb), slog and its String/GoString methods, and
// refuses JSON and text marshalling with an error. Reveal is the one way to
// the bytes, meant for the single place that hands them to a transport.
//
// The bytes sit two pointer hops away. When fmt prints a struct that
// holds a Value in an unexported field it cannot call Format, so it walks
// the fields itself: nested pointers come out as addresses, but on a verb
// that does not fit (%s, %d) it dereferences one pointer to a struct or
// slice and prints what is there. With two hops, every path ends at an
// address. TestNothingLeaks covers these cases.
//
// Close zeroes the bytes, and copies of a Value share them, so Close ends
// every copy. That is hygiene, not a guarantee. Copies made elsewhere are
// out of reach: whatever buffers held the plaintext before NewValue, bufio
// and TLS record buffers on the way to a server, and any string built from
// Reveal (strings cannot be cleared at all). The AES key schedule inside a
// Key cannot be wiped either, and memory can reach swap or a core dump.
// Anyone who can read the process's memory can read its secrets.
type Value struct{ h *holder }

type holder struct{ b *[]byte }

// NewValue wraps b, taking ownership: the caller must not keep using b,
// and Close will zero it.
func NewValue(b []byte) Value { return Value{&holder{&b}} }

// Reveal returns the plaintext, or nil after Close. The slice aliases the
// Value; do not retain it past Close or turn it into a string you keep.
func (v Value) Reveal() []byte {
	if v.h == nil {
		return nil
	}
	return *v.h.b
}

// Close zeroes the plaintext. It is safe to call more than once.
func (v Value) Close() {
	if v.h == nil {
		return
	}
	clear(*v.h.b)
	*v.h.b = nil
}

func (Value) String() string   { return redacted }
func (Value) GoString() string { return "secret.Value(" + redacted + ")" }

// Format prints [redacted] for every verb, including %x and %#v.
func (Value) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// LogValue redacts the value in slog output.
func (Value) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON refuses, so a Value in a response or audit row is a loud
// error rather than a redacted-looking string someone learns to ignore.
func (Value) MarshalJSON() ([]byte, error) { return nil, errMarshal }

// MarshalText refuses, for the same reason as MarshalJSON.
func (Value) MarshalText() ([]byte, error) { return nil, errMarshal }
