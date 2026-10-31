package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/yuuka-mail/yuuka/internal/authn"
	"github.com/yuuka-mail/yuuka/internal/clientip"
	"github.com/yuuka-mail/yuuka/internal/mailnet"
	"github.com/yuuka-mail/yuuka/internal/smtpd"
	"github.com/yuuka-mail/yuuka/internal/store"
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
}

func loadSubmissionConfig(getenv func(string) string) (submissionConfig, error) {
	c := submissionConfig{
		Listen:   getenv("YUUKA_SUBMISSION_LISTEN"),
		CertFile: getenv("YUUKA_SUBMISSION_TLS_CERT_FILE"),
		KeyFile:  getenv("YUUKA_SUBMISSION_TLS_KEY_FILE"),
	}
	var errs []error
	var err error
	if c.Proxies, err = clientip.ParseProxies(getenv("YUUKA_SUBMISSION_PROXIES")); err != nil {
		errs = append(errs, fmt.Errorf("YUUKA_SUBMISSION_PROXIES: %w", err))
	}
	switch {
	case c.Listen == "" && (c.CertFile != "" || c.KeyFile != "" || !c.Proxies.Empty()):
		errs = append(errs, errors.New("YUUKA_SUBMISSION_TLS_* and YUUKA_SUBMISSION_PROXIES configure the submission port, which is off without YUUKA_SUBMISSION_LISTEN"))
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
	return c, errors.Join(errs...)
}

// startSubmission opens the submission port and serves it in the
// background. Errors from serving arrive on the returned channel; stop
// shuts the port down, waiting for open sessions until its context ends.
func startSubmission(cfg submissionConfig, sub smtpd.Submitter, db *store.DB, domain string, log *slog.Logger) (stop func(context.Context) error, errc <-chan error, err error) {
	var tlsCfg *tls.Config
	if cfg.CertFile != "" {
		cf, err := mailnet.LoadCertFiles(cfg.CertFile, cfg.KeyFile, log)
		if err != nil {
			return nil, nil, fmt.Errorf("submission: %w", err)
		}
		tlsCfg = cf.TLSConfig()
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
		log.Info("submission port listening", "addr", ln.Addr().String(), "tls", tlsCfg != nil)
		ch <- srv.Serve(ln)
	}()
	return srv.Shutdown, ch, nil
}
