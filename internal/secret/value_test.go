package secret

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"testing"
)

func TestValueClose(t *testing.T) {
	buf := []byte(testPlain)
	v := NewValue(buf)
	cp := v
	if string(cp.Reveal()) != testPlain {
		t.Fatal("copy does not reveal the value")
	}
	v.Close()
	if cp.Reveal() != nil {
		t.Error("a copy still reveals after Close")
	}
	if !bytes.Equal(buf, make([]byte, len(buf))) {
		t.Error("Close left plaintext in the buffer")
	}
	v.Close() // twice is fine
	var zero Value
	zero.Close()
	if zero.Reveal() != nil {
		t.Error("zero Value reveals something")
	}
}

func TestValueRefusesMarshal(t *testing.T) {
	v := NewValue([]byte(testPlain))
	type row struct {
		Password Value `json:"password" xml:"password"`
	}
	for name, f := range map[string]func() ([]byte, error){
		"json":           func() ([]byte, error) { return json.Marshal(v) },
		"json in struct": func() ([]byte, error) { return json.Marshal(row{v}) },
		"json map key":   func() ([]byte, error) { return json.Marshal(map[Value]int{v: 1}) },
		"text":           func() ([]byte, error) { return v.MarshalText() },
		"xml":            func() ([]byte, error) { return xml.Marshal(row{v}) },
	} {
		out, err := f()
		if err == nil {
			t.Errorf("%s: marshalled to %q", name, out)
		}
		if bytes.Contains(out, []byte(testPlain)) || err != nil && strings.Contains(err.Error(), testPlain) {
			t.Errorf("%s: leaked", name)
		}
	}
}

