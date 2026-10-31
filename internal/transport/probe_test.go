package transport

import (
	"errors"
	"net"
	"testing"
)

func probe(t *testing.T, cfg Config) error {
	t.Helper()
	s, err := NewSMTP(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s.Probe(t.Context())
}

// TestProbe: a probe authenticates and stops there; the server never
// hears MAIL or DATA.
func TestProbe(t *testing.T) {
	srv := newServer(t, behavior{})
	if err := probe(t, srv.config()); err != nil {
		t.Fatal(err)
	}
	s := srv.next(t)
	if s.auth != "\x00user\x00secret" {
		t.Errorf("AUTH %q", s.auth)
	}
	if s.mail != "" || s.sawData || len(s.rcpts) > 0 {
		t.Errorf("probe went past AUTH: %+v", s)
	}
}

func TestProbeNoAuth(t *testing.T) {
	srv := newServer(t, behavior{ext: []string{"8BITMIME"}})
	cfg := srv.config()
	cfg.Username, cfg.Password = "", ""
	if err := probe(t, cfg); err != nil {
		t.Fatal(err)
	}
	if s := srv.next(t); s.auth != "" || s.mail != "" {
		t.Errorf("session %+v", s)
	}
}

func TestProbeFailures(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	for name, c := range map[string]struct {
		b     behavior
		cfg   func(Config) Config
		stage Stage
	}{
		"refused":       {cfg: func(c Config) Config { c.Port = closed; return c }, stage: StageConnect},
		"untrusted":     {cfg: func(c Config) Config { c.RootCAs = nil; return c }, stage: StageConnect},
		"greeting":      {b: behavior{greeting: "554 go away"}, stage: StageGreeting},
		"bad password":  {b: behavior{auth: "535 5.7.8 Authentication credentials invalid"}, stage: StageAuth},
		"no auth plain": {b: behavior{ext: []string{"AUTH LOGIN"}}, stage: StageAuth},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newServer(t, c.b)
			cfg := srv.config()
			if c.cfg != nil {
				cfg = c.cfg(cfg)
			}
			err := probe(t, cfg)
			if e, ok := errors.AsType[*Error](err); !ok || e.Stage != c.stage || !ExitFailure(err) {
				t.Errorf("error %v, want an exit failure at %s", err, c.stage)
			}
		})
	}
}
