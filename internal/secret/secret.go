// Package secret seals third-party credentials (SMTP passwords, bot tokens,
// later IMAP passwords and OAuth tokens) for storage in Postgres, and opens
// them again only for the destination they were sealed for.
//
// # Envelope
//
// Every secret gets its own random 32-byte data key (DEK). The DEK seals
// the plaintext locally; a Wrapper seals the DEK. Today the only wrapper
// is the local one (Keyring: a KEK in a file on the host), but the record
// is shaped so that HashiCorp Vault Transit or a cloud KMS can take over
// wrapping without a format change: they only ever see a 32-byte DEK and
// its AAD, never the secret or its audience. Rotating the wrapping key
// rewraps DEKs and leaves payloads alone.
//
// A Record is persisted as columns beside id, kind and audience:
//
//	wrapper     text   which Wrapper wrapped the DEK ("local")
//	key_ref     text   which of its keys (local: hex kek_id)
//	wrapped_dek bytea  the wrapper's opaque output
//	payload     bytea  0x01 || nonce (12, random) || ciphertext || tag (16)
//
// with
//
//	payload = AES-256-GCM(DEK, plaintext, AAD = field("yuuka/secret") ||
//	          field(0x01) || field(id) || field(kind) || field(audience))
//	dek AAD = field("yuuka/dek") || field(0x01) || field(id) || field(kind)
//	field(x) = uint32 big-endian len(x) || x
//
// The leading 0x01 is the version of this whole layout and is bound into
// both AADs. The DEK AAD ties a wrapped DEK to its row, so it cannot be
// moved onto another secret; it leaves out the audience so that a wrapper,
// which may be a remote service that logs its context, never learns where
// the secret goes. The vectors in testdata/secret/ pin every byte.
//
// The local wrapper derives from the KEK
//
//	kek_id  = HMAC-SHA256(KEK, "yuuka kek id v1")[:8]           (key_ref = hex)
//	wrapkey = HKDF-SHA256(KEK, salt = none, info = "yuuka dek wrap v1 aes-256-gcm", 32)
//
// and stores wrapped_dek = nonce (12, random) || AES-256-GCM(wrapkey, DEK,
// dek AAD) || tag (16). Random nonces are fine at this volume: a decade of
// a handful of accounts is ~10^6 wraps per key against a 2^32 limit. Each
// DEK seals exactly one payload, so its nonce never repeats at all.
//
// # Other wrappers (not implemented)
//
// Vault Transit: Wrap is POST transit/encrypt/<key> with plaintext =
// base64(DEK) and associated_data = base64(dek AAD) on an AES-GCM key (or,
// on a derived key, context = dek AAD); wrapped_dek is the returned
// "vault:vN:..." string, key_ref the mount and key name. Unwrap is
// transit/decrypt with the same fields, and transit/rewrap is Rewrapper:
// it moves a DEK to the latest key version without Vault ever returning it.
//
// AWS KMS: Wrap is Encrypt(KeyId, Plaintext = DEK, EncryptionContext =
// {"yuuka-aad": base64(dek AAD)}); wrapped_dek is the CiphertextBlob and
// key_ref the key ARN. Unwrap is Decrypt with KeyId and the same context,
// Rewrapper is ReEncrypt. GenerateDataKey would save drawing the DEK
// locally but is not needed: wrapping a locally drawn DEK is equivalent.
// GCP Cloud KMS: Encrypt and Decrypt with additionalAuthenticatedData =
// dek AAD and key_ref the CryptoKey name; it has no re-encrypt call, so
// Sealer.Rewrap falls back to unwrap-and-wrap.
//
// Whatever the backend, Unwrap reports a key that is gone, disabled or not
// permitted as ErrLocked and a ciphertext that does not authenticate as
// ErrTampered; anything else (a network error) is transient.
//
// # Threat model
//
// The local KEK lives on the host, never in the database or its backups,
// so a leaked dump yields ciphertext plus metadata (hosts, usernames, chat
// ids). If the KEK leaks together with a dump, what must be rotated is the
// third-party credentials themselves, not just the KEK. With Vault or KMS
// the key never reaches the host, but the host can still ask for unwraps:
// that buys audit and revocation, not protection from a compromised host.
//
// The audience is who the plaintext may be handed to: smtps://user@host:
// port for a transport, telegram:bot<id> for a bot, and so on (see
// audience.go). It is sealed into the payload AAD, and Open additionally
// requires the caller to recompute it from the row that owns the secret.
// Someone who can write the database but has no key could otherwise point
// transports.host at their own server and have yuuka hand over the
// password in AUTH PLAIN; AAD alone does not stop that, because the
// attacker would leave secrets.audience untouched and only change the
// owning row. With the recomputed audience the open fails closed, so any
// change of destination needs a fresh seal, and only an authorised write
// path can seal. This check is always local, whatever the wrapper.
//
// Not covered: an attacker with the KEK (or the right to unwrap), or with
// code execution in the running process, gets plaintext. See Value for how
// far in-memory hygiene goes in Go.
package secret

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"regexp"
)

