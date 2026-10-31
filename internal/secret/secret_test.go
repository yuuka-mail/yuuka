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
	"encoding/json"
	"errors"
	"flag"
	"os"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "fill in the derived fields of testdata/secret/v1.json")

const vectorFile = "../../testdata/secret/v1.json"

// vector is one entry of testdata/secret/v1.json. The inputs are written
// by hand; -update fills in the rest from them.
type vector struct {
	Name      string `json:"name"`
	KEK       string `json:"kek"`
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Audience  string `json:"audience"`
	Plaintext string `json:"plaintext"`
	DEK       string `json:"dek"`
	WrapNonce string `json:"wrap_nonce"`
	Nonce     string `json:"nonce"`

	Wrapper    string `json:"wrapper"`
	KeyRef     string `json:"key_ref"`
	WrapKey    string `json:"wrap_key"`
	DEKAAD     string `json:"dek_aad"`
	WrappedDEK string `json:"wrapped_dek"`
	AAD        string `json:"aad"`
	Payload    string `json:"payload"`
}

type vectorSet struct {
	Vectors []vector `json:"vectors"`
}

func loadVectors(t testing.TB) []vector {
	t.Helper()
	raw, err := os.ReadFile(vectorFile)
	if err != nil {
		t.Fatal(err)
	}
	var vs vectorSet
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	return vs.Vectors
}

