package triage

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/triage/score"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func noFile(string) ([]byte, error) { return nil, errors.New("no such file") }

func TestLoadConfigOff(t *testing.T) {
	c, err := LoadConfig(envOf(nil), noFile)
	if err != nil || c.On() {
		t.Fatalf("%+v, %v", c, err)
	}
	for _, k := range []string{"YUUKA_TRIAGE_NAME", "YUUKA_TRIAGE_HOSTED", "YUUKA_TRIAGE_TOKEN_FILE", "YUUKA_TRIAGE_LOOKBACK", "YUUKA_TRIAGE_MODEL", "YUUKA_TRIAGE_ACCOUNTS"} {
		if _, err := LoadConfig(envOf(map[string]string{k: "x"}), noFile); err == nil {
			t.Errorf("%s without a URL accepted", k)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	read := func(p string) ([]byte, error) {
		if p == "/run/token" {
			return []byte("secret-token\n"), nil
		}
		return noFile(p)
	}
	c, err := LoadConfig(envOf(map[string]string{
		"YUUKA_TRIAGE_URL":        "http://127.0.0.1:31490/",
		"YUUKA_TRIAGE_NAME":       "laya-ml-1",
		"YUUKA_TRIAGE_LOOKBACK":   "7d",
		"YUUKA_TRIAGE_TOKEN_FILE": "/run/token",
	}), read)
	if err != nil {
		t.Fatal(err)
	}
	if !c.On() || c.Endpoint != "http://127.0.0.1:31490/v1/systemone" || c.Token != "secret-token" ||
		c.Lookback != 7*24*time.Hour || c.Hosted || c.EndpointKind() != score.EndpointLocal {
		t.Errorf("%+v", c)
	}
	if p := c.Profile(); p.Questions != QuestionsVersion || p.Scorer != "laya-ml-1" {
		t.Errorf("profile %+v", p)
	}
	d, _ := LoadConfig(envOf(map[string]string{"YUUKA_TRIAGE_URL": "http://localhost:31490", "YUUKA_TRIAGE_NAME": "kev-1"}), noFile)
	if d.Lookback != DefaultLookback || d.Token != "" || d.Accounts != nil {
		t.Errorf("defaults %+v", d)
	}
	a, err := LoadConfig(envOf(map[string]string{
		"YUUKA_TRIAGE_URL": "http://localhost:31490", "YUUKA_TRIAGE_NAME": "kev-1",
		"YUUKA_TRIAGE_ACCOUNTS": "cloudflare, gmail",
	}), noFile)
	if err != nil || strings.Join(a.Accounts, ",") != "cloudflare,gmail" {
		t.Errorf("accounts %q, %v", a.Accounts, err)
	}
}

func TestLoadConfigRejects(t *testing.T) {
	base := map[string]string{"YUUKA_TRIAGE_URL": "http://10.0.0.5:31490", "YUUKA_TRIAGE_NAME": "laya-ml-1"}
	for name, c := range map[string]struct {
		over map[string]string
		want string
	}{
		"no name":            {map[string]string{"YUUKA_TRIAGE_NAME": ""}, "YUUKA_TRIAGE_NAME"},
		"bad name":           {map[string]string{"YUUKA_TRIAGE_NAME": "Laya ML"}, "YUUKA_TRIAGE_NAME"},
		"bad lookback":       {map[string]string{"YUUKA_TRIAGE_LOOKBACK": "a month"}, "LOOKBACK"},
		"token file missing": {map[string]string{"YUUKA_TRIAGE_TOKEN_FILE": "/nope"}, "TOKEN_FILE"},
		"hosted, no token":   {map[string]string{"YUUKA_TRIAGE_HOSTED": "1", "YUUKA_TRIAGE_URL": "https://scorer.example.com"}, "TOKEN_FILE"},
		"hosted over http":   {map[string]string{"YUUKA_TRIAGE_HOSTED": "1", "YUUKA_TRIAGE_URL": "http://scorer.example.com"}, "https only"},
		"hosted=yes":         {map[string]string{"YUUKA_TRIAGE_HOSTED": "yes"}, "HOSTED"},
		"public address":     {map[string]string{"YUUKA_TRIAGE_URL": "http://203.0.113.7:8000"}, "not a loopback or private"},
		"public v6":          {map[string]string{"YUUKA_TRIAGE_URL": "http://[2001:db8::1]:8000"}, "not a loopback or private"},
		"link-local":         {map[string]string{"YUUKA_TRIAGE_URL": "http://169.254.169.254"}, "not a loopback or private"},
		"token in URL":       {map[string]string{"YUUKA_TRIAGE_URL": "http://user:pass@127.0.0.1:31490"}, "credentials"},
		"query":              {map[string]string{"YUUKA_TRIAGE_URL": "http://127.0.0.1:31490/?key=x"}, "query"},
		"ftp":                {map[string]string{"YUUKA_TRIAGE_URL": "ftp://127.0.0.1"}, "http or https"},
		"bad account":        {map[string]string{"YUUKA_TRIAGE_ACCOUNTS": "cloudflare,,gmail"}, "YUUKA_TRIAGE_ACCOUNTS"},
	} {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for k, v := range c.over {
			env[k] = v
		}
		_, err := LoadConfig(envOf(env), noFile)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error naming %q", name, err, c.want)
		}
	}
}

// A hosted scorer is whatever the operator names, and its scores say so.
func TestLoadConfigHosted(t *testing.T) {
	c, err := LoadConfig(envOf(map[string]string{
		"YUUKA_TRIAGE_URL": "https://scorer.example.com", "YUUKA_TRIAGE_NAME": "jev-1",
		"YUUKA_TRIAGE_HOSTED": "1", "YUUKA_TRIAGE_TOKEN_FILE": "/t",
	}), func(string) ([]byte, error) { return []byte("tok"), nil })
	if err != nil {
		t.Fatal(err)
	}
	if c.EndpointKind() != score.EndpointHosted || c.Endpoint != "https://scorer.example.com/v1/systemone" {
		t.Errorf("%+v", c)
	}
}
