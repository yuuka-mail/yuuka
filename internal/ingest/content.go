package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// fold runs the content step for one committed delivery: parse, extract
// the trusted auth facts, and fold into messages (§4.1).
func (in *Ingester) fold(ctx context.Context, ref store.RawRef, raw []byte) (store.Folded, error) {
	msg := mimeparse.Parse(raw)
	f, err := in.cfg.DB.Fold(ctx, ref, msg, authres.Extract(raw, in.cfg.TrustedAuthservID))
	if err == nil && f.NewMessage && msg.Problem != "" {
		// The problem text may quote header bytes; the id is enough.
		in.cfg.Log.Warn("quarantined: MIME could not be parsed cleanly", "raw", ref.RawID, "message", f.MessageID)
	}
	return f, err
}

// ReadRaw returns the raw message of ref, preferring the local copy and
// falling back to the raw bucket. Readers such as the agent view use it too.
func (in *Ingester) ReadRaw(ctx context.Context, ref store.RawRef) ([]byte, error) {
	b, err := in.cfg.Local.Get(ref.LocalPath)
	if err == nil {
		return b, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	obj, err := in.cfg.Raw.Get(ctx, ref.R2Key)
	if err != nil {
		return nil, fmt.Errorf("raw %s: %w", ref.RawID, err)
	}
	return obj.Body, nil
}

// backlogBatch bounds one query of the backlog pass.
const backlogBatch = 200

// BacklogStats counts what one backlog pass did.
type BacklogStats struct {
	Processed int
	Failed    int
}

// ProcessBacklog runs the content step for deliveries that have no
// message: everything M0 committed before parsing existed, and deliveries
// whose content step failed after the commit point. It never notifies:
// the mail is not new to anyone by now, and an upgrade must not replay the
// whole M0 history into the IM channel.
func (in *Ingester) ProcessBacklog(ctx context.Context) (BacklogStats, error) {
	var st BacklogStats
	failed := map[string]bool{}
	for {
		refs, err := in.cfg.DB.Unprocessed(ctx, backlogBatch+len(failed))
		if err != nil {
			return st, err
		}
		progress := false
		for _, ref := range refs {
			if failed[ref.DeliveryID] {
				continue
			}
			if err := in.processOne(ctx, ref); err != nil {
				in.cfg.Log.Error("backlog: content step", "delivery", ref.DeliveryID, "err", err)
				failed[ref.DeliveryID] = true
				st.Failed++
				continue
			}
			st.Processed++
			progress = true
		}
		if !progress || len(refs) < backlogBatch+len(failed) {
			return st, nil
		}
	}
}

func (in *Ingester) processOne(ctx context.Context, ref store.RawRef) error {
	raw, err := in.ReadRaw(ctx, ref)
	if err != nil {
		return err
	}
	_, err = in.fold(ctx, ref, raw)
	return err
}

// ReparseStats counts what Reparse did.
type ReparseStats struct {
	Backlog  BacklogStats
	Messages int // messages rebuilt
	Changed  int // of which something changed
	Failed   int
	Threads  store.RethreadStats
}

// reparseBatch is the page size of the reparse scan.
const reparseBatch = 200

// Reparse rebuilds the derived state of every message received at or
// after since (§4.2): it re-reads each delivery's raw, re-parses, and
// rewrites the messages columns and deliveries.auth in place, keeping
// message ids. Threads are then rebuilt globally, because a changed
// Message-ID or References can move messages anywhere. Decisions
// (message_state, prefix claims, ...) and delivery facts are never
// written.
func (in *Ingester) Reparse(ctx context.Context, since time.Time) (ReparseStats, error) {
	var st ReparseStats
	var err error
	// Deliveries without a message first, so every delivery is covered.
	if st.Backlog, err = in.ProcessBacklog(ctx); err != nil {
		return st, err
	}
	after := ""
	for {
		targets, err := in.cfg.DB.ReparseTargets(ctx, since, after, reparseBatch)
		if err != nil {
			return st, err
		}
		for _, t := range targets {
			st.Messages++
			changed, err := in.rebuild(ctx, t)
			if err != nil {
				in.cfg.Log.Error("reparse", "message", t.MessageID, "err", err)
				st.Failed++
				continue
			}
			if changed {
				st.Changed++
			}
		}
		if len(targets) < reparseBatch {
			break
		}
		after = targets[len(targets)-1].MessageID
	}
	if st.Threads, err = in.cfg.DB.Rethread(ctx); err != nil {
		return st, err
	}
	return st, nil
}

func (in *Ingester) rebuild(ctx context.Context, t store.ReparseTarget) (bool, error) {
	auth := make(map[string]authres.Auth, len(t.Deliveries))
	var msg mimeparse.Message
	for i, d := range t.Deliveries {
		raw, err := in.ReadRaw(ctx, d)
		if err != nil {
			return false, err
		}
		if i == 0 {
			// The earliest delivery's raw is the canonical one; the
			// content of the others is the same by the fold key.
			msg = mimeparse.Parse(raw)
		}
		auth[d.DeliveryID] = authres.Extract(raw, in.cfg.TrustedAuthservID)
	}
	return in.cfg.DB.Rebuild(ctx, t, msg, auth)
}
