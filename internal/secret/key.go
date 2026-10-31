package secret

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// KEKSize is the length of a KEK in bytes.
const KEKSize = 32

// KeyIDSize is the length of a kek_id in bytes.
const KeyIDSize = 8

const (
	keyIDLabel  = "yuuka kek id v1"
	wrapKeyInfo = "yuuka dek wrap v1 aes-256-gcm"
	// LocalWrapper is the Wrapper.ID of a Keyring.
	LocalWrapper = "local"
	// A KEK file is one 44-character line; anything much longer is the
	// wrong file, and reading it whole would only copy more of it around.
	maxKEKFile = 256
)

// KeyID names a KEK without revealing it: HMAC-SHA256(KEK, "yuuka kek id
// v1") truncated to 8 bytes. Its String form is a local record's key_ref.
type KeyID [KeyIDSize]byte

// String is the lowercase hex form: the key_ref, and what logs and `yuuka
// secret ls` show.
func (id KeyID) String() string { return hex.EncodeToString(id[:]) }

// parseKeyRef accepts exactly the String form, so one key has one key_ref.
func parseKeyRef(ref string) (KeyID, bool) {
	var id KeyID
	if len(ref) != 2*KeyIDSize || strings.ToLower(ref) != ref {
		return id, false
	}
	if _, err := hex.Decode(id[:], []byte(ref)); err != nil {
		return id, false
	}
	return id, true
}

// Key is one loaded KEK. It keeps only its id and the AES-GCM instance
// for the derived wrapping key; the KEK bytes themselves are wiped after
// derivation. Printing a Key shows its id and nothing else.
type Key struct {
	id    KeyID
	block cipher.Block
	aead  cipher.AEAD // random-nonce GCM: Seal draws the nonce itself
	// testGCM and testNonce, when set, replace the random nonce so tests
	// can pin exact bytes. Only this package's tests set them; production
	// keys come from newKey, which never does.
	testGCM   cipher.AEAD
	testNonce []byte
}

// ParseKey parses the contents of a KEK file: one line holding the
// standard, padded base64 encoding of exactly 32 bytes, as printed by
// `openssl rand -base64 32`. A single trailing LF or CRLF is allowed and
// nothing else is. Errors never quote the input.
func ParseKey(text []byte) (*Key, error) {
	line := bytes.TrimSuffix(text, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	want := base64.StdEncoding.EncodedLen(KEKSize)
	if len(line) != want {
		return nil, fmt.Errorf("secret: KEK must be one line of %d base64 characters encoding %d bytes (openssl rand -base64 32); got %d characters",
			want, KEKSize, len(line))
	}
	kek := make([]byte, base64.StdEncoding.DecodedLen(len(line)))
	defer clear(kek)
	// Strict rejects non-canonical padding bits, so one key has exactly one
	// spelling. It still skips CR and LF, which the length check above
	// and the decoded length below catch.
	n, err := base64.StdEncoding.Strict().Decode(kek, line)
	if err != nil || n != KEKSize {
		// The decoder's error names an offset into the key; drop it.
		return nil, fmt.Errorf("secret: KEK is not valid base64 of %d bytes", KEKSize)
	}
	return newKey(kek[:KEKSize])
}

// LoadKey reads and parses a KEK file. The path appears in errors; the
// contents never do.
func LoadKey(path string) (*Key, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("secret: read KEK: %w", err)
	}
	defer func() { _ = f.Close() }() // read-only; nothing to flush
	// One fixed buffer rather than io.ReadAll, whose regrowth would leave
	// copies of the key behind that nothing clears.
	buf := make([]byte, maxKEKFile+1)
	defer clear(buf)
	n, err := io.ReadFull(f, buf)
	switch {
	case err == nil:
		return nil, fmt.Errorf("secret: KEK file %s is longer than one line of base64", path)
	case !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF):
		return nil, fmt.Errorf("secret: read KEK %s: %w", path, err)
	}
	k, err := ParseKey(buf[:n])
	if err != nil {
		return nil, fmt.Errorf("%w (file %s)", err, path)
	}
	return k, nil
}

func newKey(kek []byte) (*Key, error) {
	if len(kek) != KEKSize {
		return nil, fmt.Errorf("secret: KEK is %d bytes, want %d", len(kek), KEKSize)
	}
	if allZero(kek) {
		// /dev/zero piped through base64 parses fine and protects nothing.
		return nil, errors.New("secret: KEK is all zero bytes")
	}
	mac := hmac.New(sha256.New, kek)
	mac.Write([]byte(keyIDLabel))
	var k Key
	copy(k.id[:], mac.Sum(nil))

	sub, err := hkdf.Key(sha256.New, kek, nil, wrapKeyInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("secret: derive wrapping key: %w", err)
	}
	defer clear(sub)
	if k.block, err = aes.NewCipher(sub); err != nil {
		return nil, fmt.Errorf("secret: aes: %w", err)
	}
	if k.aead, err = cipher.NewGCMWithRandomNonce(k.block); err != nil {
		return nil, fmt.Errorf("secret: gcm: %w", err)
	}
	return &k, nil
}

