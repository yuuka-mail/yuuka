package mailnet

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// certCheckEvery is how often CertFiles looks at the files' mtimes. A
// renewal is served this long after it lands at worst; checking on
// every handshake would only add a pair of stat calls to each.
const certCheckEvery = time.Minute

// CertFiles serves a certificate from PEM files that something else
// keeps renewed (certbot, an ACME client, a proxy's certificate store),
// and picks up a renewal without a restart: when either file's mtime
// changes it loads the pair again. A pair that does not load (the
// certificate replaced but not yet the key, say) leaves the previous
// one in service and is tried again at the next check.
type CertFiles struct {
	certFile, keyFile string
	log               *slog.Logger
	now               func() time.Time

	mu              sync.Mutex
	cert            *tls.Certificate
	certMod, keyMod time.Time
	checked         time.Time
}

// LoadCertFiles loads a certificate chain and its key, both PEM. Unlike
// a later reload, failing here is an error: there is nothing to serve.
func LoadCertFiles(certFile, keyFile string, log *slog.Logger) (*CertFiles, error) {
	if log == nil {
		log = slog.Default()
	}
	c := &CertFiles{certFile: certFile, keyFile: keyFile, log: log, now: time.Now}
	if err := c.load(); err != nil {
		return nil, err
	}
	return c, nil
}

// TLSConfig is a server configuration that serves the files.
func (c *CertFiles) TLSConfig() *tls.Config {
	return &tls.Config{GetCertificate: c.GetCertificate, MinVersion: tls.VersionTLS12}
}

// GetCertificate is for tls.Config.GetCertificate.
func (c *CertFiles) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now := c.now(); now.Sub(c.checked) >= certCheckEvery {
		c.checked = now
		if c.changed() {
			if err := c.loadLocked(); err != nil {
				c.log.Error("mailnet: certificate files changed but do not load; still serving the previous certificate", "err", err)
			} else {
				c.log.Info("mailnet: certificate reloaded", "file", c.certFile)
			}
		}
	}
	return c.cert, nil
}

func (c *CertFiles) load() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checked = c.now()
	return c.loadLocked()
}

// loadLocked reads the pair and, if it is good, puts it in service with
// the mtimes it was read at.
func (c *CertFiles) loadLocked() error {
	certMod, keyMod, err := c.mtimes()
	if err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		return fmt.Errorf("load certificate %s and key %s: %w", c.certFile, c.keyFile, err)
	}
	c.cert, c.certMod, c.keyMod = &cert, certMod, keyMod
	return nil
}

func (c *CertFiles) changed() bool {
	certMod, keyMod, err := c.mtimes()
	return err == nil && (!certMod.Equal(c.certMod) || !keyMod.Equal(c.keyMod))
}

func (c *CertFiles) mtimes() (certMod, keyMod time.Time, err error) {
	ci, err := os.Stat(c.certFile)
	if err != nil {
		return certMod, keyMod, err
	}
	ki, err := os.Stat(c.keyFile)
	if err != nil {
		return certMod, keyMod, err
	}
	return ci.ModTime(), ki.ModTime(), nil
}
