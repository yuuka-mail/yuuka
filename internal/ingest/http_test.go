package ingest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/contract"
	"github.com/yuuka-mail/yuuka/internal/store"
)

type stubIngester struct {
	err    error
	calls  int
	ctxErr error // the context's state when Ingest was called
}

func (s *stubIngester) Ingest(ctx context.Context, _ contract.Marker) (store.Outcome, error) {
	s.calls++
	s.ctxErr = ctx.Err()
	return store.Delivered, s.err
}

func TestHandlerStatusCodes(t *testing.T) {
	t.Parallel()
	secret := []byte("webhook-test-secret-0123456789abcdef")
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	at := now.Add(-time.Second)
	id := "01M67SX66B38NKRKAYDXR834N3"
	valid := contract.Marshal(contract.Marker{
		ID: id, ReceivedAt: at, EnvelopeFrom: "s@example.net", EnvelopeTo: "a@example.com",
		RawKey: contract.RawKey(at, id), RawSize: 3,
	})

	type req struct {
		method string
		body   []byte
		ts     int64
		sig    string // "" means sign correctly
	}
	cases := []struct {
		name      string
		req       req
		ingestErr error
		want      int
		ingested  bool
	}{
		{"ok", req{http.MethodPost, valid, now.Unix(), ""}, nil, http.StatusNoContent, true},
		{"wrong method", req{http.MethodGet, nil, now.Unix(), ""}, nil, http.StatusMethodNotAllowed, false},
		{"bad signature", req{http.MethodPost, valid, now.Unix(), "v1=00"}, nil, http.StatusUnauthorized, false},
		{"stale timestamp", req{http.MethodPost, valid, now.Unix() - 301, ""}, nil, http.StatusUnauthorized, false},
		{"signed but invalid marker", req{http.MethodPost, []byte(`{"v":2}`), now.Unix(), ""}, nil, http.StatusBadRequest, false},
		{"too large", req{http.MethodPost, []byte(strings.Repeat(" ", maxWebhookBody+1)), now.Unix(), ""}, nil, http.StatusRequestEntityTooLarge, false},
		{"ingest fails", req{http.MethodPost, valid, now.Unix(), ""}, errors.New("db down"), http.StatusInternalServerError, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubIngester{err: tc.ingestErr}
			h := &Handler{
				Secrets:  [][]byte{[]byte("old-secret-0123456789abcdef0123456789"), secret},
				Ingester: stub,
				Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
				Now:      func() time.Time { return now },
			}
			r := httptest.NewRequestWithContext(t.Context(), tc.req.method, "/ingest/cloudflare", strings.NewReader(string(tc.req.body)))
			sig := tc.req.sig
			if sig == "" {
				sig = contract.Sign(secret, tc.req.ts, tc.req.body)
			}
			r.Header.Set(contract.HeaderTimestamp, strconv.FormatInt(tc.req.ts, 10))
			r.Header.Set(contract.HeaderSignature, sig)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Errorf("status %d, want %d", w.Code, tc.want)
			}
			if (stub.calls > 0) != tc.ingested {
				t.Errorf("ingest called %d times", stub.calls)
			}
		})
	}
}

// The Worker gives up on the webhook after 5 s; that must not cancel an
// ingest that is already running.
func TestHandlerIngestOutlivesCaller(t *testing.T) {
	t.Parallel()
	secret := []byte("webhook-test-secret-0123456789abcdef")
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	id := "01M67SX66B38NKRKAYDXR834N3"
	body := contract.Marshal(contract.Marker{
		ID: id, ReceivedAt: now, EnvelopeTo: "a@example.com", RawKey: contract.RawKey(now, id), RawSize: 3,
	})
	stub := &stubIngester{}
	h := &Handler{
		Secrets: [][]byte{secret}, Ingester: stub, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return now },
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // the caller is already gone
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/ingest/cloudflare", strings.NewReader(string(body)))
	r.Header.Set(contract.HeaderTimestamp, strconv.FormatInt(now.Unix(), 10))
	r.Header.Set(contract.HeaderSignature, contract.Sign(secret, now.Unix(), body))
	h.ServeHTTP(httptest.NewRecorder(), r)

	if stub.calls != 1 || stub.ctxErr != nil {
		t.Fatalf("ingest calls=%d ctxErr=%v; want one call with a live context", stub.calls, stub.ctxErr)
	}
}

// The webhook against a real Ingester and Postgres.
func TestHandlerEndToEnd(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	secret := []byte("webhook-test-secret-0123456789abcdef")
	m := e.deliver(t, e.now, "s@example.net", "a@example.com", message("a@example.com", "tx1"))
	body := contract.Marshal(m)
	h := &Handler{
		Secrets: [][]byte{secret}, Ingester: e.in, Now: func() time.Time { return e.now },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	for range 2 { // a replay inside the window is an idempotent no-op
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/ingest/cloudflare", strings.NewReader(string(body)))
		r.Header.Set(contract.HeaderTimestamp, strconv.FormatInt(e.now.Unix(), 10))
		r.Header.Set(contract.HeaderSignature, contract.Sign(secret, e.now.Unix(), body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNoContent {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
	}
	e.in.Wait()
	if n := e.count(t, `SELECT count(*) FROM deliveries`); n != 1 {
		t.Fatalf("%d deliveries", n)
	}
}