func allZero(b []byte) bool {
	var acc byte
	for _, c := range b {
		acc |= c
	}
	return acc == 0
}

// ID returns the key's kek_id.
func (k *Key) ID() KeyID { return k.id }

// wrap returns nonce || AES-256-GCM(wrapkey, dek, aad) || tag.
func (k *Key) wrap(dek, aad []byte) []byte {
	if k.testGCM != nil {
		out := append([]byte(nil), k.testNonce...)
		return k.testGCM.Seal(out, k.testNonce, dek, aad)
	}
	return k.aead.Seal(nil, nil, dek, aad)
}

func (k *Key) String() string { return "kek:" + k.id.String() }

// Format prints the id for every verb, so %#v or %x cannot reach the
// cipher state through reflection.
func (k *Key) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, k.String()) }

// Keyring is the local Wrapper: the current KEK, which wraps, and previous
// KEKs, which only unwrap.
type Keyring struct {
	current  *Key
	previous []*Key
	byID     map[KeyID]*Key
}

// NewKeyring builds a keyring. Loading the same key twice (say, the new
// KEK also listed among the previous files) is refused as a sign of a
// confused rotation.
func NewKeyring(current *Key, previous ...*Key) (*Keyring, error) {
	if current == nil {
		return nil, errors.New("secret: keyring needs a current KEK")
	}
	r := &Keyring{current: current, previous: previous, byID: map[KeyID]*Key{current.id: current}}
	for _, k := range previous {
		if _, dup := r.byID[k.id]; dup {
			return nil, fmt.Errorf("secret: KEK %s is loaded twice", k.id)
		}
		r.byID[k.id] = k
	}
	return r, nil
}

// LoadKeyring loads the current KEK file and the previous ones, which are
// kept for unwrapping only until `yuuka secret rewrap` has moved every
// row to the current key.
func LoadKeyring(currentPath string, previousPaths []string) (*Keyring, error) {
	cur, err := LoadKey(currentPath)
	if err != nil {
		return nil, err
	}
	prev := make([]*Key, 0, len(previousPaths))
	for _, p := range previousPaths {
		k, err := LoadKey(p)
		if err != nil {
			return nil, err
		}
		prev = append(prev, k)
	}
	return NewKeyring(cur, prev...)
}

// Current returns the key Wrap uses, or nil on a nil keyring.
func (r *Keyring) Current() *Key {
	if r == nil {
		return nil
	}
	return r.current
}

// Lookup returns the loaded key with the given id.
func (r *Keyring) Lookup(id KeyID) (*Key, bool) {
	if r == nil {
		return nil, false
	}
	k, ok := r.byID[id]
	return k, ok
}

func (r *Keyring) String() string {
	if r == nil {
		return "keyring(no KEK)"
	}
	var b strings.Builder
	b.WriteString("keyring(current " + r.current.String())
	for _, k := range r.previous {
		b.WriteString(", previous " + k.String())
	}
	b.WriteString(")")
	return b.String()
}

// Format prints only key ids, for every verb.
func (r *Keyring) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, r.String()) }

// ID is LocalWrapper.
func (r *Keyring) ID() string { return LocalWrapper }

// Wrap wraps dek under the current KEK; the key_ref is its kek_id in hex.
func (r *Keyring) Wrap(_ context.Context, dek, aad []byte) ([]byte, string, error) {
	if r == nil {
		return nil, "", fmt.Errorf("%w: no KEK loaded", ErrLocked)
	}
	return r.current.wrap(dek, aad), r.current.id.String(), nil
}

var _ Wrapper = (*Keyring)(nil)

// Unwrap unwraps a DEK wrapped by this or a previous KEK.
func (r *Keyring) Unwrap(_ context.Context, wrapped []byte, keyRef string, aad []byte) ([]byte, error) {
	id, ok := parseKeyRef(keyRef)
	if !ok {
		return nil, fmt.Errorf("%w: malformed key_ref", ErrTampered)
	}
	k, ok := r.Lookup(id)
	if !ok {
		return nil, fmt.Errorf("%w: kek_id %s is not loaded", ErrLocked, id)
	}
	if len(wrapped) != nonceSize+DEKSize+tagSize {
		return nil, ErrTampered
	}
	dek, err := k.aead.Open(nil, nil, wrapped, aad)
	if err != nil {
		return nil, ErrTampered
	}
	return dek, nil
}
