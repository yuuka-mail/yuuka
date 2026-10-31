package main

import (
	"strings"
	"testing"
)

func env(over map[string]string) func(string) string {
	base := map[string]string{
		"YUUKA_DATABASE_URL":                 "postgres://localhost/yuuka",
		"YUUKA_R2_ENDPOINT":                  "https://account.r2.example.com",
		"YUUKA_R2_RAW_BUCKET":                "raw",
		"YUUKA_R2_RAW_ACCESS_KEY_ID":         "rk",
		"YUUKA_R2_RAW_SECRET_ACCESS_KEY":     "rs",
		"YUUKA_R2_PENDING_BUCKET":            "pending",
		"YUUKA_R2_PENDING_ACCESS_KEY_ID":     "pk",
		"YUUKA_R2_PENDING_SECRET_ACCESS_KEY": "ps",
		"YUUKA_DATA_DIR":                     "/tmp/yuuka",
		"YUUKA_WEBHOOK_SECRETS":              strings.Repeat("a", 32) + " , " + strings.Repeat("b", 40),
	}
	for k, v := range over {
		base[k] = v
	}
	return func(k string) string { return base[k] }
}

func TestLoadConfig(t *testing.T) {
	c, err := loadConfig(env(nil), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.WebhookSecrets) != 2 || c.Listen != "127.0.0.1:31480" || c.Account != "cloudflare" {
		t.Fatalf("%+v", c)
	}
	if c.Raw.AccessKeyID != "rk" || c.Pending.AccessKeyID != "pk" || c.Raw.Endpoint != c.Pending.Endpoint {
		t.Fatalf("bucket credentials mixed up: %+v %+v", c.Raw, c.Pending)
	}
}

func TestLoadConfigRejects(t *testing.T) {
	for name, over := range map[string]map[string]string{
		"short secret":   {"YUUKA_WEBHOOK_SECRETS": "short"},
		"no secrets":     {"YUUKA_WEBHOOK_SECRETS": ""},
		"same bucket":    {"YUUKA_R2_PENDING_BUCKET": "raw"},
		"half telegram":  {"YUUKA_TELEGRAM_BOT_TOKEN": "t"},
		"bad interval":   {"YUUKA_RECONCILE_INTERVAL": "-1m"},
		"missing raw id": {"YUUKA_R2_RAW_ACCESS_KEY_ID": ""},
	} {
		if _, err := loadConfig(env(over), true); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := loadConfig(env(map[string]string{"YUUKA_WEBHOOK_SECRETS": ""}), false); err != nil {
		t.Errorf("reconcile must not need webhook secrets: %v", err)
	}
}
