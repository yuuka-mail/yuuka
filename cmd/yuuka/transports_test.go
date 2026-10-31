package main

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/transport"
)

func TestLoadOutboxConfig(t *testing.T) {
	c, err := loadConfig(env(nil), true)
	if err != nil || len(c.Outbox.Transports) != 0 || c.Outbox.Workers != 2 || c.Outbox.PollInterval != 30*time.Second {
		t.Fatalf("defaults: %+v %v", c.Outbox, err)
	}
	c, err = loadConfig(env(map[string]string{
		"YUUKA_TRANSPORTS":                         "cloudflare, work.mail",
		"YUUKA_TRANSPORT_CLOUDFLARE_HOST":          "smtp.example.com",
		"YUUKA_TRANSPORT_CLOUDFLARE_USERNAME":      "user",
		"YUUKA_TRANSPORT_CLOUDFLARE_PASSWORD_FILE": "/run/secrets/cf",
		"YUUKA_TRANSPORT_CLOUDFLARE_CAPS":          "cloudflare",
		"YUUKA_TRANSPORT_WORK_MAIL_HOST":           "mail.example.org",
		"YUUKA_TRANSPORT_WORK_MAIL_PORT":           "31465",
		"YUUKA_TRANSPORT_WORK_MAIL_CAPS":           "message-id, dkim",
		"YUUKA_OUTBOX_WORKERS":                     "4",
		"YUUKA_OUTBOX_POLL_INTERVAL":               "10s",
		"YUUKA_TRANSPORT_UNLISTED_HOST":            "ignored.example.com",
	}), true)
	if err != nil {
		t.Fatal(err)
	}
	want := []transportConfig{
		{Name: "cloudflare", Host: "smtp.example.com", Port: 465, Username: "user", PasswordFile: "/run/secrets/cf", Caps: transport.CloudflareCaps()},
		{Name: "work.mail", Host: "mail.example.org", Port: 31465, Caps: transport.Caps{CallerMessageID: true, CallerDKIM: true}},
	}
	if len(c.Outbox.Transports) != 2 || c.Outbox.Transports[0] != want[0] || c.Outbox.Transports[1] != want[1] ||
		c.Outbox.Workers != 4 || c.Outbox.PollInterval != 10*time.Second {
		t.Errorf("%+v", c.Outbox)
	}

	base := map[string]string{
		"YUUKA_TRANSPORTS":        "cf",
		"YUUKA_TRANSPORT_CF_HOST": "smtp.example.com",
		"YUUKA_TRANSPORT_CF_CAPS": "standard",
		"YUUKA_TRANSPORT_CF_PORT": "465",
		"YUUKA_OUTBOX_WORKERS":    "2",
	}
	with := func(over map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range over {
			m[k] = v
		}
		return m
	}
	if _, err := loadConfig(env(with(nil)), true); err != nil {
		t.Fatalf("base: %v", err)
	}
	for name, over := range map[string]map[string]string{
		"no host":              {"YUUKA_TRANSPORT_CF_HOST": ""},
		"no caps":              {"YUUKA_TRANSPORT_CF_CAPS": ""},
		"unknown cap":          {"YUUKA_TRANSPORT_CF_CAPS": "message-id,starttls"},
		"bad port":             {"YUUKA_TRANSPORT_CF_PORT": "70000"},
		"user without file":    {"YUUKA_TRANSPORT_CF_USERNAME": "u"},
		"file without user":    {"YUUKA_TRANSPORT_CF_PASSWORD_FILE": "/run/secrets/cf"},
		"password inline":      {"YUUKA_TRANSPORT_CF_USERNAME": "u", "YUUKA_TRANSPORT_CF_PASSWORD_FILE": "/f", "YUUKA_TRANSPORT_CF_PASSWORD": "secret"},
		"bad name":             {"YUUKA_TRANSPORTS": "cf,-x"},
		"colliding names":      {"YUUKA_TRANSPORTS": "cf,a.b,a-b", "YUUKA_TRANSPORT_A_B_HOST": "h", "YUUKA_TRANSPORT_A_B_CAPS": "none"},
		"zero workers":         {"YUUKA_OUTBOX_WORKERS": "0"},
		"bad poll interval":    {"YUUKA_OUTBOX_POLL_INTERVAL": "soon"},
		"negative poll period": {"YUUKA_OUTBOX_POLL_INTERVAL": "-1s"},
	} {
		if _, err := loadConfig(env(with(over)), true); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestOpenTransports(t *testing.T) {
	files := map[string]string{"/s/cf": "token\n", "/s/empty": "\n", "/s/crlf": "pw\r\n"}
	read := func(name string) ([]byte, error) {
		if v, ok := files[name]; ok {
			return []byte(v), nil
		}
		return nil, fs.ErrNotExist
	}
	trs, err := openTransports([]transportConfig{
		{Name: "cf", Host: "smtp.example.com", Port: 465, Username: "u", PasswordFile: "/s/cf", Caps: transport.CloudflareCaps()},
		{Name: "crlf", Host: "smtp.example.com", Port: 465, Username: "u", PasswordFile: "/s/crlf"},
		{Name: "open", Host: "smtp.example.com", Port: 465},
	}, read)
	if err != nil || len(trs) != 3 {
		t.Fatalf("%v %v", trs, err)
	}
	for name, tc := range map[string]transportConfig{
		"missing": {Name: "missing", Host: "h", Port: 465, Username: "u", PasswordFile: "/s/nope"},
		"empty":   {Name: "empty", Host: "h", Port: 465, Username: "u", PasswordFile: "/s/empty"},
	} {
		_, err := openTransports([]transportConfig{tc}, read)
		if err == nil || !strings.Contains(err.Error(), "transport "+name) {
			t.Errorf("%s: %v", name, err)
		}
		if name == "missing" && !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("missing: %v", err)
		}
	}
}
