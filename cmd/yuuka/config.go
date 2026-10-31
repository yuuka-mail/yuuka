package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yuuka-mail/yuuka/internal/authn"
	"github.com/yuuka-mail/yuuka/internal/clientip"
	"github.com/yuuka-mail/yuuka/internal/r2"
)

// minSecretLen is the §2.2 minimum for a webhook secret.
const minSecretLen = 32

type config struct {
	DB                dbConfig
	Raw               r2.Config
	Pending           r2.Config
	WebhookSecrets    [][]byte
	DataDir           string
	Listen            string
	Account           string
	TrustedAuthservID string
	TelegramToken     string
	TelegramChat      string
	ReconcileInterval time.Duration
	Web               webConfig
	Proxies           clientip.Proxies
}

// webConfig is the WebUI login, shared by serve and admin. An empty
// Origin means serve runs without it (ingest only).
type webConfig struct {
	Origin string
	RPID   string
}

func loadWebConfig(getenv func(string) string) (webConfig, error) {
	w := webConfig{Origin: getenv("YUUKA_WEB_ORIGIN"), RPID: getenv("YUUKA_WEBAUTHN_RP_ID")}
	if w.Origin == "" {
		if w.RPID != "" {
			return w, errors.New("YUUKA_WEBAUTHN_RP_ID needs YUUKA_WEB_ORIGIN")
		}
		return w, nil
	}
	if _, _, err := authn.ParseOrigin(w.Origin); err != nil {
		return w, fmt.Errorf("YUUKA_WEB_ORIGIN: %w", err)
	}
	return w, nil
}

// adminConfig is what `yuuka admin` needs: no R2, no webhook secrets.
type adminConfig struct {
	DB  dbConfig
	Web webConfig
}

func loadAdminConfig(getenv func(string) string) (adminConfig, error) {
	var errs []error
	var c adminConfig
	var err error
	if c.DB, err = loadDBConfig(getenv); err != nil {
		errs = append(errs, err)
	}
	w, err := loadWebConfig(getenv)
	c.Web = w
	if err != nil {
		errs = append(errs, err)
	} else if w.Origin == "" {
		errs = append(errs, errors.New("YUUKA_WEB_ORIGIN is required: enrollment links point at the WebUI"))
	}
	return c, errors.Join(errs...)
}

// loadConfig reads the environment (see the package comment for the list).
// needSecrets is false for commands that never accept webhooks.
func loadConfig(getenv func(string) string, needSecrets bool) (config, error) {
	var errs []error
	req := func(name string) string {
		v := getenv(name)
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
		return v
	}
	def := func(name, fallback string) string {
		if v := getenv(name); v != "" {
			return v
		}
		return fallback
	}

	endpoint := req("YUUKA_R2_ENDPOINT")
	db, err := loadDBConfig(getenv)
	if err != nil {
		errs = append(errs, err)
	}
	c := config{
		DB: db,
		Raw: r2.Config{
			Endpoint:        endpoint,
			Bucket:          req("YUUKA_R2_RAW_BUCKET"),
			AccessKeyID:     req("YUUKA_R2_RAW_ACCESS_KEY_ID"),
			SecretAccessKey: req("YUUKA_R2_RAW_SECRET_ACCESS_KEY"),
		},
		Pending: r2.Config{
			Endpoint:        endpoint,
			Bucket:          req("YUUKA_R2_PENDING_BUCKET"),
			AccessKeyID:     req("YUUKA_R2_PENDING_ACCESS_KEY_ID"),
			SecretAccessKey: req("YUUKA_R2_PENDING_SECRET_ACCESS_KEY"),
		},
		DataDir: req("YUUKA_DATA_DIR"),
		Listen:  def("YUUKA_LISTEN", "127.0.0.1:31480"),
		Account: def("YUUKA_ACCOUNT", "cloudflare"),
		// Measured for Cloudflare Email Routing (§10.1.1).
		TrustedAuthservID: def("YUUKA_TRUSTED_AUTHSERV_ID", "mx.cloudflare.net"),
		TelegramToken:     getenv("YUUKA_TELEGRAM_BOT_TOKEN"),
		TelegramChat:      getenv("YUUKA_TELEGRAM_CHAT_ID"),
	}
	if c.Raw.Bucket != "" && c.Raw.Bucket == c.Pending.Bucket {
		errs = append(errs, errors.New("YUUKA_R2_RAW_BUCKET and YUUKA_R2_PENDING_BUCKET must differ (§2.2)"))
	}
	if (c.TelegramToken == "") != (c.TelegramChat == "") {
		errs = append(errs, errors.New("set both YUUKA_TELEGRAM_BOT_TOKEN and YUUKA_TELEGRAM_CHAT_ID, or neither"))
	}
	interval, err := time.ParseDuration(def("YUUKA_RECONCILE_INTERVAL", "5m"))
	if err != nil || interval <= 0 {
		errs = append(errs, fmt.Errorf("YUUKA_RECONCILE_INTERVAL: want a positive duration like 5m"))
	}
	c.ReconcileInterval = interval
	if c.Web, err = loadWebConfig(getenv); err != nil {
		errs = append(errs, err)
	}
	if c.Proxies, err = clientip.ParseProxies(getenv("YUUKA_TRUSTED_PROXIES")); err != nil {
		errs = append(errs, fmt.Errorf("YUUKA_TRUSTED_PROXIES: %w", err))
	}

	if needSecrets {
		for i, s := range strings.Split(req("YUUKA_WEBHOOK_SECRETS"), ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if len(s) < minSecretLen {
				errs = append(errs, fmt.Errorf("YUUKA_WEBHOOK_SECRETS: secret #%d is shorter than %d bytes", i+1, minSecretLen))
				continue
			}
			c.WebhookSecrets = append(c.WebhookSecrets, []byte(s))
		}
	}
	return c, errors.Join(errs...)
}
