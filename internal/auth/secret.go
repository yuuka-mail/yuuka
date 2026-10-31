package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"strings"

	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Prefixes of the bearer-style secrets. They make a leaked string
// recognizable (to secret scanners and to people) and stop one kind from
// being accepted as another.
const (
	PrefixSession    = "yks"
	PrefixToken      = "ykt"
	PrefixEnrollment = "yke"
	// PrefixSMTP is an SMTP submission password; its SMTP user name is
	// the credential id it carries.
	PrefixSMTP = "ykp"
)

// secretBytes is the entropy of every token: 256 bits.
const secretBytes = 32

// Secret is a freshly generated token. Only Hash is stored; Text is shown
// to its holder once.
type Secret struct {
	ID   string
	Text string
	Hash [32]byte
}

// NewSecret returns "<prefix>_<id>_<base64url secret>" for the record id.
// The id travels in clear so the record is found by primary key and the
// hash compared in constant time, instead of searching by hash.
func NewSecret(prefix, id string) Secret {
	b := make([]byte, secretBytes)
	_, _ = rand.Read(b) // never fails (crypto/rand since Go 1.24)
	enc := base64.RawURLEncoding.EncodeToString(b)
	return Secret{ID: id, Text: prefix + "_" + id + "_" + enc, Hash: sha256.Sum256([]byte(enc))}
}

// ParseSecret splits a token of the given prefix into its record id and
// the hash of its secret part. ok is false for anything malformed.
func ParseSecret(prefix, s string) (id string, hash [32]byte, ok bool) {
	rest, ok := strings.CutPrefix(s, prefix+"_")
	if !ok {
		return "", hash, false
	}
	id, enc, ok := strings.Cut(rest, "_")
	if !ok || !ulid.Valid(id) || base64.RawURLEncoding.DecodedLen(len(enc)) != secretBytes {
		return "", hash, false
	}
	if _, err := base64.RawURLEncoding.DecodeString(enc); err != nil {
		return "", hash, false
	}
	return id, sha256.Sum256([]byte(enc)), true
}

// HashEqual compares a stored hash with a computed one in constant time.
func HashEqual(stored []byte, h [32]byte) bool {
	return subtle.ConstantTimeCompare(stored, h[:]) == 1
}

// Recovery codes are typed by hand from paper, so they use Crockford's
// base32 (no I, L, O, U) and tolerate case, dashes and spaces. 10 random
// bytes give exactly 16 characters and 80 bits: beyond offline brute force
// of the stored SHA-256 even if the database leaks.
var crockford = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

const recoveryCodeLen = 16

// NewRecoveryCode returns a code formatted as XXXX-XXXX-XXXX-XXXX.
func NewRecoveryCode() string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	s := crockford.EncodeToString(b)
	return s[0:4] + "-" + s[4:8] + "-" + s[8:12] + "-" + s[12:16]
}

// HashRecoveryCode normalizes a typed code and hashes it. ok is false if
// it cannot be a code at all.
func HashRecoveryCode(typed string) (hash [32]byte, ok bool) {
	var b strings.Builder
	for _, r := range strings.ToUpper(typed) {
		switch r {
		case '-', ' ', '\t':
			continue
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		b.WriteRune(r)
	}
	s := b.String()
	if len(s) != recoveryCodeLen {
		return hash, false
	}
	if _, err := crockford.DecodeString(s); err != nil {
		return hash, false
	}
	return sha256.Sum256([]byte(s)), true
}
