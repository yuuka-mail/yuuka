package ingest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/yuuka-mail/yuuka/internal/contract"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// maxWebhookBody bounds the request body. A marker is a few hundred bytes;
// the limit only has to be comfortably above that.
const maxWebhookBody = 64 << 10

// webhookIngestTimeout bounds an ingest started by the webhook. It is not
// tied to the request: the Worker stops waiting after 5 s because Cloudflare
// holds the sender's SMTP reply until the webhook returns (§2.2), and
// abandoning a nearly finished ingest would only push the mail to the next
// reconcile pass.
const webhookIngestTimeout = 2 * time.Minute

// markerIngester is what the webhook needs from an Ingester.
type markerIngester interface {
	Ingest(ctx context.Context, m contract.Marker) (store.Outcome, error)
}

// Handler is the webhook endpoint of §2.2. Status codes: 204 committed now
// or before; 4xx the request itself is wrong (the Worker only logs it);
// 5xx temporary, reconcile will pick the marker up.
type Handler struct {
	Secrets  [][]byte
	Ingester markerIngester
	Log      *slog.Logger
	// Now is the clock for the timestamp window; nil means time.Now.
	Now func() time.Time
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log := h.Log
	if log == nil {
		log = slog.Default()
	}
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	// Signature before parsing: unauthenticated bytes are never decoded.
	err = contract.Verify(h.Secrets, now(), r.Header.Get(contract.HeaderTimestamp), body, r.Header.Get(contract.HeaderSignature))
	if err != nil {
		log.Warn("webhook: rejected", "err", err)
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	m, err := contract.Parse(body)
	if err != nil {
		log.Warn("webhook: bad marker", "err", err)
		http.Error(w, "bad marker", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), webhookIngestTimeout)
	defer cancel()
	if _, err := h.Ingester.Ingest(ctx, m); err != nil {
		log.Error("webhook: ingest failed; left for reconcile", "id", m.ID, "err", err)
		http.Error(w, "ingest failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