// TestNothingLeaks pushes plaintext and key material through every error
// path and every way a value commonly gets printed or logged, and checks
// none of it comes out the other side.
func TestNothingLeaks(t *testing.T) {
	ctx := context.Background()
	kek, _ := base64.StdEncoding.DecodeString(kekA)
	vec := loadVectors(t)[0]
	if vec.KEK != kekA {
		t.Fatal("vector 0 is expected to use kekA")
	}
	dek := unhex(t, vec.DEK)
	wrapKey := unhex(t, vec.WrapKey)
	a, b := mustKey(t, kekA), mustKey(t, kekB)
	ring := mustRing(t, a, b)
	// The pinned sealer's DEK is known, so the test can look for it.
	s := pinnedSealer(t, vec)
	good, err := s.Seal(ctx, testID, testKind, testAudience, []byte(testPlain))
	if err != nil {
		t.Fatal(err)
	}
	open := local(t, a, b)

	var out strings.Builder
	errs := []error{}
	collect := func(_ any, err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	// Every refusal, from parsing the KEK to opening a secret.
	collect(ParseKey([]byte(kekA[:43] + "\n")))
	collect(ParseKey([]byte(kekA + " ")))
	collect(ParseKey([]byte(kekA[:42] + "9=")))
	collect(ParseKey(append(kek, '\n')))
	collect(ParseKey([]byte(hex.EncodeToString(kek))))
	collect(s.Seal(ctx, "", testKind, testAudience, []byte(testPlain)))
	collect(s.Seal(ctx, testID, testKind, strings.Repeat("a", MaxAudience+1), []byte(testPlain)))
	collect(s.Seal(ctx, testID, testKind, testAudience, bytes.Repeat([]byte(testPlain), MaxPlaintext)))
	collect(open.Open(ctx, good, evilAudience))
	collect(local(t, mustKey(t, kekC)).Open(ctx, good, testAudience))
	tampered := good
	tampered.Payload = bytes.Clone(good.Payload)
	tampered.Payload[len(tampered.Payload)-1] ^= 1
	collect(open.Open(ctx, tampered, testAudience))
	badDEK := good
	badDEK.WrappedDEK = bytes.Clone(good.WrappedDEK)
	badDEK.WrappedDEK[40] ^= 1
	collect(open.Open(ctx, badDEK, testAudience))
	collect(open.Rewrap(ctx, badDEK))
	collect(local(t, mustKey(t, kekC)).Rewrap(ctx, good))
	future := good
	future.Payload = append([]byte{0x7f}, good.Payload[1:]...)
	collect(open.Open(ctx, future, testAudience))
	collect((*Sealer)(nil).Open(ctx, good, testAudience))
	collect(OAuth("https://user:"+testPlain+"@auth.example.com/token", "client"))
	collect(OAuth("https://auth.example.com/token#"+testPlain, "client"))
	collect(OAuth("ht tp://"+testPlain, "client"))
	if len(errs) < 19 {
		t.Fatalf("only %d error paths exercised", len(errs))
	}
	for _, err := range errs {
		fmt.Fprintf(&out, "%v\n%+v\n%#v\n", err, err, err)
	}

	// A Value, and the key material holders, printed every way.
	v, err := open.Open(ctx, good, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	r := ring
	type exported struct{ Password Value }
	type unexported struct{ password Value }
	type withKeys struct {
		Ring *Keyring
		Key  *Key
		key  *Key
	}
	things := []any{
		v, &v,
		exported{v},
		&exported{v},
		unexported{v},
		&unexported{v},
		[]Value{v},
		map[string]Value{"pw": v},
		a, r,
		withKeys{r, a, b},
		&withKeys{r, a, b},
		s, open,
		struct{ s *Sealer }{s},
		good,
	}
	for _, x := range things {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T"} {
			fmt.Fprintf(&out, verb+"\n", x)
		}
		fmt.Fprintln(&out, x)
	}
	fmt.Fprintln(&out, v.String(), v.GoString(), a.String(), r.String())

	// slog with both stock handlers, and the log package.
	for _, h := range []slog.Handler{
		slog.NewTextHandler(&out, &slog.HandlerOptions{AddSource: true}),
		slog.NewJSONHandler(&out, nil),
	} {
		l := slog.New(h)
		l.Info("value", "pw", v, "ptr", &v, "struct", exported{v}, "hidden", unexported{v})
		l.Info("keys", "ring", r, "key", a, "struct", withKeys{r, a, b})
		l.Info("group", slog.Group("g", "pw", v), slog.Any("any", v))
		for _, err := range errs {
			l.Error("failed", "err", err)
		}
	}
	log.New(&out, "", 0).Println(v, r, a, exported{v})

	text := out.String()
	needles := map[string]string{
		"plaintext":    testPlain,
		"kek raw":      string(kek),
		"kek base64":   kekA,
		"kek hex":      hex.EncodeToString(kek),
		"kek b64 head": kekA[:16],
		"wrap key raw": string(wrapKey),
		"wrap key hex": hex.EncodeToString(wrapKey),
		"wrap key b64": base64.StdEncoding.EncodeToString(wrapKey),
		"dek raw":      string(dek),
		"dek hex":      hex.EncodeToString(dek),
		"dek b64":      base64.StdEncoding.EncodeToString(dek),
		"dek bytes":    fmt.Sprintf("%d", dek[:8]),
	}
	for name, n := range needles {
		if len(n) < 8 {
			t.Fatalf("needle %s is too short to mean anything", name)
		}
		if strings.Contains(text, n) {
			t.Errorf("output contains the %s", name)
		}
	}
	// Plaintext bytes, in the forms fmt would print a []byte.
	pt := []byte(testPlain)
	for _, n := range []string{fmt.Sprintf("%d", pt), fmt.Sprintf("%x", pt), fmt.Sprintf("%d", pt[:6])} {
		if strings.Contains(text, n) {
			t.Errorf("output contains plaintext bytes %q", n)
		}
	}
	if !strings.Contains(text, redacted) {
		t.Error("no redaction marker; the test printed nothing useful")
	}
}
