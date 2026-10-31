package mailnet

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writePair writes a fresh self-signed certificate for name and its key
// as PEM, stamped with mtime, and returns the certificate's DER.
func writePair(t *testing.T, certFile, keyFile, name string, mtime time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for file, block := range map[string]*pem.Block{certFile: {Type: "CERTIFICATE", Bytes: der}, keyFile: {Type: "PRIVATE KEY", Bytes: kb}} {
		if err := os.WriteFile(file, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(file, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	return der
}

// A renewal written over the files is served from the next check on; a
// pair that does not load leaves the previous certificate in service.
func TestCertFilesReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if _, err := LoadCertFiles(certFile, keyFile, nil); err == nil {
		t.Fatal("loaded missing files")
	}
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	first := writePair(t, certFile, keyFile, "a.example.com", t0)
	cf, err := LoadCertFiles(certFile, keyFile, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cf.now = func() time.Time { return now }
	serving := func() []byte {
		t.Helper()
		c, err := cf.TLSConfig().GetCertificate(&tls.ClientHelloInfo{})
		if err != nil {
			t.Fatal(err)
		}
		return c.Certificate[0]
	}
	if string(serving()) != string(first) {
		t.Fatal("not serving the files")
	}

	second := writePair(t, certFile, keyFile, "a.example.com", t0.Add(time.Minute))
	if string(serving()) != string(first) {
		t.Error("reloaded before the check interval")
	}
	now = now.Add(certCheckEvery)
	if string(serving()) != string(second) {
		t.Error("renewal not picked up")
	}

	if err := os.WriteFile(certFile, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(certFile, t0.Add(2*time.Minute), t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(certCheckEvery)
	if string(serving()) != string(second) {
		t.Error("a broken pair replaced the certificate in service")
	}
	third := writePair(t, certFile, keyFile, "a.example.com", t0.Add(3*time.Minute))
	now = now.Add(certCheckEvery)
	if string(serving()) != string(third) {
		t.Error("a good pair after a broken one not picked up")
	}
}
