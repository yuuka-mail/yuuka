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
// the trusted auth facts with the delivery's account's authserv-id, and
// fold into messages (§4.1). A new message is queued for scoring
// (NewMessage) when it came from push; pulled mail only when
// scorePulled, which the pull engine decides (§10.1.4): a backfill or
// the backlog pass must not queue a mailbox's history.
func (in *Ingester) fold(ctx context.Context, ref store.RawRef, raw []byte, scorePulled bool) (store.Folded, error) {
	ids, err := in.authservIDs(ctx)
	if err != nil {
		return store.Folded{}, err
	}
	authserv, pulled := ids.of(ref.AccountID)
	msg := mimeparse.Parse(raw)
	f, err := in.cfg.DB.Fold(ctx, ref, msg, authres.Extract(raw, authserv))
	if err == nil && f.NewMessage && msg.Problem != "" {
		// The problem text may quote header bytes; the id is enough.
		in.cfg.Log.Warn("quarantined: MIME could not be parsed cleanly", "raw", ref.RawID, "message", f.MessageID)
	}
	if err == nil && f.NewMessage {
		in.addrsCapped(msg, ref.RawID, f.MessageID)
		if in.cfg.NewMessage != nil && (!pulled || scorePulled) {
			in.cfg.NewMessage(f.MessageID)
		}
	}
	for _, h := range f.RuleFailures {
		// The rule did not match; what went wrong is about the rule,
		// never the mail's content.
		in.cfg.Log.Warn("rule failed closed", "rule", h.Rule, "message", f.MessageID, "err", h.Err)
	}
	return f, err
}

// FoldPulled runs the content step for a delivery the pull engine has
// just committed (§10.1.3). score queues a new message for scoring; the
// engine passes it only for new mail of an account the host lets be
// scored (§10.1.4). Whether to notify is the engine's too: Folded.Notify
// is the downgrade's answer, which it narrows.
func (in *Ingester) FoldPulled(ctx context.Context, ref store.RawRef, raw []byte, score bool) (store.Folded, error) {
	return in.fold(ctx, ref, raw, score)
}

// authservIDs is whose Authentication-Results each account trusts
// (§10.1.1): a pull account's own value, the host's for the rest.
type authservIDs struct {
	pulled map[string]string
	host   string
}

func (in *Ingester) authservIDs(ctx context.Context) (authservIDs, error) {
	m, err := in.cfg.DB.TrustedAuthservIDs(ctx)
	if err != nil {
		return authservIDs{}, err
	}
	return authservIDs{pulled: m, host: in.cfg.TrustedAuthservID}, nil
}

// of returns the authserv-id for a delivery of account, and whether the
// account is a pull account. Accounts without a source are Cloudflare
// accounts, which take the host's (§17.8).
func (a authservIDs) of(account string) (id string, pulled bool) {
	if id, ok := a.pulled[account]; ok {
		return id, true
	}
	return a.host, false
}

// ReadRaw returns the raw message of ref, preferring the local copy and
// falling back to the raw bucket. Readers such as the agent view use it
// too. It reads sent copies (sent/) as well as received raws.
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

// IndexSent runs the content step for the sent copy rawID (§4.6): it
// parses the raw an outbox row points at and folds it into messages like
// inbound mail, so our own mail threads and is searchable. There are no
// auth facts to extract: the bytes are our own. The outbox enqueuer calls
// it after the commit that created the row; if that fails or never runs,
// ProcessBacklog picks the sent copy up. It is idempotent and never
// notifies.
func (in *Ingester) IndexSent(ctx context.Context, rawID string) error {
	ref, err := in.cfg.DB.SentCopy(ctx, rawID)
	if err != nil {
		return err
	}
	return in.foldSent(ctx, ref)
}

func (in *Ingester) foldSent(ctx context.Context, ref store.RawRef) error {
	raw, err := in.ReadRaw(ctx, ref)
	if err != nil {
		return err
	}
	msg := mimeparse.Parse(raw)
	f, err := in.cfg.DB.FoldSent(ctx, ref, msg)
	if err == nil && f.NewMessage && msg.Problem != "" {
		// Our own composer should never produce this; an SMTP
		// submission (§13.6) might.
		in.cfg.Log.Warn("quarantined: sent copy could not be parsed cleanly", "raw", ref.RawID, "message", f.MessageID)
	}
	if err == nil && f.NewMessage {
		in.addrsCapped(msg, ref.RawID, f.MessageID)
	}
	return err
}

