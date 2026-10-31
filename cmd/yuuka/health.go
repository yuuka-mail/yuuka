package main

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// GET /healthz is the one endpoint that authenticates nobody (§13.7): a
// reverse proxy's health check has no credential to present. It is safe
// to leave open only because it answers a single bit, so it must stay
// that way: a constant body, no version, no error text, no timings, no
// query over mail data and no writes.
const (
	// healthTimeout bounds one database ping.
	healthTimeout = 2 * time.Second
	// healthTTL is how long a result is reused. Together with the mutex
	// it bounds the probe to one ping in flight and about one per
	// second however often /healthz is requested, so it cannot be used
	// to tie up the connection pool.
	healthTTL = time.Second
)

type health struct {
	ping func(context.Context) error

	mu sync.Mutex
	at time.Time
	ok bool
}

func newHealth(ping func(context.Context) error) *health {
	return &health{ping: ping}
}

// check reports whether the database answered a ping recently. Callers
// arriving during a ping wait for its result rather than starting another.
func (h *health) check(ctx context.Context) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.at.IsZero() && time.Since(h.at) < healthTTL {
		return h.ok
	}
	// Detached from the request: a prober that hangs up must not leave
	// a cancelled ping behind as "down" for the next caller.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), healthTimeout)
	defer cancel()
	h.ok = h.ping(ctx) == nil
	h.at = time.Now()
	return h.ok
}

func (h *health) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !h.check(r.Context()) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("unavailable\n"))
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}
