package pull

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/yuuka-mail/yuuka/internal/imap"
	"github.com/yuuka-mail/yuuka/internal/manage"
	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// worker pulls one source revision until it is stopped.
type worker struct {
	cfg    *Config
	rounds chan struct{}
	src    store.SourceWithSecret
	log    *slog.Logger

	password secret.Value
	// backoff is the wait after the last connection error, 0 after a
	// round that worked.
	backoff time.Duration
	// until: no round before it (a daily budget used up, or the
	// server's download limit); untilBackfill: no backfill before it
	// (the share of the budget backfill may use is spent).
	until, untilBackfill time.Time
	// reconciled is when each folder was last checked whole; empty
	// after a restart, like the full scan of raw/ (§2.1).
	reconciled map[string]time.Time
	// scored: this account's new mail is queued for scoring.
	scored bool
}

// errBudget: the day's budget is spent; errBackfillBudget: the share of
// it backfill may use is.
var (
	errBudget         = errors.New("the daily byte budget is used up")
	errBackfillBudget = errors.New("the daily byte budget left for backfill is used up")
)

func (w *worker) run(ctx context.Context) {
	if !w.open(ctx) {
		return
	}
	defer w.password.Close()
	w.reconciled = map[string]time.Time{}
	for _, a := range w.cfg.TriageAccounts {
		w.scored = w.scored || a == w.src.AccountName
	}
	retry := false
	for {
		wait := w.round(ctx, retry)
		if ctx.Err() != nil {
			return
		}
		req, ok := w.wait(ctx, wait)
		if !ok {
			return
		}
		retry = req.RetryFailed
	}
}

// open opens the source's password, recomputing its audience from the
// row (§17.2). A key that is missing, a secret that fails to
// authenticate or that was sealed for another destination does not get
// better by waiting: the worker says so and idles until the source or
// its secret changes. Anything else is retried.
func (w *worker) open(ctx context.Context) bool {
	aud, err := w.src.Audience()
	backoff := w.cfg.BackoffMin
	for err == nil {
		w.password, err = w.cfg.Sealer.Open(ctx, w.src.Secret.Record, aud)
		if err == nil {
			return true
		}
		if errors.Is(err, secret.ErrLocked) || errors.Is(err, secret.ErrTampered) ||
			errors.Is(err, secret.ErrAudienceMismatch) || errors.Is(err, secret.ErrUnsupportedVersion) {
			break
		}
		w.log.Warn("pull: opening the password failed; retrying", "err", err)
		if !sleep(ctx, backoff) {
			return false
		}
		backoff = min(2*backoff, w.cfg.BackoffMax)
	}
	w.log.Error("pull: account locked, its password cannot be opened; set it again with `yuuka account set --password-file`",
		"wrapper", w.src.Secret.Wrapper, "key_ref", w.src.Secret.KeyRef, "err", err)
	<-ctx.Done()
	return false
}

