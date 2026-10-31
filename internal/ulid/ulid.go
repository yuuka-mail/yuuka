// Package ulid generates and validates ULIDs in their canonical form:
// 26 characters of uppercase Crockford base32 encoding 128 bits
// (48-bit millisecond timestamp, 80 random bits).
package ulid

import (
	"crypto/rand"
	"encoding/binary"
	"time"
)

const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// New returns a ULID for t. Monotonicity within one millisecond is not
// needed by any caller, so none is attempted.
func New(t time.Time) string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[0:8], uint64(t.UnixMilli())<<16)
	// crypto/rand.Read never returns an error since Go 1.24.
	_, _ = rand.Read(b[6:])
	return encode(b)
}

// encode writes 128 bits as 26 base32 digits, most significant first;
// the leading digit carries only the top 3 bits.
func encode(b [16]byte) string {
	hi := binary.BigEndian.Uint64(b[0:8])
	lo := binary.BigEndian.Uint64(b[8:16])
	var out [26]byte
	for i := 25; i >= 0; i-- {
		out[i] = alphabet[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}

// Valid reports whether s is a canonical (uppercase) ULID.
func Valid(s string) bool {
	if len(s) != 26 || s[0] > '7' {
		return false
	}
	for i := range len(s) {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

func isDigit(c byte) bool {
	switch {
	case c >= '0' && c <= '9':
		return true
	case c >= 'A' && c <= 'Z':
		return c != 'I' && c != 'L' && c != 'O' && c != 'U'
	}
	return false
}