// addrsCapped logs, once per message (when it is created), that its To,
// Cc or Reply-To was cut short (mimeparse.AddrsCapped): fewer people are
// on its thread than the header lists.
func (in *Ingester) addrsCapped(msg mimeparse.Message, rawID, messageID string) {
	if msg.AddrsCapped {
		in.cfg.Log.Warn("address list cut short: too many addresses in To, Cc or Reply-To",
			"raw", rawID, "message", messageID)
	}
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
// whose content step failed after the commit point. It then does the same
// for sent copies that have no message (IndexSent failed or never ran),
// rebuilds the messages whose recipients, List-Id or recipient names
// were never read (written before migration 0007, 0010 or 0016), and
// re-extracts the messages
// an older extractor (or none, before migration 0008) went over, so
// neither an upgrade nor a better extractor needs a reparse. Rebuilding
// never runs rules (§8.5).
// It never notifies: the mail is not new to anyone by now, and an upgrade
// must not replay the whole M0 history into the IM channel.
func (in *Ingester) ProcessBacklog(ctx context.Context) (BacklogStats, error) {
	var st BacklogStats
	err := backlogPass(ctx, in, &st, "delivery", in.cfg.DB.Unprocessed,
		func(r store.RawRef) string { return r.DeliveryID }, in.processOne)
	if err != nil {
		return st, err
	}
	err = backlogPass(ctx, in, &st, "raw", in.cfg.DB.UnindexedSent,
		func(r store.RawRef) string { return r.RawID }, in.foldSent)
	if err != nil {
		return st, err
	}
	rebuild := func(ctx context.Context, t store.ReparseTarget) error {
		_, err := in.rebuild(ctx, t)
		return err
	}
	messageID := func(t store.ReparseTarget) string { return t.MessageID }
	if err := backlogPass(ctx, in, &st, "message", in.cfg.DB.UnreadRecipients, messageID, rebuild); err != nil {
		return st, err
	}
	if err := backlogPass(ctx, in, &st, "message", in.cfg.DB.UnreadListIDs, messageID, rebuild); err != nil {
		return st, err
	}
	if err := backlogPass(ctx, in, &st, "message", in.cfg.DB.UnreadNames, messageID, rebuild); err != nil {
		return st, err
	}
	return st, backlogPass(ctx, in, &st, "message", in.cfg.DB.UnextractedMessages, messageID,
		func(ctx context.Context, t store.ReparseTarget) error {
			raw, err := in.ReadRaw(ctx, t.Canonical())
			if err != nil {
				return err
			}
			_, err = in.cfg.DB.Reextract(ctx, t.MessageID, mimeparse.Parse(raw))
			return err
		})
}

// backlogPass processes what list returns until it returns nothing new.
// A failed item is logged under name and skipped for the rest of the pass;
// the next pass tries it again.
func backlogPass[T any](ctx context.Context, in *Ingester, st *BacklogStats, name string,
	list func(context.Context, int) ([]T, error), key func(T) string, process func(context.Context, T) error,
) error {
	failed := map[string]bool{}
	for {
		items, err := list(ctx, backlogBatch+len(failed))
		if err != nil {
			return err
		}
		progress := false
		for _, it := range items {
			if failed[key(it)] {
				continue
			}
			if err := process(ctx, it); err != nil {
				in.cfg.Log.Error("backlog: content step", name, key(it), "err", err)
				failed[key(it)] = true
				st.Failed++
				continue
			}
			st.Processed++
			progress = true
		}
		if !progress || len(items) < backlogBatch+len(failed) {
			return nil
		}
	}
}

func (in *Ingester) processOne(ctx context.Context, ref store.RawRef) error {
	raw, err := in.ReadRaw(ctx, ref)
	if err != nil {
		return err
	}
	_, err = in.fold(ctx, ref, raw, false)
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
// after since (§4.2): it re-reads each delivery's raw (and, for our own
// mail, the sent copy outbox.raw_id names), re-parses, and rewrites the
// messages columns and deliveries.auth in place, keeping message ids. Threads are then rebuilt globally, because a changed
// Message-ID or References can move messages anywhere. Decisions
// (message_state, prefix claims, ...) and delivery facts are never
// written.
func (in *Ingester) Reparse(ctx context.Context, since time.Time) (ReparseStats, error) {
	var st ReparseStats
	var err error
	// Deliveries and sent copies without a message first, so every one
	// is covered.
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
	ids, err := in.authservIDs(ctx)
	if err != nil {
		return false, err
	}
	auth := make(map[string]authres.Auth, len(t.Deliveries))
	canonical := t.Canonical()
	var msg mimeparse.Message
	parse := func(ref store.RawRef, raw []byte) {
		// The content of the other sources is the same by the fold key.
		if ref.RawID == canonical.RawID {
			msg = mimeparse.Parse(raw)
		}
	}
	for _, d := range t.Deliveries {
		raw, err := in.ReadRaw(ctx, d)
		if err != nil {
			return false, err
		}
		parse(d, raw)
		// Each delivery with its own account's receiver (§10.1.1): the
		// same content can arrive through Cloudflare and a pulled mailbox.
		authserv, _ := ids.of(d.AccountID)
		auth[d.DeliveryID] = authres.Extract(raw, authserv)
	}
	if canonical.DeliveryID == "" {
		// Sent copies carry no auth facts, so only the canonical one is
		// read.
		raw, err := in.ReadRaw(ctx, canonical)
		if err != nil {
			return false, err
		}
		parse(canonical, raw)
	}
	return in.cfg.DB.Rebuild(ctx, t, msg, auth)
}
