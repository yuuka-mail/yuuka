package pull

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yuuka-mail/yuuka/internal/imap"
	"github.com/yuuka-mail/yuuka/internal/manage"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// statusItems are what a round asks of each folder to tell whether it
// changed (§10.1.2).
var statusItems = []string{"MESSAGES", "UIDNEXT", "UIDVALIDITY"}

// errStale: a cursor did not move although nothing else should move it:
// another worker reset the folder, or the lock was lost. The round ends;
// the next one reads the folder again.
var errStale = errors.New("the folder's cursor moved under this round")

// round is one connection's pass over a source's folders (§10.1.3).
type round struct {
	w     *worker
	c     *imap.Client
	lock  *store.PullLock
	retry bool // `account sync --retry-failed`
	// failures are the open pull_failures of the account, by location,
	// with their attempts.
	failures map[store.RemoteLocation]int
	// used is the bytes fetched today, -1 until read.
	used int64
	// more: a folder has backfill left that this round stopped short of.
	more bool
}

func (r *round) run(ctx context.Context) (bool, error) {
	w := r.w
	r.used = -1
	if w.src.Preset == store.PresetGmail && !r.c.Caps().Has("X-GM-EXT-1") {
		// Without Gmail's ids every key would be a location key, fixed
		// for good on the first sync (§10.1).
		return false, errors.New("the server does not offer X-GM-EXT-1, which the Gmail preset needs")
	}
	boxes, err := r.c.List(ctx, imap.ListOptions{SpecialUse: true, Status: statusItems})
	if err != nil {
		return false, err
	}
	folders, missing := syncFolders(w.src.SourceConfig, boxes)
	if len(missing) > 0 {
		w.log.Warn("pull: folders to sync that the server does not list", "folders", missing)
	}
	rows, err := w.cfg.DB.SourceMailboxes(ctx, w.src.AccountID)
	if err != nil {
		return false, err
	}
	byName := map[string]store.Mailbox{}
	for _, m := range rows {
		byName[m.Name] = m
	}
	fails, err := w.cfg.DB.PullFailures(ctx, w.src.AccountID, false)
	if err != nil {
		return false, err
	}
	r.failures = map[store.RemoteLocation]int{}
	for _, f := range fails {
		r.failures[f.RemoteLocation] = f.Attempts
	}
	for _, box := range folders {
		if err := r.lock.Check(ctx); err != nil {
			return false, err
		}
		row, known := byName[box.DisplayName()]
		err := r.folder(ctx, box, row, known)
		if kind(err) == imap.KindRefused {
			// The server refused this folder (it went away between LIST
			// and EXAMINE, say); the others go on.
			w.log.Warn("pull: folder skipped this round", "folder", box.DisplayName(), "err", describe(err))
			continue
		}
		if err != nil {
			return false, fmt.Errorf("folder %q: %w", box.DisplayName(), err)
		}
	}
	return r.more, nil
}

// keyKind is the key kind of a folder seen for the first time (§10.1):
// fixed from then on, whatever the server announces later.
func (r *round) keyKind() store.KeyKind {
	switch {
	case r.w.src.Preset == store.PresetGmail:
		return store.KeyGmail
	case r.c.Caps().Has("OBJECTID"):
		return store.KeyObjectID
	}
	return store.KeyLocation
}

// folder syncs one folder: retries if asked, catch-up, the weekly
// reconciliation when due, then a slice of backfill.
func (r *round) folder(ctx context.Context, box imap.Mailbox, row store.Mailbox, known bool) error {
	w := r.w
	db := w.cfg.DB
	name := box.DisplayName()
	st := box.Status
	if st == nil {
		var err error
		if st, err = r.c.Status(ctx, box.Name, statusItems...); err != nil {
			return err
		}
	}
	now := w.cfg.Now()
	due := now.Sub(w.reconciled[name]) >= w.cfg.ReconcileEvery
	if known && !due && (!r.retry || !r.failing(name, row.UIDValidity)) && unchanged(row, st) &&
		(backfillDone(row) || now.Before(w.untilBackfill)) {
		return nil
	}
	sel, err := r.c.Examine(ctx, box.Name)
	if err != nil {
		return err
	}
	uidnext := sel.UIDNext
	if uidnext == 0 {
		uidnext = st.UIDNext
	}
	if sel.UIDValidity == 0 || uidnext == 0 {
		// UIDs mean nothing across sessions without them.
		return &imap.Error{Kind: imap.KindRefused, Op: "EXAMINE", Err: errors.New("no UIDVALIDITY or UIDNEXT")}
	}
	top := uidnext - 1
	switch {
	case !known:
		// The first sync: what is there is the backfill's, newest
		// first; catch-up starts at what arrives from now on.
		row, err = db.StartMailbox(ctx, w.src.AccountID, store.MailboxStart{
			Name: name, SpecialUse: manage.SpecialUse(box), KeyKind: r.keyKind(), UIDValidity: sel.UIDValidity, High: top,
		})
		if err != nil {
			return err
		}
		w.log.Info("pull: new folder", "folder", name, "key_kind", int(row.KeyKind), "messages", sel.Exists)
	case row.UIDValidity != sel.UIDValidity:
		// §10.1.3: start the folder over. Stable keys relocate their
		// deliveries as backfill walks it; location keys rescan it,
		// matching by sha256.
		w.log.Warn("pull: UIDVALIDITY changed; walking the folder again", "folder", name,
			"was", row.UIDValidity, "now", sel.UIDValidity, "key_kind", int(row.KeyKind))
		if _, err := db.ResetMailbox(ctx, w.src.AccountID, name, row.UIDValidity, sel.UIDValidity, top); err != nil {
			return err
		}
		if row, err = db.StartMailbox(ctx, w.src.AccountID, store.MailboxStart{Name: name, KeyKind: row.KeyKind, UIDValidity: sel.UIDValidity}); err != nil {
			return err
		}
		if row.UIDValidity != sel.UIDValidity {
			return errStale
		}
	}
	f := &folder{r: r, wire: box.Name, name: name, row: row, top: top, exists: sel.Exists, window: w.cfg.Window}
	if r.retry {
		if err := f.passOver(ctx, f.retryable(), modeRetry); err != nil {
			return err
		}
	}
	if err := f.catchUp(ctx); err != nil {
		return err
	}
	if due {
		w.reconciled[name] = now
		if err := f.reconcile(ctx); err != nil {
			return err
		}
	}
	if err := f.backfill(ctx); err != nil {
		return err
	}
	_, err = db.RecordMailboxStatus(ctx, w.src.AccountID, name, row.UIDValidity, uidnext, sel.Exists, w.cfg.Now())
	return err
}

