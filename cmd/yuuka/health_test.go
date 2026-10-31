package main

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/ingest"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
)

// testApp is the serve wiring over pool, without R2 or a listener.
func testApp(t *testing.T, pool *pgxpool.Pool, origin string) *app {
	t.Helper()
	db := store.New(pool)
	return &app{
		cfg:  config{Web: webConfig{Origin: origin}, WebhookSecrets: [][]byte{[]byte(strings.Repeat("s", minSecretLen))}},
		pool: pool,
		db:   db,
		in:   ingest.New(ingest.Config{DB: db}),
	}
}

func testMux(t *testing.T, a *app) *http.ServeMux {
	t.Helper()
	rs, err := a.routes(slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return newMux(rs)
}

func get(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), method, path, nil))
	return w
}

// assertHealthResponse checks that a /healthz answer carries the status
// and nothing else: a constant body and only the headers health.go sets.
func assertHealthResponse(t *testing.T, w *httptest.ResponseRecorder, code int, body string) {
	t.Helper()
	if w.Code != code || w.Body.String() != body {
		t.Errorf("got %d %q, want %d %q", w.Code, w.Body.String(), code, body)
	}
	want := []string{"Cache-Control", "Content-Type", "X-Content-Type-Options"}
	if got := slices.Sorted(maps.Keys(w.Header())); !slices.Equal(got, want) {
		t.Errorf("headers %v, want only %v", got, want)
	}
}

func TestHealthzUp(t *testing.T) {
	t.Parallel()
	_, pool := storetest.New(t)
	for _, origin := range []string{"", "https://mail.example.com"} {
		mux := testMux(t, testApp(t, pool, origin))
		// No credential of any kind.
		assertHealthResponse(t, get(t, mux, http.MethodGet, "/healthz"), http.StatusOK, "ok\n")
		if w := get(t, mux, http.MethodHead, "/healthz"); w.Code != http.StatusOK {
			t.Errorf("HEAD: %d", w.Code)
		}
	}
	// Other methods never reach the probe. (With the WebUI on they fall
	// through to the page shell like any unknown path.)
	if w := get(t, testMux(t, testApp(t, pool, "")), http.MethodPost, "/healthz"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", w.Code)
	}
}

func TestHealthzDatabaseDown(t *testing.T) {
	t.Parallel()
	// Nothing listens on port 1; the pool connects lazily, so only the
	// ping finds out.
	pool, err := pgxpool.New(t.Context(), "postgres://yuuka:secret-password@127.0.0.1:1/yuuka?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	w := get(t, testMux(t, testApp(t, pool, "")), http.MethodGet, "/healthz")
	assertHealthResponse(t, w, http.StatusServiceUnavailable, "unavailable\n")

	// A closed pool, as during shutdown.
	_, live := storetest.New(t)
	a := testApp(t, live, "")
	mux := testMux(t, a)
	live.Close()
	assertHealthResponse(t, get(t, mux, http.MethodGet, "/healthz"), http.StatusServiceUnavailable, "unavailable\n")
}

// However many probes arrive, at most one ping runs at a time and a
// result is reused for healthTTL.
func TestHealthzIsBounded(t *testing.T) {
	t.Parallel()
	var pings, inFlight, maxInFlight atomic.Int32
	h := newHealth(func(context.Context) error {
		pings.Add(1)
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		return errors.New("down")
	})
	done := make(chan struct{})
	for range 50 {
		go func() {
			defer func() { done <- struct{}{} }()
			assertHealthResponse(t, get(t, h, http.MethodGet, "/healthz"), http.StatusServiceUnavailable, "unavailable\n")
		}()
	}
	for range 50 {
		<-done
	}
	if pings.Load() != 1 || maxInFlight.Load() != 1 {
		t.Errorf("%d pings, %d at once; want 1 and 1", pings.Load(), maxInFlight.Load())
	}
}

// TestRouteInventory lists every endpoint serve exposes and how it
// authenticates (§13.7). Three are open: the WebUI page shell, which
// holds no data, /healthz, which answers one bit, and the API's contract
// (§5.1), which is public like the repository. Anything else that turns
// up without authentication fails here.
func TestRouteInventory(t *testing.T) {
	t.Parallel()
	_, pool := storetest.New(t)
	always := map[string]string{
		"GET /healthz":            authNoHealth,
		"POST /ingest/cloudflare": authHMAC,
		"/mcp":                    authBearer,
	}
	withWeb := maps.Clone(always)
	maps.Copy(withWeb, map[string]string{
		"/auth/": authPasskey,
		"/api/":  authSession,
		"/":      authNoShell,
	})
	for origin, want := range map[string]map[string]string{"": always, "https://mail.example.com": withWeb} {
		rs, err := testApp(t, pool, origin).routes(slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, r := range rs {
			got[r.pattern] = r.auth
		}
		if !maps.Equal(got, want) {
			t.Errorf("origin %q: routes %v, want %v", origin, got, want)
		}
	}

	// Anonymous requests: only the open routes answer.
	mux := testMux(t, testApp(t, pool, "https://mail.example.com"))
	for _, c := range []struct {
		method, path string
		code         int
	}{
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodPost, "/ingest/cloudflare", http.StatusUnauthorized},
		{http.MethodPost, "/mcp", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/messages", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/openapi.json", http.StatusOK},
		{http.MethodGet, "/auth/session", http.StatusUnauthorized},
	} {
		if w := get(t, mux, c.method, c.path); w.Code != c.code {
			t.Errorf("anonymous %s %s: %d, want %d", c.method, c.path, w.Code, c.code)
		}
	}
}