// wait waits d, or less when `yuuka account sync` asks for a round; a
// wait for the next day (budget, server limit) is not cut short, since
// the limit is the reason for it.
func (w *worker) wait(ctx context.Context, d time.Duration) (store.SyncRequest, bool) {
	end := w.cfg.Now().Add(d)
	for {
		left := end.Sub(w.cfg.Now())
		if left <= 0 {
			return store.SyncRequest{}, true
		}
		if !sleep(ctx, min(left, w.cfg.RequestPoll)) {
			return store.SyncRequest{}, false
		}
		if w.cfg.Now().Before(w.until) {
			continue
		}
		req, err := w.cfg.DB.TakeSyncRequest(ctx, w.src.AccountID)
		if err != nil {
			if ctx.Err() == nil {
				w.log.Warn("pull: read sync request", "err", err)
			}
			continue
		}
		if req.Sync {
			return req, true
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// round runs one round when it may, and says how long to wait for the
// next.
func (w *worker) round(ctx context.Context, retry bool) time.Duration {
	poll := time.Duration(w.src.PollSeconds) * time.Second
	now := w.cfg.Now()
	if now.Before(w.until) {
		return w.until.Sub(now)
	}
	// A request that came in while the worker was waiting for the next
	// day, or during the last round, is this round's.
	if req, err := w.cfg.DB.TakeSyncRequest(ctx, w.src.AccountID); err == nil {
		retry = retry || req.RetryFailed
	}
	select {
	case w.rounds <- struct{}{}:
		defer func() { <-w.rounds }()
	case <-ctx.Done():
		return 0
	}
	lock, ok, err := w.cfg.DB.LockPullAccount(ctx, w.src.AccountID)
	if err != nil {
		w.log.Error("pull: lock", "err", err)
		return w.failed()
	}
	if !ok {
		// Another serve pulls this account (§10.1.3).
		w.log.Debug("pull: another worker holds the account")
		return poll
	}
	defer lock.Release()

	c, err := w.connect(ctx)
	if err != nil {
		return w.trouble(ctx, err, poll)
	}
	defer func() {
		lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = c.Logout(lctx)
	}()
	r := &round{w: w, c: c, lock: lock, retry: retry}
	more, err := r.run(ctx)
	for _, a := range c.Alerts() {
		// RFC 3501 asks that alerts reach a person; server text, quoted.
		w.log.Warn("pull: the server sent an alert", "alert", a)
	}
	if err != nil {
		return w.trouble(ctx, err, poll)
	}
	w.backoff = 0
	if more {
		// Backfill left: the next round catches up first, then goes on.
		return min(poll, time.Second)
	}
	return poll
}

// connect dials and logs in.
func (w *worker) connect(ctx context.Context) (*imap.Client, error) {
	dctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	c, err := imap.Dial(dctx, imap.Config{Host: w.src.Host, Port: w.src.Port, RootCAs: w.cfg.RootCAs})
	if err != nil {
		return nil, err
	}
	err = func() error {
		if c.Caps() == nil {
			if _, err := c.Capability(dctx); err != nil {
				return err
			}
		}
		if c.Caps().Has("ID") {
			// Some providers ask for it before login; it is harmless.
			if _, err := c.ID(dctx, map[string]string{"name": "yuuka"}); err != nil && kind(err) != imap.KindRefused {
				return err
			}
		}
		if c.Caps().Has("AUTH=PLAIN") {
			return c.AuthenticatePlain(dctx, w.src.Username, string(w.password.Reveal()))
		}
		return c.Login(dctx, w.src.Username, string(w.password.Reveal()))
	}()
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// trouble decides what a failed round means.
func (w *worker) trouble(ctx context.Context, err error, poll time.Duration) time.Duration {
	if ctx.Err() != nil {
		return 0
	}
	switch {
	case errors.Is(err, errBudget):
		w.until = nextDay(w.cfg.Now())
		w.log.Info("pull: daily byte budget used up; resuming tomorrow (UTC)", "budget", w.src.DailyByteBudget)
		return w.until.Sub(w.cfg.Now())
	case kind(err) == imap.KindAuth:
		w.refused(ctx)
		return 24 * time.Hour
	case kind(err) == imap.KindLimit && fetching(err):
		// A download quota: stop until it resets, without counting it
		// as a failure (§10.1.2). Limits on connecting are more likely
		// a cap on connections, which backing off is enough for.
		w.until = nextDay(w.cfg.Now())
		w.log.Warn("pull: the server's usage limit was reached; resuming tomorrow (UTC)", "err", describe(err))
		return w.until.Sub(w.cfg.Now())
	}
	w.log.Warn("pull: round failed", "err", describe(err))
	return w.failed()
}

// failed returns the next backoff: from BackoffMin doubling to
// BackoffMax.
func (w *worker) failed() time.Duration {
	w.backoff = min(max(2*w.backoff, w.cfg.BackoffMin), w.cfg.BackoffMax)
	return w.backoff
}

// refused pauses the source after the server refused its login: no
// retry, since repeating a wrong password gets a mailbox locked
// (§10.1.3). The pause names the revision this worker logged in with, so
// a password changed meanwhile is not paused for the old one's failure.
func (w *worker) refused(ctx context.Context) {
	w.log.Error("pull: the server refused the login; account paused. Set a new password with " +
		"`yuuka account set --password-file` (or `yuuka account resume` to try the same one)")
	backoff := w.cfg.BackoffMin
	for {
		now := w.cfg.Now()
		err := w.cfg.DB.PauseSource(ctx, w.src.AccountName, w.src.Revision, store.PauseAuthFailed, store.Action{
			ID: ulid.New(now), Actor: store.SystemPrincipalID, Tool: "account.pause",
			Args: map[string]any{"account": w.src.AccountName, "reason": store.PauseAuthFailed}, At: now,
		})
		if err == nil || errors.Is(err, store.ErrStale) || errors.Is(err, store.ErrNotFound) {
			return
		}
		w.log.Error("pull: pausing after a refused login", "err", err)
		if !sleep(ctx, backoff) {
			return
		}
		backoff = min(2*backoff, w.cfg.BackoffMax)
	}
}

func nextDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
}

func kind(err error) imap.ErrorKind {
	var e *imap.Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return 0
}

// fetching: err came from a UID FETCH.
func fetching(err error) bool {
	var e *imap.Error
	return errors.As(err, &e) && e.Op == "UID FETCH"
}

// describe is err for a log line without the server's words, which are
// untrusted input (§10.1.3): an *imap.Error is reduced to its command,
// kind and response code.
func describe(err error) string {
	var e *imap.Error
	if !errors.As(err, &e) {
		return err.Error()
	}
	s := fmt.Sprintf("imap: %s: %s", e.Op, e.Kind)
	if e.Resp != nil {
		s += " (" + e.Resp.Type
		if e.Resp.Code != "" {
			s += " [" + e.Resp.Code + "]"
		}
		s += ")"
	} else if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

// syncFolders is manage.SyncFolders: one rule for what `account test`
// shows and what is pulled.
var syncFolders = manage.SyncFolders
