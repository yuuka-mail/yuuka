// Package ingest is the Go side of the Cloudflare ingest commit protocol
// (§2.1): it turns a delivery marker plus its raw object in R2 into a local
// raw copy and one Postgres transaction, then removes the marker.
package ingest

import (
	"context"
	"crypto/sha256"
	"fmt"
	"iter"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/yuuka-mail/yuuka/internal/contenthash"
	"github.com/yuuka-mail/yuuka/internal/contract"
	"github.com/yuuka-mail/yuuka/internal/localraw"
	"github.com/yuuka-mail/yuuka/internal/notify"
	"github.com/yuuka-mail/yuuka/internal/r2"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// RawStore is read access to the raw bucket. A missing key is reported as
// an error wrapping r2.ErrNotFound.
type RawStore interface {
	Get(ctx context.Context, key string) (r2.Object, error)
	List(ctx context.Context, prefix string) iter.Seq2[string, error]
}

// MarkerStore is the pending bucket.
type MarkerStore interface {
	Get(ctx context.Context, key string) (r2.Object, error)
	List(ctx context.Context, prefix string) iter.Seq2[string, error]
	Delete(ctx context.Context, key string) error
}

// Notifier delivers a one-line summary somewhere a human will see it.
type Notifier interface {
	Notify(ctx context.Context, text string) error
}

// Config holds an Ingester's dependencies.
type Config struct {
	Raw     RawStore
	Pending MarkerStore
	Local   localraw.Dir
	DB      *store.DB
	// Account is the configured key of the Cloudflare account (M0 has no
	// accounts table; see the 0001 migration).
	Account string
	// TrustedAuthservID is the authserv-id of the account's trusted
	// receiver (§10.1.1); empty means no authentication facts are taken.
	TrustedAuthservID string
	// Notifier is optional.
	Notifier Notifier
	Log      *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// notifyConcurrency caps in-flight notifications. Beyond it summaries are
// dropped rather than queued: a burst of spam must not grow memory or
// delay ingest, and the mail itself is already committed.
const notifyConcurrency = 4

// notifyTimeout bounds one notification.
const notifyTimeout = 15 * time.Second

// Ingester commits markers. It is safe for concurrent use.
type Ingester struct {
	cfg       Config
	notifySem chan struct{}
	notifyWG  sync.WaitGroup
}

// New returns an Ingester.
func New(cfg Config) *Ingester {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Ingester{cfg: cfg, notifySem: make(chan struct{}, notifyConcurrency)}
}

// Ingest reads the raw object named by m and commits it. It is idempotent:
// a marker that was already committed returns store.AlreadyIngested. If
// the raw object does not exist the error wraps r2.ErrNotFound.
func (in *Ingester) Ingest(ctx context.Context, m contract.Marker) (store.Outcome, error) {
	obj, err := in.cfg.Raw.Get(ctx, m.RawKey)
	if err != nil {
		return 0, fmt.Errorf("ingest %s: %w", m.ID, err)
	}
	return in.commit(ctx, m, obj.Body)
}

func (in *Ingester) commit(ctx context.Context, m contract.Marker, body []byte) (store.Outcome, error) {
	if int64(len(body)) != m.RawSize {
		return 0, fmt.Errorf("ingest %s: raw is %d bytes, marker says %d", m.ID, len(body), m.RawSize)
	}
	// The local copy goes first: once the row exists, nothing would come
	// back to write it.
	if err := in.cfg.Local.Put(m.RawKey, body); err != nil {
		return 0, fmt.Errorf("ingest %s: %w", m.ID, err)
	}
	zone, prefix := SplitAddress(m.EnvelopeTo)
	rec := store.Ingest{
		RawID:        m.ID,
		R2Key:        m.RawKey,
		LocalPath:    m.RawKey,
		Size:         m.RawSize,
		SHA256:       sha256.Sum256(body),
		ContentHash:  contenthash.V1(body),
		ReceivedAt:   m.ReceivedAt,
		DeliveryID:   ulid.New(in.cfg.Now()),
		Account:      in.cfg.Account,
		Zone:         zone,
		Prefix:       prefix,
		EnvelopeFrom: m.EnvelopeFrom,
		EnvelopeTo:   m.EnvelopeTo,
	}
	out, err := in.cfg.DB.CommitIngest(ctx, rec)
	if err != nil {
		return 0, fmt.Errorf("ingest %s: %w", m.ID, err)
	}
	// Only after commit. A failed delete is harmless: the next reconcile
	// pass re-ingests (a no-op) and deletes again.
	if err := in.cfg.Pending.Delete(ctx, contract.PendingKey(m.ID)); err != nil {
		in.cfg.Log.Warn("delete pending marker", "id", m.ID, "err", err)
	}
	// Addresses and subjects stay out of the log on purpose; the id is
	// enough to find everything else.
	in.cfg.Log.Info("ingested", "id", m.ID, "outcome", out.String(), "size", m.RawSize)
	if out != store.Delivered {
		return out, nil
	}
	// The content step runs after the commit point, so the mail is safe
	// whatever happens here. If it fails, the delivery stays without a
	// message for the backlog pass, and nobody is notified: whether the
	// mail is downgraded (§4.3) could not be decided.
	f, err := in.fold(ctx, store.RawRef{
		DeliveryID: rec.DeliveryID, RawID: rec.RawID, R2Key: rec.R2Key, LocalPath: rec.LocalPath,
		ContentHash: rec.ContentHash[:], Zone: zone, Prefix: prefix, ReceivedAt: rec.ReceivedAt,
	}, body)
	if err != nil {
		in.cfg.Log.Error("content step failed; left for the backlog pass", "id", m.ID, "err", err)
		return out, nil
	}
	if f.Notify {
		in.notify(ctx, notify.Summary(zone, prefix, body))
	}
	return out, nil
}

// notify runs best effort in the background: it never blocks or fails
// the ingest that triggered it.
func (in *Ingester) notify(ctx context.Context, text string) {
	if in.cfg.Notifier == nil {
		return
	}
	select {
	case in.notifySem <- struct{}{}:
	default:
		in.cfg.Log.Warn("notification dropped: too many in flight")
		return
	}
	in.notifyWG.Go(func() {
		defer func() { <-in.notifySem }()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notifyTimeout)
		defer cancel()
		if err := in.cfg.Notifier.Notify(ctx, text); err != nil {
			in.cfg.Log.Warn("notify", "err", err)
		}
	})
}

// Wait blocks until in-flight notifications finish, for shutdown and tests.
func (in *Ingester) Wait() { in.notifyWG.Wait() }

// SplitAddress derives the catch-all dimensions from an envelope address:
// lowercased domain and local part, split at the last "@" (a quoted local
// part may contain "@"). The address itself is stored verbatim elsewhere.
// `yuuka prefix claim` derives its key the same way, so a claim matches
// the mail it is meant for.
func SplitAddress(addr string) (zone, prefix string) {
	i := strings.LastIndexByte(addr, '@')
	if i < 0 {
		return "", strings.ToLower(addr)
	}
	return strings.ToLower(addr[i+1:]), strings.ToLower(addr[:i])
}
