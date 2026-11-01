// Package pull is the sync engine that pulls external mailboxes over IMAP
// into the store (§9, §10.1.3). It runs inside serve: a Manager reads the
// enabled sources every Interval and runs one worker per source that is
// not paused, restarting it when the source or its secret moves to
// another revision, as the channel manager does (§17.5).
//
// The invariant everything here serves is §3.5 for pulling: a folder's
// cursor never passes a message that is neither committed nor recorded
// in pull_failures. A message is written to the local raw copy, then to
// the raw bucket under pull/ (§2.2), then committed (store.CommitPull),
// and only then may a cursor move past it (store.AdvanceHigh,
// AdvanceLow). A crash anywhere costs a refetch the next round, which
// the source key turns into a no-op.
//
// The engine only reads the remote mailbox (EXAMINE, BODY.PEEK) and
// never sends mail.
package pull

import (
	"context"
	"crypto/x509"
	"log/slog"
	"time"

	"github.com/yuuka-mail/yuuka/internal/localraw"
	"github.com/yuuka-mail/yuuka/internal/notify"
	"github.com/yuuka-mail/yuuka/internal/r2"
	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// RawBucket is where pulled raws go (r2.RawBucket).
type RawBucket interface {
	PutPull(ctx context.Context, key string, body []byte, meta r2.PullMeta) error
}

// Content is the content step after a pulled delivery's commit
// (ingest.Ingester): parse and fold with the account's authserv-id, and
// the bounded notifier.
type Content interface {
	FoldPulled(ctx context.Context, ref store.RawRef, raw []byte, score bool) (store.Folded, error)
	Notify(ctx context.Context, m notify.Mail)
}

// Config is what the engine needs. Zero tunables take the defaults.
type Config struct {
	DB    *store.DB
	Raw   RawBucket
	Local localraw.Dir
	// Sealer opens the sources' passwords; nil is the locked state
	// (§17.3), in which nothing is pulled.
	Sealer  *secret.Sealer
	Content Content
	// TriageAccounts are the accounts whose new mail is queued for
	// scoring, by name (triage.Config.Accounts); a pull account is never
	// scored unless named, so nil means none of them (§10.1.4).
	TriageAccounts []string
	Log            *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// RootCAs verifies the servers; nil is the system roots. For tests.
	RootCAs *x509.CertPool

	// Interval is how often the sources are read (15 s).
	Interval time.Duration
	// Rounds is how many rounds run at once over all sources (2). Each
	// holds a database connection for its lock, and it bounds the
	// connections to one server too (§10.1.3).
	Rounds int
	// Window is how many UIDs one metadata fetch covers (1000).
	Window uint32
	// BatchMessages and BatchBytes bound the mail committed between two
	// cursor moves (100, 20 MiB).
	BatchMessages int
	BatchBytes    int64
	// MaxAttempts is how often a message may fail before the cursor
	// passes it, recorded (5).
	MaxAttempts int
	// BackoffMin and BackoffMax bound the wait after a connection error
	// (1 min, 1 h).
	BackoffMin, BackoffMax time.Duration
	// ReconcileEvery is how often each folder is checked whole (7 days).
	ReconcileEvery time.Duration
	// BackfillSlice is how long a round backfills one folder before the
	// next round catches up again (2 min).
	BackfillSlice time.Duration
	// RequestPoll is how often a waiting worker looks for
	// `yuuka account sync` (5 s).
	RequestPoll time.Duration
}

func (c *Config) defaults() {
	set := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	set(&c.Interval, 15*time.Second)
	set(&c.BackoffMin, time.Minute)
	set(&c.BackoffMax, time.Hour)
	set(&c.ReconcileEvery, 7*24*time.Hour)
	set(&c.BackfillSlice, 2*time.Minute)
	set(&c.RequestPoll, 5*time.Second)
	if c.Rounds <= 0 {
		c.Rounds = 2
	}
	if c.Window == 0 {
		c.Window = 1000
	}
	if c.BatchMessages <= 0 {
		c.BatchMessages = 100
	}
	if c.BatchBytes <= 0 {
		c.BatchBytes = 20 << 20
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 5
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Manager runs the workers.
type Manager struct {
	cfg    Config
	rounds chan struct{}
}

// New returns a Manager; Run starts it.
func New(cfg Config) *Manager {
	cfg.defaults()
	return &Manager{cfg: cfg, rounds: make(chan struct{}, cfg.Rounds)}
}

// running is the worker of one source revision.
type running struct {
	rev, secretRev int64
	stop           context.CancelFunc
	done           chan struct{}
}

// Run manages the workers until ctx ends, then stops them all.
func (m *Manager) Run(ctx context.Context) {
	if m.cfg.Sealer == nil {
		m.locked(ctx)
		return
	}
	workers := map[string]*running{}
	refused := map[string]time.Time{} // last ERROR for a source paused on a refused login
	defer func() {
		for _, r := range workers {
			r.stop()
			<-r.done
		}
	}()
	tick := time.NewTicker(m.cfg.Interval)
	defer tick.Stop()
	for {
		if err := m.reconcile(ctx, workers, refused); err != nil && ctx.Err() == nil {
			m.cfg.Log.Error("pull: read sources", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// locked says once that no source is pulled for want of a key, when
// there is a source to pull; the remote keeps the mail, so this is a
// delay, never a loss (§17.3).
func (m *Manager) locked(ctx context.Context) {
	tick := time.NewTicker(m.cfg.Interval)
	defer tick.Stop()
	for {
		ss, err := m.cfg.DB.EnabledSources(ctx)
		if err == nil && len(ss) > 0 {
			m.cfg.Log.Error("pull: external mailboxes are locked: YUUKA_KEK_FILE is not set, so no password can be opened; "+
				"none is pulled until serve restarts with it", "accounts", len(ss))
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// reconcile starts, restarts and stops workers to match the table.
func (m *Manager) reconcile(ctx context.Context, workers map[string]*running, refused map[string]time.Time) error {
	ss, err := m.cfg.DB.EnabledSources(ctx)
	if err != nil {
		return err
	}
	now := m.cfg.Now()
	want := map[string]bool{}
	for _, s := range ss {
		if s.PausedAt != nil {
			// A refused login stays an ERROR once a day until a person
			// acts (§10.1.3): retrying would get the mailbox locked.
			if s.PauseReason == store.PauseAuthFailed && now.Sub(refused[s.AccountID]) >= 24*time.Hour {
				refused[s.AccountID] = now
				m.cfg.Log.Error("pull: account paused, its server refused the login; set a new password with "+
					"`yuuka account set --password-file` or `yuuka account resume`", "account", s.AccountName)
			}
			continue
		}
		delete(refused, s.AccountID)
		want[s.AccountID] = true
		r := workers[s.AccountID]
		if r != nil && r.rev == s.Revision && r.secretRev == s.Secret.Revision {
			continue
		}
		if r != nil {
			r.stop()
			<-r.done
		}
		wctx, stop := context.WithCancel(ctx)
		r = &running{rev: s.Revision, secretRev: s.Secret.Revision, stop: stop, done: make(chan struct{})}
		workers[s.AccountID] = r
		w := &worker{cfg: &m.cfg, rounds: m.rounds, src: s, log: m.cfg.Log.With("account", s.AccountName)}
		go func() {
			defer close(r.done)
			w.run(wctx)
		}()
		w.log.Info("pull: worker started", "revision", s.Revision)
	}
	for id, r := range workers {
		if !want[id] {
			r.stop()
			<-r.done
			delete(workers, id)
		}
	}
	return nil
}
