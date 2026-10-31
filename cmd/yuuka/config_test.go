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
	if len(c.WebhookSecrets) != 2 || c.Listen != "127.0.0.1:31480" || c.Account != "cloudflare" ||
		c.TrustedAuthservID != "mx.cloudflare.net" {
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
		"http origin":    {"YUUKA_WEB_ORIGIN": "http://mail.example.com"},
		"rp id alone":    {"YUUKA_WEBAUTHN_RP_ID": "example.com"},
		"proxy by name":  {"YUUKA_TRUSTED_PROXIES": "localhost"},
	} {
		if _, err := loadConfig(env(over), true); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := loadConfig(env(map[string]string{"YUUKA_WEBHOOK_SECRETS": ""}), false); err != nil {
		t.Errorf("reconcile must not need webhook secrets: %v", err)
	}
}

func TestWebConfig(t *testing.T) {
	c, err := loadConfig(env(nil), true)
	if err != nil || c.Web.Origin != "" {
		t.Fatalf("serve must run without the WebUI: %+v %v", c.Web, err)
	}
	c, err = loadConfig(env(map[string]string{"YUUKA_WEB_ORIGIN": "https://mail.example.com"}), true)
	if err != nil || c.Web.Origin != "https://mail.example.com" {
		t.Fatalf("%+v %v", c.Web, err)
	}
}

func TestLoadAdminConfig(t *testing.T) {
	// No R2 or webhook settings needed, but an origin is.
	only := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	c, err := loadAdminConfig(only(map[string]string{
		"YUUKA_DATABASE_URL": "postgres://localhost/yuuka",
		"YUUKA_WEB_ORIGIN":   "https://mail.example.com",
	}))
	if err != nil || c.Web.Origin != "https://mail.example.com" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := loadAdminConfig(only(map[string]string{"YUUKA_DATABASE_URL": "postgres://localhost/yuuka"})); err == nil {
		t.Error("admin accepted without YUUKA_WEB_ORIGIN")
	}
}

// The recall button needs both operator variables, the bot, a numeric
// chat and a human principal.
func TestTelegramOperatorConfig(t *testing.T) {
	bot := map[string]string{"YUUKA_TELEGRAM_BOT_TOKEN": "t", "YUUKA_TELEGRAM_CHAT_ID": "-1001"}
	with := func(m map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range bot {
			out[k] = v
		}
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	c, err := loadConfig(env(with(map[string]string{"YUUKA_TELEGRAM_OPERATOR_ID": "12345", "YUUKA_TELEGRAM_OPERATOR": "human:operator"})), true)
	if err != nil || c.TelegramOperatorID != 12345 || c.TelegramOperator != "human:operator" {
		t.Fatalf("%+v %v", c, err)
	}
	if c, err := loadConfig(env(bot), true); err != nil || c.TelegramOperatorID != 0 {
		t.Errorf("bot without operator: %v", err)
	}
	for name, over := range map[string]map[string]string{
		"id alone":       with(map[string]string{"YUUKA_TELEGRAM_OPERATOR_ID": "12345"}),
		"operator alone": with(map[string]string{"YUUKA_TELEGRAM_OPERATOR": "human:operator"}),
		"no bot":         {"YUUKA_TELEGRAM_OPERATOR_ID": "12345", "YUUKA_TELEGRAM_OPERATOR": "human:operator"},
		"not a number":   with(map[string]string{"YUUKA_TELEGRAM_OPERATOR_ID": "@me", "YUUKA_TELEGRAM_OPERATOR": "human:operator"}),
		"chat by name": with(map[string]string{
			"YUUKA_TELEGRAM_CHAT_ID":     "@channel",
			"YUUKA_TELEGRAM_OPERATOR_ID": "12345", "YUUKA_TELEGRAM_OPERATOR": "human:operator",
		}),
		"an agent": with(map[string]string{"YUUKA_TELEGRAM_OPERATOR_ID": "12345", "YUUKA_TELEGRAM_OPERATOR": "agent:arisu"}),
	} {
		if _, err := loadConfig(env(over), true); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