// Version is the record format byte that starts every sealed payload.
const Version byte = 0x01

const (
	// DEKSize is the length of a data key.
	DEKSize   = 32
	nonceSize = 12
	tagSize   = 16
	// Overhead is how much longer a sealed payload is than its plaintext.
	Overhead = 1 + nonceSize + tagSize
	// MaxPayload is the largest payload Seal produces or Open accepts.
	MaxPayload = 65536
	// MaxPlaintext is the largest plaintext Seal accepts.
	MaxPlaintext = MaxPayload - Overhead
	// MaxAudience bounds the audience column.
	MaxAudience = 512
	// MaxKeyRef and MaxWrappedDEK bound what a wrapper may return, with
	// room for KMS key ARNs and ciphertext blobs.
	MaxKeyRef     = 512
	MaxWrappedDEK = 4096
)

var (
	// ErrLocked means the key a secret needs is not available: no key is
	// configured at all, the record's wrapper is not configured, or its
	// key_ref is not loaded (an old KEK file dropped before every row was
	// rewrapped). Lock this secret and say why; nothing is wrong with it.
	ErrLocked = errors.New("secret: locked: the key it was sealed under is not available")
	// ErrTampered means the record does not authenticate: the wrapped DEK
	// or the payload was altered or corrupted, or moved from another row.
	// Lock it and raise a security alert.
	ErrTampered = errors.New("secret: sealed value failed authentication (tampered or corrupted)")
	// ErrAudienceMismatch means the destination recomputed from the owning
	// row is not the one the secret was sealed for. Nothing was decrypted;
	// the secret has to be set again for the new destination.
	ErrAudienceMismatch = errors.New("secret: destination changed since the secret was sealed")
	// ErrUnsupportedVersion means the payload starts with a format byte
	// this build does not know, e.g. written by a newer yuuka.
	ErrUnsupportedVersion = errors.New("secret: unsupported sealed format version")
)

// Record is one secrets row: the columns this package reads and writes.
type Record struct {
	ID         string // secrets.id
	Kind       string // secrets.kind
	Audience   string // secrets.audience: the destination it was sealed for
	Wrapper    string // secrets.wrapper: Wrapper.ID() of what wrapped the DEK
	KeyRef     string // secrets.key_ref: which of the wrapper's keys
	WrappedDEK []byte // secrets.wrapped_dek
	Payload    []byte // secrets.payload: Version || nonce || ciphertext || tag
}

// Wrapper wraps and unwraps data keys. It sees only the DEK and the DEK
// AAD, never a secret or its audience.
type Wrapper interface {
	// ID names the wrapper in records ("local"). It is frozen once a
	// record carries it; a new wrapped format gets a new ID.
	ID() string
	// Wrap encrypts dek under the wrapper's current key, bound to aad, and
	// returns the opaque result and a reference to the key used.
	Wrap(ctx context.Context, dek, aad []byte) (wrapped []byte, keyRef string, err error)
	// Unwrap reverses Wrap. A key it does not have must yield an error
	// matching ErrLocked; a wrapped value that does not authenticate, one
	// matching ErrTampered. Other errors are taken as transient.
	Unwrap(ctx context.Context, wrapped []byte, keyRef string, aad []byte) ([]byte, error)
}

// Rewrapper is a Wrapper that can move a wrapped DEK to its current key
// without handing the DEK out (Vault transit/rewrap, AWS ReEncrypt).
type Rewrapper interface {
	Wrapper
	Rewrap(ctx context.Context, wrapped []byte, keyRef string, aad []byte) (rewrapped []byte, newKeyRef string, err error)
}

var wrapperID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// Sealer seals with its current wrapper and opens records made by any of
// its wrappers, so a move from one wrapper to another can run while old
// rows are still being rewrapped. A nil *Sealer is the "no key configured"
// state: every operation fails with ErrLocked.
type Sealer struct {
	current  Wrapper
	wrappers map[string]Wrapper

	// testDEK and testNonce, when set, replace the random DEK and payload
	// nonce so tests can pin exact bytes. Only this package's tests set
	// them; NewSealer never does.
	testDEK, testNonce []byte
}

