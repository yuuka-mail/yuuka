package ingest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yuuka-mail/yuuka/internal/contract"
	"github.com/yuuka-mail/yuuka/internal/r2"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// OrphanGrace is how long a marker may wait for its raw object. The Worker
// gives its two writes a 60 s deadline, so a marker older than this without
// raw belongs to an invocation that failed and was retried under a new id.
const OrphanGrace = 15 * time.Minute

// ReconcileStats counts what one pass over pending/ did.
type ReconcileStats struct {
	Markers  int // markers seen
	Ingested int // committed now or found already committed
	Waiting  int // raw missing, still within OrphanGrace
	Orphaned int // raw missing past OrphanGrace; marker deleted
	Failed   int // left in place for the next pass
}

// Reconcile applies the marker table of §2.1 to every pending marker.
// Per-marker failures are logged and counted, never fatal; the error is
// only for failing to list.
func (in *Ingester) Reconcile(ctx context.Context) (ReconcileStats, error) {
	var st ReconcileStats
	log := in.cfg.Log
	for key, err := range in.cfg.Pending.List(ctx, "pending/") {
		if err != nil {
			return st, fmt.Errorf("reconcile: %w", err)
		}
		st.Markers++
		id, ok := contract.IDFromPendingKey(key)
		if !ok {
			// Not ours to delete; someone has to look at it.
			log.Error("reconcile: unexpected key in pending bucket", "key", key)
			st.Failed++
			continue
		}
		obj, err := in.cfg.Pending.Get(ctx, key)
		if errors.Is(err, r2.ErrNotFound) {
			st.Markers-- // committed by the webhook since the listing
			continue
		}
		if err != nil {
			log.Error("reconcile: read marker", "id", id, "err", err)
			st.Failed++
			continue
		}
		m, err := contract.Parse(obj.Body)
		if err == nil && m.ID != id {
			err = fmt.Errorf("marker id %s under key %s", m.ID, key)
		}
		if err != nil {
			log.Error("reconcile: bad marker", "id", id, "err", err)
			st.Failed++
			continue
		}
		_, err = in.Ingest(ctx, m)
		switch {
		case err == nil:
			st.Ingested++
		case errors.Is(err, r2.ErrNotFound):
			age := in.cfg.Now().Sub(m.ReceivedAt)
			if age < OrphanGrace {
				st.Waiting++
				continue
			}
			// The Worker failed at its raw write and threw, so the sender
			// retries under a new id: this marker has nothing to deliver.
			log.Warn("orphan_marker", "id", id, "age", age.Round(time.Second).String())
			if err := in.cfg.Pending.Delete(ctx, key); err != nil {
				log.Error("reconcile: delete orphan marker", "id", id, "err", err)
				st.Failed++
				continue
			}
			st.Orphaned++
		default:
			log.Error("reconcile: ingest", "id", id, "err", err)
			st.Failed++
		}
	}
	return st, nil
}

// ScanStats counts what a full raw scan did.
type ScanStats struct {
	RawObjects int // keys listed under raw/
	Missing    int // not in raw_objects
	Recovered  int // committed from metadata (or found to be byte duplicates)
	Failed     int
}

// FullScan is the last row of the §2.1 table: every raw object must have a
// raw_objects row. Objects without one are ingested with the envelope
// recovered from their yuuka-marker metadata.
func (in *Ingester) FullScan(ctx context.Context) (ScanStats, error) {
	var st ScanStats
	log := in.cfg.Log
	known, err := in.cfg.DB.RawKeys(ctx)
	if err != nil {
		return st, fmt.Errorf("full scan: %w", err)
	}
	for key, err := range in.cfg.Raw.List(ctx, "raw/") {
		if err != nil {
			return st, fmt.Errorf("full scan: %w", err)
		}
		st.RawObjects++
		if _, ok := known[key]; ok {
			continue
		}
		st.Missing++
		out, err := in.recover(ctx, key)
		if err != nil {
			log.Error("full scan: recover", "key", key, "err", err)
			st.Failed++
			continue
		}
		log.Warn("full scan: recovered raw without row", "key", key, "outcome", out.String())
		st.Recovered++
	}
	return st, nil
}

func (in *Ingester) recover(ctx context.Context, key string) (store.Outcome, error) {
	obj, err := in.cfg.Raw.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	enc, ok := obj.Metadata[contract.MetadataKey]
	if !ok {
		return 0, fmt.Errorf("no %s metadata", contract.MetadataKey)
	}
	data, err := contract.DecodeMetadata(enc)
	if err != nil {
		return 0, err
	}
	m, err := contract.Parse(data)
	if err != nil {
		return 0, err
	}
	if m.RawKey != key {
		return 0, fmt.Errorf("metadata names raw_key %s", m.RawKey)
	}
	return in.commit(ctx, m, obj.Body)
}
