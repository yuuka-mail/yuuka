package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/triage"
)

func triageConfig(t *testing.T, url string) triage.Config {
	t.Helper()
	tc, err := triage.LoadConfig(func(k string) string {
		return map[string]string{"YUUKA_TRIAGE_URL": url, "YUUKA_TRIAGE_NAME": "laya-ml-1"}[k]
	}, os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	return tc
}

// status says what is configured, how far scoring got, and whether the
// scorer answers a probe that carries no mail.
func TestTriageStatus(t *testing.T) {
	t.Parallel()
	e := newHostEnv(t)
	answer, err := os.ReadFile("../../internal/triage/testdata/systemone/kev.json")
	if err != nil {
		t.Fatal(err)
	}
	probes := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		probes <- b
		_, _ = w.Write(answer)
	}))
	defer srv.Close()
	var out bytes.Buffer
	h := &host{db: e.db, out: &out, msg: &out, now: time.Now}
	if err := h.triageStatus(t.Context(), triageConfig(t, srv.URL)); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"laya-ml-1 (questions v1)", "(local)", "0 scored, 0 to score", "probe       ok", "model kev-latest"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("status lacks %q:\n%s", s, out.String())
		}
	}
	if probe := <-probes; !bytes.Contains(probe, []byte("There is no email here")) {
		t.Errorf("probe sent %s", probe)
	}

	srv.Close()
	out.Reset()
	if err := h.triageStatus(t.Context(), triageConfig(t, srv.URL)); err == nil || !strings.Contains(out.String(), "probe       failed") {
		t.Errorf("a scorer that is down: %v\n%s", err, out.String())
	}

	out.Reset()
	if err := h.triageReport(t.Context(), triageConfig(t, srv.URL), time.Time{}); err != nil || !strings.Contains(out.String(), "0 scored") {
		t.Errorf("report: %v\n%s", err, out.String())
	}
}

func TestLoadConfigTriage(t *testing.T) {
	c, err := loadConfig(env(map[string]string{"YUUKA_TRIAGE_URL": "http://10.0.0.5:31490", "YUUKA_TRIAGE_NAME": "laya-ml-1"}), true)
	if err != nil || !c.Triage.On() {
		t.Fatalf("%+v, %v", c.Triage, err)
	}
	if _, err := loadConfig(env(map[string]string{"YUUKA_TRIAGE_URL": "http://203.0.113.7", "YUUKA_TRIAGE_NAME": "laya-ml-1"}), true); err == nil {
		t.Error("a public scorer without YUUKA_TRIAGE_HOSTED accepted")
	}
	if c, err := loadConfig(env(nil), true); err != nil || c.Triage.On() {
		t.Errorf("default: %+v, %v", c.Triage, err)
	}
}