// NewSealer builds a Sealer that wraps with current and can also unwrap
// with others.
func NewSealer(current Wrapper, others ...Wrapper) (*Sealer, error) {
	if current == nil {
		return nil, errors.New("secret: sealer needs a current wrapper")
	}
	s := &Sealer{current: current, wrappers: map[string]Wrapper{}}
	for _, w := range append([]Wrapper{current}, others...) {
		id := w.ID()
		if !wrapperID.MatchString(id) {
			return nil, fmt.Errorf("secret: bad wrapper id %q", id)
		}
		if _, dup := s.wrappers[id]; dup {
			return nil, fmt.Errorf("secret: wrapper %q configured twice", id)
		}
		s.wrappers[id] = w
	}
	return s, nil
}

// Seal encrypts plaintext under a fresh DEK bound to id, kind and audience,
// and wraps the DEK with the current wrapper. It does not retain
// plaintext; zeroing it is the caller's job.
func (s *Sealer) Seal(ctx context.Context, id, kind, audience string, plaintext []byte) (Record, error) {
	if s == nil {
		return Record{}, fmt.Errorf("%w: no key configured", ErrLocked)
	}
	if err := checkBinding(id, kind, audience); err != nil {
		return Record{}, err
	}
	switch {
	case len(plaintext) == 0:
		return Record{}, errors.New("secret: refusing to seal an empty value")
	case len(plaintext) > MaxPlaintext:
		return Record{}, fmt.Errorf("secret: value is %d bytes, more than the %d allowed", len(plaintext), MaxPlaintext)
	}
	dek := make([]byte, DEKSize)
	defer clear(dek)
	if s.testDEK != nil {
		copy(dek, s.testDEK)
	} else if _, err := rand.Read(dek); err != nil {
		return Record{}, fmt.Errorf("secret: draw DEK: %w", err)
	}

	r := Record{ID: id, Kind: kind, Audience: audience, Wrapper: s.current.ID()}
	var err error
	r.WrappedDEK, r.KeyRef, err = s.current.Wrap(ctx, dek, dekAAD(r))
	if err != nil {
		return Record{}, fmt.Errorf("secret: wrap DEK with %s: %w", r.Wrapper, err)
	}
	if err := checkWrapped(r); err != nil {
		return Record{}, fmt.Errorf("secret: wrapper %s returned a bad record: %w", r.Wrapper, err)
	}

	gcm, err := payloadGCM(dek, s.testNonce != nil)
	if err != nil {
		return Record{}, err
	}
	out := make([]byte, 1, Overhead+len(plaintext))
	out[0] = Version
	if s.testNonce != nil {
		out = append(out, s.testNonce...)
		r.Payload = gcm.Seal(out, s.testNonce, plaintext, payloadAAD(r))
	} else {
		r.Payload = gcm.Seal(out, nil, plaintext, payloadAAD(r))
	}
	return r, nil
}

// Open decrypts r for the destination audience, which the caller must
// recompute from the row that owns the secret (the transport's user, host
// and port; the channel's bot id; ...) and must never copy from
// r.Audience: that column is exactly what a database writer controls.
// (`yuuka secret check`, which hands the plaintext to no one, is the one
// caller that may pass r.Audience.) A changed destination yields
// ErrAudienceMismatch without decrypting. The other failures match
// ErrLocked, ErrUnsupportedVersion or ErrTampered, or are a wrapper's
// transient error; none of their messages carry key or plaintext bytes.
func (s *Sealer) Open(ctx context.Context, r Record, audience string) (Value, error) {
	if s == nil {
		return Value{}, fmt.Errorf("%w: no key configured", ErrLocked)
	}
	if subtle.ConstantTimeCompare([]byte(r.Audience), []byte(audience)) != 1 {
		return Value{}, ErrAudienceMismatch
	}
	if err := checkRecord(r); err != nil {
		return Value{}, err
	}
	dek, err := s.unwrap(ctx, r)
	if err != nil {
		return Value{}, err
	}
	defer clear(dek)
	gcm, err := payloadGCM(dek, false)
	if err != nil {
		return Value{}, err
	}
	pt, err := gcm.Open(nil, nil, r.Payload[1:], payloadAAD(r))
	if err != nil {
		// The AEAD error says nothing useful and nothing secret; replace
		// it so callers have one thing to match.
		return Value{}, ErrTampered
	}
	return NewValue(pt), nil
}

