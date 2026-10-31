package main

import (
	"bufio"
	"context"
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
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/smtpd"
)

func TestLoadSubmissionConfig(t *testing.T) {
	c, err := loadConfig(env(nil), true)
	if err != nil || c.Submission.Listen != "" {
		t.Fatalf("off by default: %+v %v", c.Submission, err)
	}
	for name, over := range map[string]map[string]string{
		"own TLS":          {"YUUKA_SUBMISSION_LISTEN": ":465", "YUUKA_SUBMISSION_TLS_CERT_FILE": "c.pem", "YUUKA_SUBMISSION_TLS_KEY_FILE": "k.pem"},
		"proxy TLS":        {"YUUKA_SUBMISSION_LISTEN": "0.0.0.0:31465", "YUUKA_SUBMISSION_PROXIES": "10.213.77.0/24"},
		"both":             {"YUUKA_SUBMISSION_LISTEN": ":465", "YUUKA_SUBMISSION_TLS_CERT_FILE": "c.pem", "YUUKA_SUBMISSION_TLS_KEY_FILE": "k.pem", "YUUKA_SUBMISSION_PROXIES": "127.0.0.1"},
		"own HTTP proxies": {"YUUKA_SUBMISSION_LISTEN": ":465", "YUUKA_SUBMISSION_PROXIES": "127.0.0.1", "YUUKA_TRUSTED_PROXIES": "10.0.0.0/8"},
	} {
		if _, err := loadConfig(env(over), true); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, over := range map[string]map[string]string{
		"no TLS, no proxy":   {"YUUKA_SUBMISSION_LISTEN": ":465"},
		"HTTP proxies only":  {"YUUKA_SUBMISSION_LISTEN": ":465", "YUUKA_TRUSTED_PROXIES": "10.0.0.0/8"},
		"cert without key":   {"YUUKA_SUBMISSION_LISTEN": ":465", "YUUKA_SUBMISSION_TLS_CERT_FILE": "c.pem"},
		"cert without port":  {"YUUKA_SUBMISSION_TLS_CERT_FILE": "c.pem", "YUUKA_SUBMISSION_TLS_KEY_FILE": "k.pem"},
		"proxy without port": {"YUUKA_SUBMISSION_PROXIES": "127.0.0.1"},
		"bad proxy":          {"YUUKA_SUBMISSION_LISTEN": ":465", "YUUKA_SUBMISSION_PROXIES": "proxy.example.com"},
		"bad address":        {"YUUKA_SUBMISSION_LISTEN": "465", "YUUKA_SUBMISSION_PROXIES": "127.0.0.1"},
	} {
		if _, err := loadConfig(env(over), true); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// testCert writes a self-signed certificate for mail.example.com into
// dir and returns its files and a pool that trusts it.
func testCert(t *testing.T, dir string) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mail.example.com"}, DNSNames: []string{"mail.example.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certFile, keyFile, pool
}

type noCore struct{}

func (noCore) SubmitRaw(context.Context, *auth.Viewer, string, []string, []byte) (smtpd.Result, error) {
	return smtpd.Result{}, nil
}

// With certificate files the port terminates TLS itself and greets with
// the configured name; stop closes it.
func TestStartSubmission(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, pool := testCert(t, dir)

	// Find a free port, then let the port open it.
	probe, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	stop, errc, err := startSubmission(submissionConfig{Listen: addr, CertFile: certFile, KeyFile: keyFile}, noCore{}, nil, "mail.example.com", log)
	if err != nil {
		t.Fatal(err)
	}
	c, err := (&tls.Dialer{Config: &tls.Config{RootCAs: pool, ServerName: "mail.example.com", MinVersion: tls.VersionTLS12}}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "220 mail.example.com ESMTP") {
		t.Errorf("greeting %q %v", line, err)
	}
	_ = c.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := stop(ctx); err != nil {
		t.Error(err)
	}
	if err := <-errc; err != nil {
		t.Errorf("serve: %v", err)
	}

	if _, _, err := startSubmission(submissionConfig{Listen: addr, CertFile: filepath.Join(dir, "missing.pem"), KeyFile: keyFile}, noCore{}, nil, "", log); err == nil {
		t.Error("started without a certificate")
	}
}