// unchanged: the folder is as the last finished round left it
// (§10.1.2, the first level), and catch-up has nothing to do.
func unchanged(row store.Mailbox, st *imap.MailboxStatus) bool {
	return st.UIDValidity == row.UIDValidity && st.UIDNext != 0 && st.UIDNext == row.UIDNextSeen &&
		st.Messages == row.MessagesSeen && uint64(row.High)+1 >= uint64(st.UIDNext) && row.RescanUIDValidity == 0
}

// backfillDone: backfill reached the bottom, or there was nothing to
// backfill.
func backfillDone(row store.Mailbox) bool {
	return row.Low == 1 || row.Low == 0 && row.High == 0
}

// failing: the folder has open failures under uidvalidity.
func (r *round) failing(name string, uidvalidity uint32) bool {
	for loc := range r.failures {
		if loc.Mailbox == name && loc.UIDValidity == uidvalidity {
			return true
		}
	}
	return false
}

// budget checks the daily byte budget before a fetch (§10.1.3).
// Backfill, reconciliation and retries may use nine tenths of it, so that
// a backfill that spends its day leaves room for the mail that arrives.
func (r *round) budget(ctx context.Context, m mode) error {
	b := r.w.src.DailyByteBudget
	if b == 0 {
		return nil
	}
	if r.used < 0 {
		used, err := r.w.cfg.DB.AddBudget(ctx, r.w.src.AccountID, r.w.cfg.Now(), 0)
		if err != nil {
			return err
		}
		r.used = used
	}
	switch {
	case m == modeCatchUp && r.used >= b:
		return errBudget
	case m != modeCatchUp && r.used >= b/10*9:
		return errBackfillBudget
	}
	return nil
}

// spent counts n fetched bytes against the budget.
func (r *round) spent(ctx context.Context, n int) error {
	used, err := r.w.cfg.DB.AddBudget(ctx, r.w.src.AccountID, r.w.cfg.Now(), int64(n))
	r.used = used
	return err
}

// uidSet writes ascending uids as an IMAP sequence set, "1:3,7,9:12".
func uidSet(uids []uint32) string {
	var b strings.Builder
	for i := 0; i < len(uids); {
		j := i
		for j+1 < len(uids) && uids[j+1] == uids[j]+1 {
			j++
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatUint(uint64(uids[i]), 10))
		if j > i {
			b.WriteByte(':')
			b.WriteString(strconv.FormatUint(uint64(uids[j]), 10))
		}
		i = j + 1
	}
	return b.String()
}

// chunks splits ascending uids so that no sequence set has more than n
// ranges, keeping each command line short.
func chunks(uids []uint32, n int) [][]uint32 {
	var out [][]uint32
	start, ranges := 0, 0
	for i := range uids {
		if i == 0 || uids[i] != uids[i-1]+1 {
			ranges++
			if ranges > n {
				out = append(out, uids[start:i])
				start, ranges = i, 1
			}
		}
	}
	if start < len(uids) {
		out = append(out, uids[start:])
	}
	return out
}

// hasFlag reports whether flags has f, ignoring case (RFC 3501 §2.3.2).
func hasFlag(flags []string, f string) bool {
	return slices.ContainsFunc(flags, func(s string) bool { return strings.EqualFold(s, f) })
}

// pastDay: the server says the message arrived more than a day before
// now; no notification for it (§10.1.4).
func pastDay(now, date time.Time) bool { return now.Sub(date) > 24*time.Hour }
