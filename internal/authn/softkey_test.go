package authn

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
)

// softKey is a software passkey: enough of an authenticator and a browser
// to answer the JSON options of begin with the JSON finish expects.
type softKey struct {
	t       *testing.T
	origin  string
	priv    *ecdsa.PrivateKey
	id      []byte
	user    []byte // user handle learned at registration
	counter uint32
}

func newSoftKey(t *testing.T, origin string) *softKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return &softKey{t: t, origin: origin, priv: priv, id: id}
}

var b64 = base64.RawURLEncoding

func (k *softKey) clientData(typ, challenge string) []byte {
	cd, err := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": k.origin, "crossOrigin": false})
	if err != nil {
		k.t.Fatal(err)
	}
	return cd
}

func (k *softKey) authData(rpID string, flags byte, attested []byte) []byte {
	h := sha256.Sum256([]byte(rpID))
	out := append(h[:], flags)
	out = binary.BigEndian.AppendUint32(out, k.counter)
	return append(out, attested...)
}

// create answers registration options (the JSON of register/begin).
func (k *softKey) create(options []byte) []byte {
	k.t.Helper()
	var opts struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			RP        struct {
				ID string `json:"id"`
			} `json:"rp"`
			User struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(options, &opts); err != nil {
		k.t.Fatalf("creation options: %v: %s", err, options)
	}
	user, err := b64.DecodeString(opts.PublicKey.User.ID)
	if err != nil {
		k.t.Fatal(err)
	}
	k.user = user
	cose, err := webauthncbor.Marshal(webauthncose.EC2PublicKeyData{
		PublicKeyData: webauthncose.PublicKeyData{KeyType: int64(webauthncose.EllipticKey), Algorithm: int64(webauthncose.AlgES256)},
		Curve:         1, // P-256
		XCoord:        k.priv.X.FillBytes(make([]byte, 32)),
		YCoord:        k.priv.Y.FillBytes(make([]byte, 32)),
	})
	if err != nil {
		k.t.Fatal(err)
	}
	attested := make([]byte, 16) // zero AAGUID
	attested = binary.BigEndian.AppendUint16(attested, uint16(len(k.id)))
	attested = append(attested, k.id...)
	attested = append(attested, cose...)
	const up, uv, at = 0x01, 0x04, 0x40
	authData := k.authData(opts.PublicKey.RP.ID, up|uv|at, attested)
	attObj, err := webauthncbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": authData})
	if err != nil {
		k.t.Fatal(err)
	}
	out, _ := json.Marshal(map[string]any{
		"id": b64.EncodeToString(k.id), "rawId": b64.EncodeToString(k.id), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64.EncodeToString(k.clientData("webauthn.create", opts.PublicKey.Challenge)),
			"attestationObject": b64.EncodeToString(attObj),
			"transports":        []string{"internal"},
		},
		"clientExtensionResults": map[string]any{},
	})
	return out
}

// get answers assertion options (the JSON of login/begin).
func (k *softKey) get(options []byte) []byte {
	k.t.Helper()
	var opts struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			RPID      string `json:"rpId"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(options, &opts); err != nil {
		k.t.Fatalf("assertion options: %v: %s", err, options)
	}
	const up, uv = 0x01, 0x04
	authData := k.authData(opts.PublicKey.RPID, up|uv, nil)
	cd := k.clientData("webauthn.get", opts.PublicKey.Challenge)
	cdHash := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, authData...), cdHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, k.priv, digest[:])
	if err != nil {
		k.t.Fatal(err)
	}
	out, _ := json.Marshal(map[string]any{
		"id": b64.EncodeToString(k.id), "rawId": b64.EncodeToString(k.id), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64.EncodeToString(cd),
			"authenticatorData": b64.EncodeToString(authData),
			"signature":         b64.EncodeToString(sig),
			"userHandle":        b64.EncodeToString(k.user),
		},
		"clientExtensionResults": map[string]any{},
	})
	return out
}