func (v vector) record(t testing.TB) Record {
	return Record{
		ID: v.ID, Kind: v.Kind, Audience: v.Audience, Wrapper: v.Wrapper, KeyRef: v.KeyRef,
		WrappedDEK: unhex(t, v.WrappedDEK), Payload: unhex(t, v.Payload),
	}
}

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustKey(t testing.TB, b64 string) *Key {
	t.Helper()
	k, err := ParseKey([]byte(b64))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func mustRing(t testing.TB, cur *Key, prev ...*Key) *Keyring {
	t.Helper()
	r, err := NewKeyring(cur, prev...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mustSealer(t testing.TB, cur Wrapper, others ...Wrapper) *Sealer {
	t.Helper()
	s, err := NewSealer(cur, others...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// local is a Sealer over a keyring of the given keys, the first current.
func local(t testing.TB, cur *Key, prev ...*Key) *Sealer {
	t.Helper()
	return mustSealer(t, mustRing(t, cur, prev...))
}

// withNonce returns a copy of k that wraps with a fixed nonce.
func withNonce(t testing.TB, k *Key, nonce []byte) *Key {
	t.Helper()
	gcm, err := cipher.NewGCM(k.block)
	if err != nil {
		t.Fatal(err)
	}
	c := *k
	c.testGCM, c.testNonce = gcm, nonce
	return &c
}

func lp(fs ...[]byte) []byte {
	var b bytes.Buffer
	for _, f := range fs {
		n := len(f)
		b.Write([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
		b.Write(f)
	}
	return b.Bytes()
}

func gcmSeal(t *testing.T, key, nonce, pt, aad []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	return gcm.Seal(append([]byte(nil), nonce...), nonce, pt, aad)
}

// independent computes a vector's derived fields straight from the
// primitives, sharing no code with the package, so the vectors pin the
// format rather than whatever the package happens to do.
func independent(t *testing.T, v vector) (out vector) {
	t.Helper()
	kek, err := base64.StdEncoding.DecodeString(v.KEK)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, kek)
	mac.Write([]byte("yuuka kek id v1"))
	keyID := mac.Sum(nil)[:8]
	wrapKey, err := hkdf.Key(sha256.New, kek, nil, "yuuka dek wrap v1 aes-256-gcm", 32)
	if err != nil {
		t.Fatal(err)
	}
	dekAAD := lp([]byte("yuuka/dek"), []byte{0x01}, []byte(v.ID), []byte(v.Kind))
	aad := lp([]byte("yuuka/secret"), []byte{0x01}, []byte(v.ID), []byte(v.Kind), []byte(v.Audience))
	dek := unhex(t, v.DEK)
	out = v
	out.Wrapper = "local"
	out.KeyRef = hex.EncodeToString(keyID)
	out.WrapKey = hex.EncodeToString(wrapKey)
	out.DEKAAD = hex.EncodeToString(dekAAD)
	out.WrappedDEK = hex.EncodeToString(gcmSeal(t, wrapKey, unhex(t, v.WrapNonce), dek, dekAAD))
	out.AAD = hex.EncodeToString(aad)
	out.Payload = hex.EncodeToString(append([]byte{0x01}, gcmSeal(t, dek, unhex(t, v.Nonce), []byte(v.Plaintext), aad)...))
	return out
}

// pinnedSealer seals v with its fixed DEK and nonces.
func pinnedSealer(t testing.TB, v vector) *Sealer {
	s := local(t, withNonce(t, mustKey(t, v.KEK), unhex(t, v.WrapNonce)))
	s.testDEK, s.testNonce = unhex(t, v.DEK), unhex(t, v.Nonce)
	return s
}

// TestVectors freezes the v1 format. A change that alters these bytes is
// a new version byte, never an edit: existing rows must keep opening.
func TestVectors(t *testing.T) {
	ctx := context.Background()
	vs := loadVectors(t)
	if len(vs) == 0 {
		t.Fatal("no vectors")
	}
	for i := range vs {
		v := &vs[i]
		r, err := pinnedSealer(t, *v).Seal(ctx, v.ID, v.Kind, v.Audience, []byte(v.Plaintext))
		if err != nil {
			t.Fatal(err)
		}
		if *update {
			ind := independent(t, *v)
			v.Wrapper, v.KeyRef, v.WrapKey = r.Wrapper, r.KeyRef, ind.WrapKey
			v.DEKAAD, v.AAD = hex.EncodeToString(dekAAD(r)), hex.EncodeToString(payloadAAD(r))
			v.WrappedDEK, v.Payload = hex.EncodeToString(r.WrappedDEK), hex.EncodeToString(r.Payload)
		}
		if ind := independent(t, *v); ind != *v {
			t.Errorf("%s: primitives disagree with the vector:\n%+v\n%+v", v.Name, ind, *v)
		}
		if got := v.record(t); r.Wrapper != got.Wrapper || r.KeyRef != got.KeyRef ||
			!bytes.Equal(r.WrappedDEK, got.WrappedDEK) || !bytes.Equal(r.Payload, got.Payload) {
			t.Errorf("%s: package seals differently from the vector:\n%+v", v.Name, r)
		}
		if hex.EncodeToString(dekAAD(r)) != v.DEKAAD || hex.EncodeToString(payloadAAD(r)) != v.AAD {
			t.Errorf("%s: package AADs differ from the vector", v.Name)
		}

		// A production sealer (random DEK and nonces) opens the pinned bytes.
		got, err := local(t, mustKey(t, v.KEK)).Open(ctx, v.record(t), v.Audience)
		if err != nil {
			t.Fatalf("%s: open: %v", v.Name, err)
		}
		if string(got.Reveal()) != v.Plaintext {
			t.Errorf("%s: opened to the wrong plaintext", v.Name)
		}
	}
	if *update {
		out, err := json.MarshalIndent(vectorSet{vs}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(vectorFile, append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const (
	kekA = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	kekB = "ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8="
	kekC = "QEFCQ0RFRkdISUpLTE1OT1BRUlNUVVZXWFlaW1xdXl8="

	testID       = "01JC0000000000000000000009"
	testKind     = "smtp_password"
	testAudience = "smtps://agent%40example.com@smtp.example.com:465"
	testPlain    = "PLAINTEXT-MARKER-hunter2"
	evilAudience = "smtps://agent%40example.com@evil.example:465"
)

func sealTest(t *testing.T, s *Sealer) Record {
	t.Helper()
	r, err := s.Seal(context.Background(), testID, testKind, testAudience, []byte(testPlain))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func openTest(t *testing.T, s *Sealer, r Record) {
	t.Helper()
	v, err := s.Open(context.Background(), r, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	if string(v.Reveal()) != testPlain {
		t.Fatal("opened to a different value")
	}
}

func TestRoundTripRandom(t *testing.T) {
	k := mustKey(t, kekA)
	s := local(t, k)
	a, b := sealTest(t, s), sealTest(t, s)
	if bytes.Equal(a.Payload, b.Payload) || bytes.Equal(a.WrappedDEK, b.WrappedDEK) {
		t.Fatal("two seals of the same value share bytes; DEK or nonce is not random")
	}
	if len(a.Payload) != len(testPlain)+Overhead || a.Payload[0] != Version {
		t.Fatalf("payload is %d bytes starting %#x", len(a.Payload), a.Payload[0])
	}
	if a.Wrapper != LocalWrapper || a.KeyRef != k.ID().String() || len(a.WrappedDEK) != 60 {
		t.Fatalf("record: %s %s %d-byte wrapped DEK", a.Wrapper, a.KeyRef, len(a.WrappedDEK))
	}
	openTest(t, s, a)
	openTest(t, s, b)
}

func TestSealLimits(t *testing.T) {
	ctx := context.Background()
	s := local(t, mustKey(t, kekA))
	big := bytes.Repeat([]byte{'x'}, MaxPlaintext)
	r, err := s.Seal(ctx, testID, testKind, testAudience, big)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Payload) != MaxPayload {
		t.Fatalf("largest payload is %d bytes, want %d", len(r.Payload), MaxPayload)
	}
	if _, err := s.Open(ctx, r, testAudience); err != nil {
		t.Fatalf("largest payload does not open: %v", err)
	}

	for name, f := range map[string]func() (Record, error){
		"empty value":    func() (Record, error) { return s.Seal(ctx, testID, testKind, testAudience, nil) },
		"value too long": func() (Record, error) { return s.Seal(ctx, testID, testKind, testAudience, append(big, 'x')) },
		"empty id":       func() (Record, error) { return s.Seal(ctx, "", testKind, testAudience, []byte("x")) },
		"empty kind":     func() (Record, error) { return s.Seal(ctx, testID, "", testAudience, []byte("x")) },
		"empty audience": func() (Record, error) { return s.Seal(ctx, testID, testKind, "", []byte("x")) },
		"long audience": func() (Record, error) {
			return s.Seal(ctx, testID, testKind, strings.Repeat("a", MaxAudience+1), []byte("x"))
		},
	} {
		if _, err := f(); err == nil {
			t.Errorf("%s: sealed", name)
		}
	}
	if _, err := s.Seal(ctx, testID, testKind, strings.Repeat("a", MaxAudience), []byte("x")); err != nil {
		t.Errorf("audience of exactly %d bytes: %v", MaxAudience, err)
	}
}

// fakeWrapper is a second wrapper for tests: a keyring under another ID
// that can also rewrap in place, and can be told to misbehave.
type fakeWrapper struct {
	*Keyring
	id       string
	rewraps  int
	unwrapFn func() ([]byte, error)
}

func (f *fakeWrapper) ID() string { return f.id }

func (f *fakeWrapper) Unwrap(ctx context.Context, wrapped []byte, keyRef string, aad []byte) ([]byte, error) {
	if f.unwrapFn != nil {
		return f.unwrapFn()
	}
	return f.Keyring.Unwrap(ctx, wrapped, keyRef, aad)
}

func (f *fakeWrapper) Rewrap(ctx context.Context, wrapped []byte, keyRef string, aad []byte) ([]byte, string, error) {
	f.rewraps++
	dek, err := f.Unwrap(ctx, wrapped, keyRef, aad)
	if err != nil {
		return nil, "", err
	}
	return f.Wrap(ctx, dek, aad)
}

// TestOpenErrors walks every way Open refuses, and checks each lands on
// the error a caller acts on: lock quietly, lock and alert, or ask for
// the secret again.
func TestOpenErrors(t *testing.T) {
	a, b := mustKey(t, kekA), mustKey(t, kekB)
	s := local(t, a, b)
	good := sealTest(t, s)
	other, err := s.Seal(context.Background(), "01JC000000000000000000000A", testKind, testAudience, []byte("another"))
	if err != nil {
		t.Fatal(err)
	}

	mod := func(f func(r *Record)) Record {
		r := good
		r.WrappedDEK, r.Payload = bytes.Clone(good.WrappedDEK), bytes.Clone(good.Payload)
		f(&r)
		return r
	}
	flipP := func(i int) Record { return mod(func(r *Record) { r.Payload[i] ^= 0x01 }) }
	flipW := func(i int) Record { return mod(func(r *Record) { r.WrappedDEK[i] ^= 0x01 }) }
	shortDEK := mustSealer(t, &fakeWrapper{
		Keyring: mustRing(t, a), id: "local",
		unwrapFn: func() ([]byte, error) { return make([]byte, 31), nil },
	})

	cases := []struct {
		name     string
		s        *Sealer
		r        Record
		audience string
		want     error
	}{
		{"no sealer", nil, good, testAudience, ErrLocked},
		{"destination changed", s, good, evilAudience, ErrAudienceMismatch},
		{"destination changed, same length", s, good, strings.Replace(testAudience, "465", "587", 1), ErrAudienceMismatch},
		{"stored and recomputed audience both rewritten", s, mod(func(r *Record) { r.Audience = evilAudience }), evilAudience, ErrTampered},
		{"future version", s, mod(func(r *Record) { r.Payload[0] = 0x02 }), testAudience, ErrUnsupportedVersion},
		{"zero version", s, mod(func(r *Record) { r.Payload[0] = 0x00 }), testAudience, ErrUnsupportedVersion},
		{"kek not loaded", local(t, mustKey(t, kekC)), good, testAudience, ErrLocked},
		{"wrapper not configured", s, mod(func(r *Record) { r.Wrapper = "vault-transit" }), testAudience, ErrLocked},
		{"key_ref swapped to another loaded key", s, mod(func(r *Record) { r.KeyRef = b.ID().String() }), testAudience, ErrTampered},
		{"key_ref in upper case", s, mod(func(r *Record) { r.KeyRef = strings.ToUpper(r.KeyRef) }), testAudience, ErrTampered},
		{"key_ref not hex", s, mod(func(r *Record) { r.KeyRef = "zz" + r.KeyRef[2:] }), testAudience, ErrTampered},
		{"key_ref empty", s, mod(func(r *Record) { r.KeyRef = "" }), testAudience, ErrTampered},
		{"wrapper id empty", s, mod(func(r *Record) { r.Wrapper = "" }), testAudience, ErrTampered},
		{"wrapped DEK nonce flipped", s, flipW(0), testAudience, ErrTampered},
		{"wrapped DEK flipped", s, flipW(20), testAudience, ErrTampered},
		{"wrapped DEK tag flipped", s, flipW(59), testAudience, ErrTampered},
		{"wrapped DEK truncated", s, mod(func(r *Record) { r.WrappedDEK = r.WrappedDEK[:59] }), testAudience, ErrTampered},
		{"wrapped DEK empty", s, mod(func(r *Record) { r.WrappedDEK = nil }), testAudience, ErrTampered},
		{"wrapped DEK from another row", s, mod(func(r *Record) { r.WrappedDEK = other.WrappedDEK }), testAudience, ErrTampered},
		{"payload from another row", s, mod(func(r *Record) { r.Payload = other.Payload }), testAudience, ErrTampered},
		{"whole crypto from another row", s, mod(func(r *Record) { r.WrappedDEK, r.Payload = other.WrappedDEK, other.Payload }), testAudience, ErrTampered},
		{"id swapped", s, mod(func(r *Record) { r.ID = other.ID }), testAudience, ErrTampered},
		{"kind swapped", s, mod(func(r *Record) { r.Kind = "telegram_bot_token" }), testAudience, ErrTampered},
		{"payload nonce flipped", s, flipP(1), testAudience, ErrTampered},
		{"payload ciphertext flipped", s, flipP(1 + nonceSize), testAudience, ErrTampered},
		{"payload tag flipped", s, flipP(len(good.Payload) - 1), testAudience, ErrTampered},
		{"payload truncated", s, mod(func(r *Record) { r.Payload = r.Payload[:len(r.Payload)-1] }), testAudience, ErrTampered},
		{"payload of empty plaintext length", s, mod(func(r *Record) { r.Payload = r.Payload[:Overhead] }), testAudience, ErrTampered},
		{"version byte only", s, mod(func(r *Record) { r.Payload = r.Payload[:1] }), testAudience, ErrTampered},
		{"empty payload", s, mod(func(r *Record) { r.Payload = nil }), testAudience, ErrTampered},
		{"oversized payload", s, mod(func(r *Record) { r.Payload = append(r.Payload, make([]byte, MaxPayload)...) }), testAudience, ErrTampered},
		{"trailing byte", s, mod(func(r *Record) { r.Payload = append(r.Payload, 0) }), testAudience, ErrTampered},
		{"empty stored audience", s, mod(func(r *Record) { r.Audience = "" }), "", ErrTampered},
		{"wrapper returns a short DEK", shortDEK, good, testAudience, ErrTampered},
	}
	sentinels := []error{ErrLocked, ErrTampered, ErrAudienceMismatch, ErrUnsupportedVersion}
	for _, c := range cases {
		v, err := c.s.Open(context.Background(), c.r, c.audience)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
		for _, o := range sentinels {
			if !errors.Is(c.want, o) && errors.Is(err, o) {
				t.Errorf("%s: also matches %v", c.name, o)
			}
		}
		if v.Reveal() != nil {
			t.Errorf("%s: returned plaintext alongside an error", c.name)
		}
	}

	// A wrapper's own failure (Vault unreachable, say) passes through
	// untyped, so callers treat it as transient rather than lock or alert.
	down := errors.New("connection refused")
	flaky := mustSealer(t, &fakeWrapper{
		Keyring: mustRing(t, a), id: "local",
		unwrapFn: func() ([]byte, error) { return nil, down },
	})
	if _, err := flaky.Open(context.Background(), good, testAudience); !errors.Is(err, down) {
		t.Errorf("transient wrapper error: %v", err)
	}
}

func TestNilSealer(t *testing.T) {
	ctx := context.Background()
	var s *Sealer
	if _, err := s.Seal(ctx, testID, testKind, testAudience, []byte("x")); !errors.Is(err, ErrLocked) {
		t.Errorf("seal: %v", err)
	}
	if _, err := s.Rewrap(ctx, Record{}); !errors.Is(err, ErrLocked) {
		t.Errorf("rewrap: %v", err)
	}
	var r *Keyring
	if r.Current() != nil {
		t.Error("nil keyring has a current key")
	}
	if _, ok := r.Lookup(KeyID{}); ok {
		t.Error("nil keyring found a key")
	}
	if _, _, err := r.Wrap(ctx, make([]byte, DEKSize), nil); !errors.Is(err, ErrLocked) {
		t.Errorf("nil keyring wrap: %v", err)
	}
}

func TestNewSealer(t *testing.T) {
	ring := mustRing(t, mustKey(t, kekA))
	if _, err := NewSealer(nil); err == nil {
		t.Error("accepted no current wrapper")
	}
	if _, err := NewSealer(ring, &fakeWrapper{Keyring: ring, id: "local"}); err == nil {
		t.Error("accepted two wrappers with one id")
	}
	for _, id := range []string{"", "Local", "1local", "vault transit", strings.Repeat("a", 33)} {
		if _, err := NewSealer(&fakeWrapper{Keyring: ring, id: id}); err == nil {
			t.Errorf("accepted wrapper id %q", id)
		}
	}
}

func TestRewrap(t *testing.T) {
	ctx := context.Background()
	oldK, newK := mustKey(t, kekA), mustKey(t, kekB)
	old := sealTest(t, local(t, oldK))

	r, err := local(t, newK, oldK).Rewrap(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	if r.KeyRef != newK.ID().String() || r.Wrapper != LocalWrapper {
		t.Fatalf("rewrapped to %s/%s", r.Wrapper, r.KeyRef)
	}
	if r.ID != old.ID || r.Kind != old.Kind || r.Audience != old.Audience || !bytes.Equal(r.Payload, old.Payload) {
		t.Fatal("rewrap touched more than the wrapped DEK")
	}
	// Once rewrapped, the old key can go.
	openTest(t, local(t, newK), r)

	if _, err := local(t, newK).Rewrap(ctx, old); !errors.Is(err, ErrLocked) {
		t.Errorf("rewrap without the old key: %v", err)
	}
	bad := old
	bad.WrappedDEK = bytes.Clone(old.WrappedDEK)
	bad.WrappedDEK[30] ^= 0x80
	if _, err := local(t, newK, oldK).Rewrap(ctx, bad); !errors.Is(err, ErrTampered) {
		t.Errorf("rewrap of a tampered DEK: %v", err)
	}
	future := old
	future.Payload = append([]byte{0x02}, old.Payload[1:]...)
	if _, err := local(t, newK, oldK).Rewrap(ctx, future); !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("rewrap of an unknown version: %v", err)
	}
	// Rewrap keeps the sealed audience; a moved destination still fails.
	if _, err := local(t, newK).Open(ctx, r, evilAudience); !errors.Is(err, ErrAudienceMismatch) {
		t.Errorf("rewrapped secret followed a new destination: %v", err)
	}
}

func TestRewrapAcrossWrappers(t *testing.T) {
	ctx := context.Background()
	ringA, ringB := mustRing(t, mustKey(t, kekA)), mustRing(t, mustKey(t, kekB))
	viaLocal := sealTest(t, mustSealer(t, ringA))

	// Moving from the local wrapper to another one: unwrap, then wrap.
	remote := &fakeWrapper{Keyring: ringB, id: "remote"}
	moving := mustSealer(t, remote, ringA)
	moved, err := moving.Rewrap(ctx, viaLocal)
	if err != nil {
		t.Fatal(err)
	}
	if moved.Wrapper != "remote" || !bytes.Equal(moved.Payload, viaLocal.Payload) || remote.rewraps != 0 {
		t.Fatalf("moved to %s, payload kept %v, rewraps %d", moved.Wrapper, bytes.Equal(moved.Payload, viaLocal.Payload), remote.rewraps)
	}
	openTest(t, mustSealer(t, remote), moved)

	// Within a wrapper that can rewrap itself, the DEK never comes out.
	again, err := moving.Rewrap(ctx, moved)
	if err != nil {
		t.Fatal(err)
	}
	if remote.rewraps != 1 || bytes.Equal(again.WrappedDEK, moved.WrappedDEK) {
		t.Fatalf("rewraps %d", remote.rewraps)
	}
	openTest(t, moving, again)
	// Both wrappers stay readable during the move.
	openTest(t, moving, viaLocal)
}

// FuzzOpen feeds arbitrary records to Open. Only the vector's own bytes
// may ever open, and every refusal must be one of the typed errors.
func FuzzOpen(f *testing.F) {
	v := loadVectors(f)[0]
	s := local(f, mustKey(f, v.KEK))
	good := v.record(f)
	f.Add(good.WrappedDEK, good.Payload, good.KeyRef)
	f.Add([]byte{}, []byte{}, "")
	f.Add(good.WrappedDEK, []byte{Version}, good.KeyRef)
	f.Add(good.WrappedDEK[:59], good.Payload, good.KeyRef)
	f.Add(good.WrappedDEK, good.Payload[:Overhead], good.KeyRef)
	f.Add(good.WrappedDEK, append([]byte{0x02}, good.Payload[1:]...), good.KeyRef)
	f.Add(good.WrappedDEK, good.Payload, "0000000000000000")
	f.Fuzz(func(t *testing.T, wrapped, payload []byte, keyRef string) {
		r := good
		r.WrappedDEK, r.Payload, r.KeyRef = wrapped, payload, keyRef
		got, err := s.Open(context.Background(), r, v.Audience)
		if err == nil {
			if !bytes.Equal(wrapped, good.WrappedDEK) || !bytes.Equal(payload, good.Payload) ||
				keyRef != good.KeyRef || string(got.Reveal()) != v.Plaintext {
				t.Fatalf("opened a record that is not the vector: %x %x %q", wrapped, payload, keyRef)
			}
			return
		}
		if !errors.Is(err, ErrTampered) && !errors.Is(err, ErrUnsupportedVersion) && !errors.Is(err, ErrLocked) {
			t.Fatalf("untyped error: %v", err)
		}
	})
}
