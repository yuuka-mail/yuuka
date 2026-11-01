package pull

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/yuuka-mail/yuuka/internal/contenthash"
	"github.com/yuuka-mail/yuuka/internal/imap"
	"github.com/yuuka-mail/yuuka/internal/notify"
	"github.com/yuuka-mail/yuuka/internal/r2"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// mode is why a message is being looked at; it decides the side effects
// (§10.1.4) and which share of the budget the fetch may use.
type mode int

const (
	// modeCatchUp: above the high cursor, mail that arrived since the
	// last round. The only mode that notifies or queues for scoring.
	modeCatchUp mode = iota
	// modeBackfill: below the low cursor, older mail, newest first.
	modeBackfill
	// modeReconcile: a gap the weekly reconciliation found.
	modeReconcile
	// modeRetry: an open failure, by `account sync --retry-failed`.
	modeRetry
)

// folder is one folder in one round, opened with EXAMINE.
type folder struct {
	r          *round
	wire, name string // the name as sent (modified UTF-7) and in UTF-8
	row        store.Mailbox
	top        uint32 // UIDNEXT-1 when the round opened it
	exists     uint32
	window     uint32 // UIDs per metadata fetch, adapted to how sparse they are
	batch      int    // messages committed since the cursor last moved
	batchBytes int64
}

// meta is what the metadata fetch says of one message.
type meta struct {
	uid    uint32
	size   int64 // RFC822.SIZE; -1 when not given
	date   time.Time
	draft  bool
	labels []string
	key    string
	// bad says why the message cannot be taken (no key, no date); it is
	// recorded as a failure.
	bad string
}

func (f *folder) loc(uid uint32) store.RemoteLocation {
	return store.RemoteLocation{Mailbox: f.name, UIDValidity: f.row.UIDValidity, UID: uid}
}

// items are the metadata fetch items for the folder's key kind (§10.1.3).
func (f *folder) items() []string {
	it := []string{"UID", "RFC822.SIZE", "INTERNALDATE", "FLAGS"}
	switch f.row.KeyKind {
	case store.KeyGmail:
		it = append(it, "X-GM-MSGID", "X-GM-LABELS")
	case store.KeyObjectID:
		it = append(it, "EMAILID")
	}
	return it
}

