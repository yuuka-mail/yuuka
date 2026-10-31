package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authn"
	"github.com/yuuka-mail/yuuka/internal/clientip"
	"github.com/yuuka-mail/yuuka/internal/mailnet"
	"github.com/yuuka-mail/yuuka/internal/smtpd"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/submit"
)

// submissionConfig is the SMTP submission port of serve (§13.6). It is
// off unless Listen is set: the port takes passwords, so it exists only
// where someone asked for it.
type submissionConfig struct {
	Listen string
	// CertFile and KeyFile make yuuka terminate TLS; without them a
	// proxy in Proxies terminates it.
	CertFile, KeyFile string
	// Proxies send the PROXY protocol; a separate list from
	// YUUKA_TRUSTED_PROXIES (§13.6 says why).
	Proxies clientip.Proxies
	// Hostname is the name the port greets with. Empty means the name on
	// the certificate when yuuka terminates TLS, else the WebUI's host:
	// clients connect to the submission host, which is often not the
	// WebUI's (that one may sit behind a CDN that does not carry SMTP).
	Hostname string
}

func loadSubmissionConfig(getenv func(string) string) (submissionConfig, error) {
	c := submissionConfig{
		Listen:   getenv("YUUKA_SUBMISSION_LISTEN"),
		CertFile: getenv("YUUKA_SUBMISSION_TLS_CERT_FILE"),
		KeyFile:  getenv("YUUKA_SUBMISSION_TLS_KEY_FILE"),
		Hostname: getenv("YUUKA_SUBMISSION_HOSTNAME"),
	}
	var errs []error
	var err error
	if c.Proxies, err = clientip.ParseProxies(getenv("YUUKA_SUBMISSION_PROXIES")); err != nil {
		errs = append(errs, fmt.Errorf("YUUKA_SUBMISSION_PROXIES: %w", err))
	}
	switch {
	case c.Listen == "" && (c.CertFile != "" || c.KeyFile != "" || !c.Proxies.Empty() || c.Hostname != ""):
		errs = append(errs, errors.New("YUUKA_SUBMISSION_TLS_*, YUUKA_SUBMISSION_PROXIES and YUUKA_SUBMISSION_HOSTNAME configure the submission port, which is off without YUUKA_SUBMISSION_LISTEN"))
	case (c.CertFile == "") != (c.KeyFile == ""):
		errs = append(errs, errors.New("set both YUUKA_SUBMISSION_TLS_CERT_FILE and YUUKA_SUBMISSION_TLS_KEY_FILE, or neither"))
	case c.Listen != "" && c.CertFile == "" && c.Proxies.Empty():
		errs = append(errs, errors.New("YUUKA_SUBMISSION_LISTEN: without YUUKA_SUBMISSION_TLS_CERT_FILE a proxy in front must terminate TLS, and YUUKA_SUBMISSION_PROXIES must name it; plaintext from anyone else is never accepted"))
	}
	if c.Listen != "" {
		if _, _, err := net.SplitHostPort(c.Listen); err != nil {
			errs = append(errs, fmt.Errorf("YUUKA_SUBMISSION_LISTEN: %w", err))
		}
	}
	if c.Hostname != "" && !isHostname(c.Hostname) {
		errs = append(errs, fmt.Errorf("YUUKA_SUBMISSION_HOSTNAME: %q is not a host name such as smtp.example.com", c.Hostname))
	}
	return c, errors.Join(errs...)
}

// isHostname accepts what may follow "220 " in a greeting: dot-separated
// labels of letters, digits and hyphens. No port, scheme or wildcard.
func isHostname(s string) bool {
	if len(s) > 253 {
		return false
	}
	for label := range strings.SplitSeq(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		if strings.TrimFunc(label, func(r rune) bool {
			return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-'
		}) != "" {
			return false
		}
	}
	return true
}

// certHostname is the first DNS name on the leaf certificate in a PEM
// file that a greeting can use, or "" if there is none (a wildcard names
// no host). Read once at start: a renewal keeps the name.
func certHostname(certFile string) string {
	b, err := os.ReadFile(certFile)
	if err != nil {
		return ""
	}
	for {
		var blk *pem.Block
		if blk, b = pem.Decode(b); blk == nil {
			return ""
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		leaf, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return ""
		}
		for _, n := range leaf.DNSNames {
			if isHostname(n) {
				return n
			}
		}
		return ""
	}
}

// submitPort is the submission core as the port consumes it: the core's
// refusals (a broken §13.6 rule, tier none, no identity) are the port's
// final answer, 550; anything else is temporary.
type submitPort struct{ *submit.Service }

func (p submitPort) SubmitRaw(ctx context.Context, v *auth.Viewer, mailFrom string, rcpts []string, raw []byte) (smtpd.Result, error) {
	r, err := p.Service.SubmitRaw(ctx, v, mailFrom, rcpts, raw)
	if errors.Is(err, submit.ErrRejected) || errors.Is(err, submit.ErrNoIdentity) {
		err = fmt.Errorf("%w: %w", smtpd.ErrRefused, err)
	}
	return smtpd.Result(r), err
}

// startSubmission opens the submission port and serves it in the
// background. webHost is the greeting's last resort (see
// submissionConfig.Hostname). Errors from serving arrive on the returned
// channel; stop shuts the port down, waiting for open sessions until its
// context ends.
func startSubmission(cfg submissionConfig, sub smtpd.Submitter, db *store.DB, webHost string, log *slog.Logger) (stop func(context.Context) error, errc <-chan error, err error) {
	var tlsCfg *tls.Config
	domain := cfg.Hostname
	if cfg.CertFile != "" {
		cf, err := mailnet.LoadCertFiles(cfg.CertFile, cfg.KeyFile, log)
		if err != nil {
			return nil, nil, fmt.Errorf("submission: %w", err)
		}
		tlsCfg = cf.TLSConfig()
		if domain == "" {
			domain = certHostname(cfg.CertFile)
		}
	}
	if domain == "" {
		domain = webHost
	}
	srv := smtpd.New(smtpd.Config{
		Submitter: sub,
		Auth:      authn.NewBearer(db, log, nil),
		Audit:     db,
		TLS:       tlsCfg,
		Proxies:   cfg.Proxies,
		Domain:    domain,
		Log:       log,
	})
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", cfg.Listen)
	if err != nil {
		return nil, nil, fmt.Errorf("submission: %w", err)
	}
	ch := make(chan error, 1)
	go func() {
		log.Info("submission port listening", "addr", ln.Addr().String(), "tls", tlsCfg != nil, "hostname", domain)
		ch <- srv.Serve(ln)
	}()
	return srv.Shutdown, ch, nil
}
