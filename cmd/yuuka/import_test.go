package main

import (
	"bytes"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
)

// syncBuffer is a log destination several serve processes share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestImportEnvTransports(t *testing.T) {
	t.Parallel()
	e := newHostEnv(t)
	ctx := t.Context()
	const pw = "imported password"
	e.env = map[string]string{
		"YUUKA_TRANSPORTS":                         "cloudflare,relay,done",
		"YUUKA_TRANSPORT_CLOUDFLARE_HOST":          "SMTP.mx.cloudflare.net",
		"YUUKA_TRANSPORT_CLOUDFLARE_USERNAME":      "user",
		"YUUKA_TRANSPORT_CLOUDFLARE_PASSWORD_FILE": writeSecret(t, pw),
		"YUUKA_TRANSPORT_CLOUDFLARE_CAPS":          "cloudflare",
		"YUUKA_TRANSPORT_RELAY_HOST":               "relay.example.com",
		"YUUKA_TRANSPORT_RELAY_PORT":               "2465",
		"YUUKA_TRANSPORT_RELAY_CAPS":               "standard",
		"YUUKA_TRANSPORT_DONE_HOST":                "done.example.com",
		"YUUKA_TRANSPORT_DONE_CAPS":                "none",
	}
	oc, err := loadOutboxConfig(func(k string) string { return e.env[k] })
	if err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string { return e.env[k] }
	var logs syncBuffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	importOnce := func() {
		t.Helper()
		if err := importEnvTransports(ctx, log, e.db, oc.Transports, getenv, os.ReadFile); err != nil {
			t.Fatal(err)
		}
	}

	// cloudflare is a placeholder (0017 or an earlier serve); relay is
	// not in the database at all; done is already configured there.
	storetest.Transports(t, e.db, "cloudflare")
	e.env["YUUKA_KEK_FILE"] = writeKEK(t, "kek", 5)
	e.mustIn(nil, "transport", "add", "done", "--host", "elsewhere.example.com", "--caps", "dkim")
	delete(e.env, "YUUKA_KEK_FILE")

	// Without a KEK: nothing is written, and serve says why.
	importOnce()
	if tr, _ := e.db.TransportByName(ctx, "cloudflare"); tr.ConfiguredAt != nil {
		t.Errorf("imported without a KEK: %+v", tr)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "YUUKA_KEK_FILE") {
		t.Errorf("no warning without a KEK:\n%s", logs.String())
	}

	// With one, several processes starting at once import each transport
	// exactly once.
	e.env["YUUKA_KEK_FILE"] = writeKEK(t, "kek", 5)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(importOnce)
	}
	wg.Wait()
	cf, err := e.db.TransportByName(ctx, "cloudflare")
	if err != nil {
		t.Fatal(err)
	}
	if cf.ConfiguredAt == nil || cf.Host != "smtp.mx.cloudflare.net" || cf.Port != 465 || cf.Auth != "plain" ||
		cf.Username != "user" || len(cf.Caps) != 0 || cf.Revision != 2 {
		t.Errorf("cloudflare %+v", cf)
	}
	if got := e.opened("cloudflare"); got != pw {
		t.Errorf("cloudflare opens to %q", got)
	}
	relay, err := e.db.TransportByName(ctx, "relay")
	if err != nil {
		t.Fatal(err)
	}
	if relay.ConfiguredAt == nil || relay.Port != 2465 || relay.Auth != "none" || relay.SecretID != "" ||
		!slices.Equal(relay.Caps, []string{"message-id", "return-path"}) {
		t.Errorf("relay %+v", relay)
	}
	if done, _ := e.db.TransportByName(ctx, "done"); done.Host != "elsewhere.example.com" {
		t.Errorf("the environment overwrote done: %+v", done)
	}
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM actions WHERE tool = 'config.import'`).Scan(&n); err != nil || n != 2 {
		t.Errorf("%d config.import actions (%v), want 2", n, err)
	}
	actor, cred, args := e.lastAction("config.import")
	if actor != store.SystemPrincipalID || cred != nil || args["via"] != "env" || args["host_user"] != nil {
		t.Errorf("config.import: %s %v %v", actor, cred, args)
	}
	if !strings.Contains(logs.String(), `"transport is configured in the database; the environment's is ignored: remove YUUKA_TRANSPORT_DONE_*"`) {
		t.Errorf("no warning for done:\n%s", logs.String())
	}

	// Again: nothing more is written; the environment is now ignored.
	importOnce()
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM actions WHERE tool = 'config.import'`).Scan(&n); err != nil || n != 2 {
		t.Errorf("%d config.import actions after a restart (%v)", n, err)
	}
	if again, _ := e.db.TransportByName(ctx, "cloudflare"); again.Revision != cf.Revision {
		t.Errorf("rewritten on restart: %+v", again)
	}
	// A transport removed in the database stays removed.
	e.mustIn(nil, "transport", "rm", "relay")
	var later syncBuffer
	log = slog.New(slog.NewTextHandler(&later, nil))
	importOnce()
	if tr, _ := e.db.TransportByName(ctx, "relay"); tr.DisabledAt == nil {
		t.Errorf("the environment brought relay back: %+v", tr)
	}
	if !strings.Contains(later.String(), "removed in the database") {
		t.Errorf("no warning for relay:\n%s", later.String())
	}
	e.noPlaintext(pw)
}