// Rewrap moves r's DEK to the current wrapper and key, for `yuuka secret
// rewrap`. The payload is not touched. A record already under the current
// wrapper and key is simply rewrapped again; callers that want to skip it
// compare Wrapper and KeyRef first. No plaintext is produced, and the
// destination is not checked: the result is bound to the same audience
// as r, so it is no more usable for another destination than r was.
func (s *Sealer) Rewrap(ctx context.Context, r Record) (Record, error) {
	if s == nil {
		return Record{}, fmt.Errorf("%w: no key configured", ErrLocked)
	}
	if err := checkRecord(r); err != nil {
		return Record{}, err
	}
	out := r
	out.Wrapper = s.current.ID()
	var err error
	if rw, ok := s.current.(Rewrapper); ok && r.Wrapper == out.Wrapper {
		out.WrappedDEK, out.KeyRef, err = rw.Rewrap(ctx, r.WrappedDEK, r.KeyRef, dekAAD(r))
		if err != nil {
			return Record{}, fmt.Errorf("secret: rewrap DEK with %s: %w", out.Wrapper, err)
		}
	} else {
		dek, err := s.unwrap(ctx, r)
		if err != nil {
			return Record{}, err
		}
		defer clear(dek)
		out.WrappedDEK, out.KeyRef, err = s.current.Wrap(ctx, dek, dekAAD(r))
		if err != nil {
			return Record{}, fmt.Errorf("secret: wrap DEK with %s: %w", out.Wrapper, err)
		}
	}
	if err := checkWrapped(out); err != nil {
		return Record{}, fmt.Errorf("secret: wrapper %s returned a bad record: %w", out.Wrapper, err)
	}
	return out, nil
}

func (s *Sealer) String() string {
	if s == nil {
		return "sealer(no key)"
	}
	return "sealer(wraps with " + s.current.ID() + ")"
}

// Format prints only the wrapper id, for every verb.
func (s *Sealer) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, s.String()) }

func (s *Sealer) unwrap(ctx context.Context, r Record) ([]byte, error) {
	w, ok := s.wrappers[r.Wrapper]
	if !ok {
		return nil, fmt.Errorf("%w: wrapper %q is not configured", ErrLocked, r.Wrapper)
	}
	dek, err := w.Unwrap(ctx, r.WrappedDEK, r.KeyRef, dekAAD(r))
	if err != nil {
		return nil, err
	}
	if len(dek) != DEKSize {
		clear(dek)
		return nil, ErrTampered
	}
	return dek, nil
}

// payloadGCM returns AES-256-GCM under dek: with random nonces, or with
// caller-supplied ones for the test seam.
func payloadGCM(dek []byte, fixedNonce bool) (cipher.AEAD, error) {
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("secret: aes: %w", err)
	}
	if fixedNonce {
		return cipher.NewGCM(block)
	}
	return cipher.NewGCMWithRandomNonce(block)
}

// checkRecord validates the parts of a record that Open and Rewrap rely
// on before any key is involved.
func checkRecord(r Record) error {
	if len(r.Payload) == 0 {
		return ErrTampered
	}
	if r.Payload[0] != Version {
		return fmt.Errorf("%w: 0x%02x", ErrUnsupportedVersion, r.Payload[0])
	}
	if len(r.Payload) <= Overhead || len(r.Payload) > MaxPayload ||
		checkBinding(r.ID, r.Kind, r.Audience) != nil || checkWrapped(r) != nil {
		return ErrTampered
	}
	return nil
}

func checkBinding(id, kind, audience string) error {
	switch {
	case id == "":
		return errors.New("secret: empty secret id")
	case kind == "":
		return errors.New("secret: empty secret kind")
	case audience == "":
		return errors.New("secret: empty audience")
	case len(audience) > MaxAudience:
		return fmt.Errorf("secret: audience is %d bytes, more than the %d allowed", len(audience), MaxAudience)
	}
	return nil
}

func checkWrapped(r Record) error {
	switch {
	case !wrapperID.MatchString(r.Wrapper):
		return errors.New("bad wrapper id")
	case r.KeyRef == "" || len(r.KeyRef) > MaxKeyRef:
		return fmt.Errorf("key_ref is %d bytes, want 1 to %d", len(r.KeyRef), MaxKeyRef)
	case len(r.WrappedDEK) == 0 || len(r.WrappedDEK) > MaxWrappedDEK:
		return fmt.Errorf("wrapped DEK is %d bytes, want 1 to %d", len(r.WrappedDEK), MaxWrappedDEK)
	}
	return nil
}

// payloadAAD binds the payload to its row and destination.
func payloadAAD(r Record) []byte {
	return fields([]byte("yuuka/secret"), []byte{Version}, []byte(r.ID), []byte(r.Kind), []byte(r.Audience))
}

// dekAAD binds a wrapped DEK to its row.
func dekAAD(r Record) []byte {
	return fields([]byte("yuuka/dek"), []byte{Version}, []byte(r.ID), []byte(r.Kind))
}

// fields length-prefixes each field so no two lists share an encoding.
func fields(fs ...[]byte) []byte {
	n := 0
	for _, f := range fs {
		n += 4 + len(f)
	}
	out := make([]byte, 0, n)
	for _, f := range fs {
		out = binary.BigEndian.AppendUint32(out, uint32(len(f)))
		out = append(out, f...)
	}
	return out
}