// metas fetches the metadata of the messages in set, ascending, and
// which of their keys the account already has: the one query that skips
// what is there before any body is fetched.
func (f *folder) metas(ctx context.Context, set string) ([]meta, map[string]store.PulledDelivery, error) {
	var out []meta
	err := f.r.c.UIDFetch(ctx, set, f.items(), 0, func(m *imap.Message) error {
		out = append(out, f.parse(m))
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	slices.SortFunc(out, func(a, b meta) int { return int(int64(a.uid) - int64(b.uid)) })
	out = slices.CompactFunc(out, func(a, b meta) bool { return a.uid == b.uid })
	keys := make([]string, 0, len(out))
	for _, m := range out {
		if m.key != "" {
			keys = append(keys, m.key)
		}
	}
	known, err := f.r.w.cfg.DB.KnownSourceKeys(ctx, f.r.w.src.AccountID, keys)
	return out, known, err
}

func (f *folder) parse(m *imap.Message) meta {
	uid, _ := m.UID()
	out := meta{uid: uid, size: -1}
	if n, ok := m.Size(); ok {
		out.size = n
	}
	if flags, ok := m.Flags(); ok {
		out.draft = hasFlag(flags, `\Draft`)
	}
	date, ok := m.InternalDate()
	if !ok {
		out.bad = "the server gave no INTERNALDATE"
	}
	out.date = date
	switch f.row.KeyKind {
	case store.KeyGmail:
		id, ok := m.GmailMsgID()
		if !ok {
			out.bad = "the server gave no X-GM-MSGID"
			return out
		}
		out.key = store.GmailSourceKey(id)
		out.labels, _ = m.GmailLabels()
	case store.KeyObjectID:
		id, _ := m.EmailID()
		key, err := store.ObjectIDSourceKey(id)
		if err != nil {
			out.bad = "the server gave no valid EMAILID"
			return out
		}
		out.key = key
	default:
		out.key = store.LocationSourceKey(f.row.UIDValidity, uid, f.name)
	}
	return out
}

// catchUp takes the mail from the high cursor up to the round's UIDNEXT-1
// (§10.1.2: never "*", which names the last message when nothing is
// new), moving the cursor after each batch. A message that fails short
// of MaxAttempts holds the cursor below it; the mail after it is still
// committed, so a new message is not delayed by a broken one, and is
// skipped by its key once the cursor can move.
func (f *folder) catchUp(ctx context.Context) error {
	blocked := false
	for a := uint64(f.row.High) + 1; a <= uint64(f.top); {
		if err := f.r.lock.Check(ctx); err != nil {
			return err
		}
		b := min(uint64(f.top), a+uint64(f.window)-1)
		ms, known, err := f.metas(ctx, fmt.Sprintf("%d:%d", a, b))
		if err != nil {
			return err
		}
		f.adapt(len(ms), b-a+1)
		cursor, drafts := f.row.High, 0
		for _, m := range ms {
			settled, err := f.handle(ctx, m, modeCatchUp, known)
			if err != nil {
				return errors.Join(err, f.advanceHigh(ctx, cursor, drafts))
			}
			blocked = blocked || !settled
			if !blocked {
				cursor = m.uid
				if m.draft {
					drafts++
				}
			}
			if f.full() {
				if err := f.advanceHigh(ctx, cursor, drafts); err != nil {
					return err
				}
				drafts = 0
			}
		}
		if !blocked {
			cursor = uint32(b)
		}
		if err := f.advanceHigh(ctx, cursor, drafts); err != nil {
			return err
		}
		a = b + 1
	}
	return nil
}

// backfill takes older mail below the low cursor, newest first, for at
// most BackfillSlice (§10.1.2, §10.1.4). A message that fails short of
// MaxAttempts stops it there until a later round.
func (f *folder) backfill(ctx context.Context) error {
	w := f.r.w
	if backfillDone(f.row) {
		return f.finishRescan(ctx)
	}
	if w.cfg.Now().Before(w.untilBackfill) {
		return nil
	}
	end := w.cfg.Now().Add(w.cfg.BackfillSlice)
	low := f.row.Low
	if low == 0 {
		low = f.row.High + 1
	}
	for low > 1 {
		if w.cfg.Now().After(end) {
			f.r.more = true
			return nil
		}
		if err := f.r.lock.Check(ctx); err != nil {
			return err
		}
		b := low - 1
		a := uint32(1)
		if b > f.window {
			a = b - f.window + 1
		}
		ms, known, err := f.metas(ctx, fmt.Sprintf("%d:%d", a, b))
		if err != nil {
			return err
		}
		f.adapt(len(ms), uint64(b-a)+1)
		cursor, drafts, blocked := low, 0, false
		for _, m := range slices.Backward(ms) {
			settled, err := f.handle(ctx, m, modeBackfill, known)
			if err != nil {
				err = errors.Join(err, f.advanceLow(ctx, cursor, drafts))
				if errors.Is(err, errBackfillBudget) {
					return f.stopBackfill(err)
				}
				return err
			}
			if !settled {
				blocked = true
				break
			}
			cursor = m.uid
			if m.draft {
				drafts++
			}
			if f.full() {
				if err := f.advanceLow(ctx, cursor, drafts); err != nil {
					return err
				}
				drafts = 0
			}
		}
		if !blocked {
			cursor = a
		}
		if err := f.advanceLow(ctx, cursor, drafts); err != nil {
			return err
		}
		if blocked {
			return nil
		}
		low = cursor
	}
	return f.finishRescan(ctx)
}

// stopBackfill parks backfill until tomorrow: its share of the budget is
// spent, catch-up goes on.
func (f *folder) stopBackfill(err error) error {
	if !errors.Is(err, errBackfillBudget) {
		return err
	}
	w := f.r.w
	if w.cfg.Now().After(w.untilBackfill) {
		w.log.Info("pull: backfill's share of the daily byte budget is used up; it goes on tomorrow (UTC)")
	}
	w.untilBackfill = nextDay(w.cfg.Now())
	return nil
}

// finishRescan records that backfill went over the whole folder after a
// UIDVALIDITY change, so whatever of the old generation it did not match
// keeps its old location.
func (f *folder) finishRescan(ctx context.Context) error {
	if f.row.RescanUIDValidity == 0 {
		return nil
	}
	if _, err := f.r.w.cfg.DB.FinishRescan(ctx, f.r.w.src.AccountID, f.name, f.row.RescanUIDValidity); err != nil {
		return err
	}
	f.row.RescanUIDValidity = 0
	return nil
}

// reconcile is the weekly check of the whole folder (§10.1.3): every UID
// the server has between the cursors that has neither a delivery nor an
// open failure is taken again. It guards against a cursor that passed
// mail because the server once said less than it had.
func (f *folder) reconcile(ctx context.Context) error {
	w := f.r.w
	if f.row.Low == 0 || f.row.High == 0 {
		return nil // nothing below the cursors is the round's yet
	}
	uids, err := f.r.c.UIDSearchAll(ctx)
	if err != nil {
		return err
	}
	if uint32(len(uids)) != f.exists { //nolint:gosec // a SEARCH result is bounded far below 2^32
		// Some servers answer SEARCH partially (§10.1.2): list them.
		uids = uids[:0]
		err := f.r.c.UIDFetch(ctx, "1:*", []string{"UID"}, 0, func(m *imap.Message) error {
			uid, _ := m.UID()
			uids = append(uids, uid)
			return nil
		})
		if err != nil {
			return err
		}
	}
	have, err := w.cfg.DB.PulledUIDs(ctx, w.src.AccountID, f.name, f.row.UIDValidity)
	if err != nil {
		return err
	}
	slices.Sort(uids)
	var missing []uint32
	for _, u := range uids {
		if u < f.row.Low || u > f.row.High {
			continue
		}
		if _, ok := slices.BinarySearch(have, u); ok {
			continue
		}
		if _, ok := f.r.failures[f.loc(u)]; ok {
			continue
		}
		missing = append(missing, u)
	}
	if len(missing) == 0 {
		return nil
	}
	// Drafts and mail older than backfill_since are here too: looked at
	// again, by their metadata only.
	w.log.Info("pull: reconciliation found UIDs without a delivery; looking at them again", "folder", f.name, "uids", len(missing))
	return f.passOver(ctx, missing, modeReconcile)
}

// retryable are the folder's open failures under its UIDVALIDITY.
func (f *folder) retryable() []uint32 {
	var out []uint32
	for loc := range f.r.failures {
		if loc.Mailbox == f.name && loc.UIDValidity == f.row.UIDValidity {
			out = append(out, loc.UID)
		}
	}
	slices.Sort(out)
	return out
}

// passOver handles the messages at uids without moving a cursor:
// reconciliation and retries.
func (f *folder) passOver(ctx context.Context, uids []uint32, m mode) error {
	for _, chunk := range chunks(uids, 200) {
		if err := f.r.lock.Check(ctx); err != nil {
			return err
		}
		ms, known, err := f.metas(ctx, uidSet(chunk))
		if err != nil {
			return err
		}
		for _, msg := range ms {
			if _, err := f.handle(ctx, msg, m, known); err != nil {
				return f.stopBackfill(err)
			}
		}
	}
	return nil
}

// adapt widens the metadata window over sparse UIDs (a mailbox where
// much was deleted) and narrows it back over dense ones.
func (f *folder) adapt(got int, span uint64) {
	switch {
	case uint64(got)*8 < span && f.window < 1<<20:
		f.window *= 2
	case uint64(got)*2 > span:
		f.window = f.r.w.cfg.Window
	}
}

// full: the batch since the cursor last moved is as large as a batch
// gets.
func (f *folder) full() bool {
	return f.batch >= f.r.w.cfg.BatchMessages || f.batchBytes >= f.r.w.cfg.BatchBytes
}

// advanceHigh moves the catch-up cursor to `to` and counts drafts
// passed, once everything up to it is committed or recorded.
func (f *folder) advanceHigh(ctx context.Context, to uint32, drafts int) error {
	f.batch, f.batchBytes = 0, 0
	if to <= f.row.High {
		return nil
	}
	moved, err := f.r.w.cfg.DB.AdvanceHigh(ctx, f.r.w.src.AccountID, f.name, f.row.UIDValidity, to, drafts)
	if err != nil {
		return err
	}
	if !moved {
		return errStale
	}
	f.row.High = to
	return nil
}

// advanceLow moves the backfill cursor down to `to`.
func (f *folder) advanceLow(ctx context.Context, to uint32, drafts int) error {
	f.batch, f.batchBytes = 0, 0
	low := f.row.Low
	if low == 0 {
		low = f.row.High + 1
	}
	if to >= low || to == 0 {
		return nil
	}
	moved, err := f.r.w.cfg.DB.AdvanceLow(ctx, f.r.w.src.AccountID, f.name, f.row.UIDValidity, to, drafts)
	if err != nil {
		return err
	}
	if !moved {
		return errStale
	}
	f.row.Low = to
	return nil
}

// handle settles one message: skipped (a draft, older than
// backfill_since, already there), committed, or failed. settled is false
// only for a failure the cursor may not pass yet. An error ends the
// round.
func (f *folder) handle(ctx context.Context, m meta, md mode, known map[string]store.PulledDelivery) (settled bool, err error) {
	w := f.r.w
	loc := f.loc(m.uid)
	switch {
	case m.bad != "":
		return f.fail(ctx, m, m.bad)
	case m.draft:
		// Every save of a draft is a new message on the server; none
		// was ever sent. Counted when the cursor passes it (§10.1).
		return true, nil
	case md != modeCatchUp && w.src.BackfillSince != nil && m.date.Before(*w.src.BackfillSince):
		return true, nil
	}
	if pd, ok := known[m.key]; ok {
		if !strings.HasPrefix(m.key, "loc:") && pd.Mailbox == f.name && pd.UIDValidity != f.row.UIDValidity {
			// A stable key after a UIDVALIDITY change: only the location
			// is rewritten (§10.1.3), no body is fetched.
			if _, err := w.cfg.DB.Relocate(ctx, w.src.AccountID, pd, store.PulledDelivery{SourceKey: m.key, RemoteLocation: loc}); err != nil {
				return false, err
			}
		}
		return true, f.resolve(ctx, loc)
	}
	if err := f.r.budget(ctx, md); err != nil {
		return false, err
	}
	body, omitted, err := f.fetch(ctx, m)
	switch kind(err) {
	case 0:
	case imap.KindRefused:
		// NO for this message; the connection is fine.
		return f.fail(ctx, m, err.Error())
	case imap.KindTooLarge, imap.KindProtocol:
		// The connection is gone, so the round ends; the message is
		// recorded first, so that it cannot hold the folder forever.
		if _, ferr := f.fail(ctx, m, err.Error()); ferr != nil {
			return false, errors.Join(err, ferr)
		}
		return false, err
	default:
		return false, err
	}
	if body == nil {
		return f.fail(ctx, m, "the server answered without the message")
	}
	if err := f.r.spent(ctx, len(body)); err != nil {
		return false, err
	}
	if ok, err := f.rematch(ctx, m, body); err != nil || ok {
		if err == nil {
			err = f.resolve(ctx, loc)
		}
		return ok, err
	}
	return f.commit(ctx, m, body, omitted, md)
}

// fetch takes the message, or only its header when RFC822.SIZE is over
// the source's limit (§10.1.2). The limit also bounds the literal: a
// server that understated the size by more than the client's smallest
// literal bound is cut off before the excess is read; one that
// understated it by less is asked for the header after all.
func (f *folder) fetch(ctx context.Context, m meta) ([]byte, store.Omitted, error) {
	limit := f.r.w.src.MaxMessageBytes
	if m.size <= limit {
		body, err := f.fetchItem(ctx, m.uid, "BODY.PEEK[]", "", limit)
		if err != nil || int64(len(body)) <= limit {
			return body, 0, err
		}
	}
	body, err := f.fetchItem(ctx, m.uid, "BODY.PEEK[HEADER]", "HEADER", limit)
	return body, store.OmittedTooLarge, err
}

func (f *folder) fetchItem(ctx context.Context, uid uint32, item, section string, limit int64) ([]byte, error) {
	var body []byte
	err := f.r.c.UIDFetch(ctx, fmt.Sprint(uid), []string{"UID", item}, limit, func(msg *imap.Message) error {
		if u, _ := msg.UID(); u == uid {
			body, _ = msg.Body(section)
		}
		return nil
	})
	return body, err
}

// fail records one more failed attempt at m; once there have been
// MaxAttempts the cursor may pass it, never before (§10.1.3).
func (f *folder) fail(ctx context.Context, m meta, detail string) (bool, error) {
	w := f.r.w
	loc := f.loc(m.uid)
	n, err := w.cfg.DB.RecordPullFailure(ctx, store.PullFailure{
		AccountID: w.src.AccountID, RemoteLocation: loc, Size: m.size, Detail: detail, LastAt: w.cfg.Now(),
	})
	if err != nil {
		return false, err
	}
	f.r.failures[loc] = n
	passed := n >= w.cfg.MaxAttempts
	// The detail may quote the server; it stays in the table.
	w.log.Warn("pull: a message could not be fetched", "folder", f.name, "uid", m.uid, "attempts", n, "passed", passed)
	return passed, nil
}

// resolve closes the open failure at loc, if any: the message is there
// after all.
func (f *folder) resolve(ctx context.Context, loc store.RemoteLocation) error {
	if _, ok := f.r.failures[loc]; !ok {
		return nil
	}
	delete(f.r.failures, loc)
	return f.r.w.cfg.DB.ResolvePullFailure(ctx, f.r.w.src.AccountID, loc, f.r.w.cfg.Now())
}

// rematch is the location-keyed rescan after a UIDVALIDITY change
// (§10.1.3): a delivery of the old generation with the same bytes gets
// this location and key in place, and the bytes are dropped rather than
// stored twice.
func (f *folder) rematch(ctx context.Context, m meta, body []byte) (bool, error) {
	if f.row.KeyKind != store.KeyLocation || f.row.RescanUIDValidity == 0 {
		return false, nil
	}
	w := f.r.w
	old, err := w.cfg.DB.RescanMatches(ctx, w.src.AccountID, f.name, f.row.RescanUIDValidity, sha256.Sum256(body))
	if err != nil {
		return false, err
	}
	for _, pd := range old {
		moved, err := w.cfg.DB.Relocate(ctx, w.src.AccountID, pd, store.PulledDelivery{SourceKey: m.key, RemoteLocation: f.loc(m.uid)})
		switch {
		case errors.Is(err, store.ErrDuplicate):
			return true, nil // the new key is there already
		case err != nil:
			return false, err
		case moved:
			return true, nil
		}
	}
	return false, nil
}

// commit stores and commits one message in the order of §10.1.3: the
// local copy, the raw bucket, then the transaction; the content step
// after it.
func (f *folder) commit(ctx context.Context, m meta, body []byte, omitted store.Omitted, md mode) (bool, error) {
	w := f.r.w
	now := w.cfg.Now()
	id := ulid.New(now)
	key := r2.PullKey(now, id)
	loc := f.loc(m.uid)
	pm := r2.PullMeta{Account: w.src.AccountName, SourceKey: m.key, Mailbox: f.name, UIDValidity: loc.UIDValidity, UID: m.uid, InternalDate: m.date}
	if _, err := pm.Encode(); err != nil {
		// The object must describe itself (§2.2); one that cannot is
		// a message that cannot be taken, and recorded as such.
		return f.fail(ctx, m, err.Error())
	}
	if err := w.cfg.Local.Put(key, body); err != nil {
		return false, err
	}
	if err := w.cfg.Raw.PutPull(ctx, key, body, pm); err != nil {
		return false, err
	}
	p := store.Pull{
		RawID: id, R2Key: key, LocalPath: key, Size: int64(len(body)), SHA256: sha256.Sum256(body),
		Omitted: omitted, InternalDate: m.date, DeliveryID: ulid.New(now), AccountID: w.src.AccountID,
		SourceKey: m.key, RemoteLocation: loc, Labels: m.labels,
	}
	var ch []byte
	if omitted == 0 {
		h := contenthash.V1(body)
		p.ContentHash, ch = &h, h[:]
	}
	out, err := w.cfg.DB.CommitPull(ctx, p)
	if err != nil {
		return false, err
	}
	f.batch++
	f.batchBytes += int64(len(body))
	if err := f.resolve(ctx, loc); err != nil {
		return false, err
	}
	if out == store.Delivered {
		f.content(ctx, m, p, ch, body, md)
	}
	return true, nil
}

// content runs the content step of a committed delivery and decides its
// side effects (§10.1.4): rules always (in the fold); scoring and a
// notification only for mail that arrived since the last round, and a
// notification only if it arrived within a day, is not Gmail's \Sent and
// is not mail we sent. Backfill never notifies: a first sync must not
// replay a mailbox's history into the IM channel.
func (f *folder) content(ctx context.Context, m meta, p store.Pull, ch, body []byte, md mode) {
	w := f.r.w
	live := md == modeCatchUp
	ref := store.RawRef{
		DeliveryID: p.DeliveryID, RawID: p.RawID, R2Key: p.R2Key, LocalPath: p.LocalPath, ContentHash: ch,
		Zone: w.src.Zone(), Prefix: w.src.Prefix(), ReceivedAt: p.InternalDate, AccountID: p.AccountID,
	}
	fd, err := w.cfg.Content.FoldPulled(ctx, ref, body, live && w.scored)
	if err != nil {
		w.log.Error("pull: content step failed; left for the backlog pass", "delivery", p.DeliveryID, "err", err)
		return
	}
	if !live || !fd.Notify || pastDay(w.cfg.Now(), m.date) || hasFlag(m.labels, `\Sent`) {
		return
	}
	if own, err := w.cfg.DB.FromOutbox(ctx, fd.MessageID); err != nil || own {
		return
	}
	w.cfg.Content.Notify(ctx, notify.Mail{
		MessageID: fd.MessageID, DeliveryID: p.DeliveryID, Text: notify.Summary(ref.Zone, ref.Prefix, body), Silent: fd.Silent,
	})
}
