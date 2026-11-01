package pull

import (
	"context"
	"time"

	"github.com/yuuka-mail/yuuka/internal/notify"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// account.trouble (§8.4, §10.1.3): a person hears that a mailbox stopped
// or skipped mail, once per change and not once per round. What a person
// has to act on is told from the tables, so a push that did not get
// through or a restart changes nothing: a refused login and passed-over
// mail. What heals by itself is told by the worker, from memory: long
// failing rounds and a limit. A card carries no words of the server's.

// failuresEvery is the least time between two cards about passed-over
// mail: a folder of bad mail is one card a day, however long the walk.
const failuresEvery = 24 * time.Hour

// tellTimeout bounds one push; the bindings are tried one after another.
const tellTimeout = 2 * time.Minute

// Notifier sends account.trouble to who may read the account
// (notify.Dispatcher), and says how many it reached.
type Notifier interface {
	AccountTrouble(ctx context.Context, c notify.TroubleCard) (int, error)
}

// tell pushes c and reports whether that is settled: someone got it, or
// nobody is there to get it. Retrying after a partial failure would repeat
// the card to those who have it for as long as one chat stays broken, so
// only a push that reached nobody is worth repeating.
func (c *Config) tell(ctx context.Context, card notify.TroubleCard) bool {
	if c.Trouble == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, tellTimeout)
	defer cancel()
	n, err := c.Trouble.AccountTrouble(ctx, card)
	if err != nil && ctx.Err() == nil {
		c.Log.Warn("pull: account.trouble was not delivered everywhere", "account", card.Account, "kind", int(card.Kind), "reached", n, "err", err)
	}
	return err == nil || n > 0
}

// watchTrouble runs in its own goroutine so that a slow bot does not hold
// up starting and stopping the workers.
func (m *Manager) watchTrouble(ctx context.Context) {
	tick := time.NewTicker(m.cfg.TroublePoll)
	defer tick.Stop()
	for {
		m.tellTrouble(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// tellTrouble pushes what the tables say that nobody has been told. A
// card is recorded after it went out: a crash in between, or two serves
// reading the same row, repeats it, which beats a mailbox stopped with
// nobody told.
func (m *Manager) tellTrouble(ctx context.Context) {
	log := m.cfg.Log
	ts, err := m.cfg.DB.SourceTroubles(ctx, m.cfg.MaxAttempts)
	if err != nil {
		if ctx.Err() == nil {
			log.Error("pull: read account trouble", "err", err)
		}
		return
	}
	for _, t := range ts {
		if err := m.tellSource(ctx, t); err != nil {
			if ctx.Err() == nil {
				log.Error("pull: record account trouble", "account", t.AccountName, "err", err)
			}
			return
		}
	}
}

func (m *Manager) tellSource(ctx context.Context, t store.SourceTrouble) error {
	db := m.cfg.DB
	card := notify.TroubleCard{AccountID: t.AccountID, Account: t.AccountName}
	if t.Refused != nil {
		card.Kind = notify.TroubleRefused
		if m.cfg.tell(ctx, card) {
			if err := db.RecordRefusalTold(ctx, t.AccountID, *t.Refused); err != nil {
				return err
			}
		}
	}
	if t.Failures < t.FailuresTold {
		// A retry fetched some: what fails next counts as new.
		return db.RecordFailuresTold(ctx, t.AccountID, t.Failures, nil)
	}
	now := m.cfg.Now()
	if t.Failures == t.FailuresTold || t.FailuresToldAt != nil && now.Sub(*t.FailuresToldAt) < failuresEvery {
		return nil
	}
	card.Kind, card.New, card.Open = notify.TroubleFailures, t.Failures-t.FailuresTold, t.Failures
	if !m.cfg.tell(ctx, card) {
		return nil
	}
	return db.RecordFailuresTold(ctx, t.AccountID, t.Failures, &now)
}

// worked is a round that reached the server and did its work, or stopped
// on a limit: whatever it was failing on is over.
func (w *worker) worked() {
	w.backoff, w.failingSince, w.failingKind, w.stallTold = 0, time.Time{}, 0, false
}

// limited notes that a limit stopped pulling until tomorrow. By its
// nature that is one card a day at most; none for backfill's share of
// the budget, after which new mail still comes in.
func (w *worker) limited(k notify.TroubleKind) {
	w.worked()
	w.stopped = &notify.TroubleCard{AccountID: w.src.AccountID, Account: w.src.AccountName, Kind: k}
}

// tellRound pushes what the round just over found, outside it: a slow bot
// must not sit on a connection, the account's lock and a slot of the
// global limit on rounds.
func (w *worker) tellRound(ctx context.Context) {
	if c := w.stopped; c != nil {
		w.stopped = nil
		w.cfg.tell(ctx, *c)
	}
	if w.failingSince.IsZero() || w.stallTold {
		return
	}
	if d := w.cfg.Now().Sub(w.failingSince); d >= w.cfg.StallAfter {
		c := notify.TroubleCard{AccountID: w.src.AccountID, Account: w.src.AccountName, Kind: notify.TroubleStalled, For: d}
		if w.failingKind != 0 {
			c.Why = w.failingKind.String()
		}
		w.stallTold = w.cfg.tell(ctx, c)
	}
}
