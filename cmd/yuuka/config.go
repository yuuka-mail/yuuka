package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yuuka-mail/yuuka/internal/r2"
)

// minSecretLen is the §2.2 minimum for a webhook secret.
const minSecretLen = 32

type config struct {
	DatabaseURL       string
	Raw               r2.Config
	Pending           r2.Config
	WebhookSecrets    [][]byte
	DataDir           string
	Listen            string
	Account           string
	TelegramToken     string
	TelegramChat      string
	ReconcileInterval time.Duration
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
	c := config{
		DatabaseURL: req("YUUKA_DATABASE_URL"),
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
		DataDir:       req("YUUKA_DATA_DIR"),
		Listen:        def("YUUKA_LISTEN", "127.0.0.1:31480"),
		Account:       def("YUUKA_ACCOUNT", "cloudflare"),
		TelegramToken: getenv("YUUKA_TELEGRAM_BOT_TOKEN"),
		TelegramChat:  getenv("YUUKA_TELEGRAM_CHAT_ID"),
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
