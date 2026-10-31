package outbox

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/transport"
)

// recordTimeout bounds writing an attempt's result, which happens even
// while shutting down.
const recordTimeout = 30 * time.Second

// errShutdown is what the commit callback answers once Run is stopping:
// the attempt is abandoned before the point of no return.
var errShutdown = errors.New("shutting down")

// Run recovers expired attempts, then sends due rows with cfg.Workers
// workers until ctx ends. It returns once every attempt in flight has
// been recorded: an attempt that has not reached its commit point is
// abandoned at once (nothing was sent, it is due again immediately), and
// one past it runs to its final reply or SendTimeout, so a shutdown never
// turns into an unknown by itself.
func (s *Service) Run(ctx context.Context) {
	s.housekeep(ctx)
	var wg sync.WaitGroup
	for range s.cfg.Workers {
		wg.Go(func() { s.worker(ctx) })
	}
	wg.Go(func() {
		t := time.NewTicker(s.cfg.RecoverInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.housekeep(ctx)
			}
		}
	})
	wg.Wait()
}

func (s *Service) worker(ctx context.Context) {
	for ctx.Err() == nil {
		did, err := s.step(ctx)
		if err != nil && ctx.Err() == nil {
			s.log.Error("claim", "err", err)
		}
		if did && err == nil {
			continue
		}
		t := time.NewTimer(s.idle())
		select {
		case <-ctx.Done():
		case <-s.wake:
		case <-t.C:
		}
		t.Stop()
	}
}

// step releases the held mail whose recall window has ended, then
// claims one due row and sends it. It reports whether it found one.
func (s *Service) step(ctx context.Context) (bool, error) {
	s.promote(ctx)
	c, err := s.cfg.DB.ClaimOutbox(ctx, s.cfg.Owner, s.available(), s.cfg.Lease)
	if err != nil || c == nil {
		return false, err
	}
	// More may be due; let another worker look while this one sends.
	s.Wake()
	s.attempt(ctx, c)
	return true, nil
}

// promote releases held rows whose window has ended (store.PromoteHeld)
// and notes when the next window ends, so an idle worker wakes for it
// rather than up to a poll interval later.
func (s *Service) promote(ctx context.Context) {
	rel, next, err := s.cfg.DB.PromoteHeld(ctx)
	for _, r := range rel {
		if r.State == store.OutboxQueued {
			s.log.Info("recall window ended; released", "outbox", r.OutboxID)
		} else {
			s.log.Warn("recall window ended on a suspended grant or a disabled identity or submitter; withdrawn to its draft",
				"outbox", r.OutboxID, "draft", r.DraftID, "reason", r.Reason)
		}
	}
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("promote held", "err", err)
		}
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextHold = time.Time{}
	if next >= 0 {
		s.nextHold = s.cfg.Now().Add(next)
	}
}

// idle is how long an idle worker waits: the poll interval, or until the
// next recall window ends if that is sooner.
func (s *Service) idle() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	wait := s.cfg.PollInterval
	if !s.nextHold.IsZero() {
		// A moment past the end: the window is the database's clock.
		wait = min(wait, max(s.nextHold.Sub(s.cfg.Now())+100*time.Millisecond, 0))
	}
	return wait
}

// housekeep recovers attempts whose executor is gone and parks rows whose
// transport is not configured.
func (s *Service) housekeep(ctx context.Context) {
	rec, err := s.cfg.DB.RecoverAttempts(ctx, recoverRetry)
	for _, r := range rec {
		if r.Outcome == store.AttemptUnknown {
			s.log.Error("executor lost after the commit point: the message may have been delivered; "+
				"check and decide with `yuuka outbox resolve`", "outbox", r.OutboxID, "attempt", r.AttemptID)
		} else {
			s.log.Warn("executor lost before the commit point; queued again", "outbox", r.OutboxID, "attempt", r.AttemptID)
		}
	}
	if err != nil && ctx.Err() == nil {
		s.log.Error("recover attempts", "err", err)
	}
	parked, err := s.cfg.DB.ParkUnconfigured(ctx, s.names, unconfiguredRetry)
	for _, p := range parked {
		s.log.Error("not sending: the identity's transport is not configured (YUUKA_TRANSPORTS); the message waits",
			"outbox", p.OutboxID, "transport", p.Transport)
	}
	if err != nil && ctx.Err() == nil {
		s.log.Error("park unconfigured", "err", err)
	}
}

// attempt runs one claimed attempt to its end and records it.
func (s *Service) attempt(ctx context.Context, c *store.Claimed) {
	log := s.log.With("outbox", c.OutboxID, "attempt", c.AttemptID, "transport", c.Transport)
	// What happened is recorded even while shutting down.
	bg := context.WithoutCancel(ctx)
	stopLease := s.keepLease(bg, log, c)
	defer stopLease()

	raw, err := s.readRaw(ctx, c)
	if err != nil {
		log.Error("cannot read the sent copy; not sending", "raw", c.RawID, "err", err)
		s.abandon(bg, log, c, "read sent copy: "+err.Error(), storageRetry)
		return
	}

	sendCtx, cancelSend := context.WithTimeout(bg, s.cfg.SendTimeout)
	defer cancelSend()
	// Shutdown cancels an attempt only while it is short of the commit
	// point; mu orders that against the commit callback, so an attempt is
	// either cancelled or committed, never cancelled after committing.
	var mu sync.Mutex
	var stopping, committing bool
	stopWatch := context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		stopping = true
		if !committing {
			cancelSend()
		}
	})
	commit := func(cctx context.Context) error {
		mu.Lock()
		if stopping {
			mu.Unlock()
			return errShutdown
		}
		committing = true
		mu.Unlock()
		return s.cfg.DB.CommitAttempt(cctx, c.AttemptID, s.cfg.Owner)
	}

	tr := s.cfg.Transports[c.Transport]
	res, sendErr := tr.Send(sendCtx, transport.Envelope{From: c.FromAddr, To: c.Recipients}, raw, commit)
	stopWatch()
	mu.Lock()
	stopped := stopping
	mu.Unlock()

	switch {
	case res.Committed:
		s.markReachable(c.Transport)
		s.finish(bg, log, c, res, sendErr)
	case stopped:
		s.abandon(bg, log, c, "shut down before the commit point", 0)
	case stage(sendErr) == transport.StageCommit:
		// Not about the message: the commit point could not be recorded
		// (lease lost, database unreachable), so nothing was sent.
		log.Warn("commit point not recorded; not sent", "err", sendErr)
		s.abandon(bg, log, c, sendErr.Error(), commitRetry)
	case transport.ExitFailure(sendErr):
		d := s.pause(c.Transport, sendErr)
		s.abandon(bg, log, c, fmt.Sprintf("transport paused for %s: %v", d, sendErr), d)
	default:
		s.markReachable(c.Transport)
		s.finish(bg, log, c, res, sendErr)
	}
}

func stage(err error) transport.Stage {
	if e, ok := errors.AsType[*transport.Error](err); ok {
		return e.Stage
	}
	return ""
}

// keepLease renews the attempt's lease every third of it until stopped.
func (s *Service) keepLease(ctx context.Context, log *slog.Logger, c *store.Claimed) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(s.cfg.Lease / 3)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			ok, err := s.cfg.DB.RenewLease(ctx, c.AttemptID, s.cfg.Owner, s.cfg.Lease)
			switch {
			case err != nil && ctx.Err() == nil:
				// The commit point checks the lease itself, so a lease
				// that runs out here cannot lead to a send.
				log.Warn("renew lease", "err", err)
			case err == nil && !ok:
				log.Warn("lease lost: recovery finished this attempt; its result will be recorded as late evidence")
				return
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// readRaw returns the sent copy of c, checked against the hash recorded
// when it was stored: the bytes sent must be the sent copy.
func (s *Service) readRaw(ctx context.Context, c *store.Claimed) ([]byte, error) {
	if c.LocalPath != c.R2Key || len(c.SHA256) != sha256.Size {
		return nil, fmt.Errorf("raw %s: local path %q, key %q, sha256 of %d bytes", c.RawID, c.LocalPath, c.R2Key, len(c.SHA256))
	}
	return s.ReadSent(ctx, store.SentRaw{ID: c.RawID, Key: c.R2Key, Size: c.Size, SHA256: [sha256.Size]byte(c.SHA256)})
}

func (s *Service) abandon(ctx context.Context, log *slog.Logger, c *store.Claimed, why string, retry time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, recordTimeout)
	defer cancel()
	ok, err := s.cfg.DB.AbandonAttempt(ctx, c.AttemptID, why, retry)
	switch {
	case err != nil:
		// The lease runs out and recovery abandons it instead.
		log.Error("record abandoned attempt", "err", err)
	case ok:
		log.Info("attempt abandoned before the commit point; queued again", "why", why, "retry_in", retry)
	}
}

// finish records an attempt that ended with an answer, retrying for a
// while: after the commit point a lost result turns into unknown once the
// lease runs out.
func (s *Service) finish(ctx context.Context, log *slog.Logger, c *store.Claimed, res *transport.SendResult, sendErr error) {
	ar := store.AttemptResult{
		Outcome:    res.Outcome.String(),
		ProviderID: res.ProviderID,
		MessageID:  res.MessageID,
		Error:      errorText(res, sendErr),
	}
	for _, r := range res.Recipients {
		ar.Recipients = append(ar.Recipients, store.RecipientResult{Addr: r.Addr, State: r.Status.String(), Code: r.Code, Detail: r.Text})
	}
	ctx, cancel := context.WithTimeout(ctx, recordTimeout)
	defer cancel()
	var f store.Finished
	var err error
	for wait := time.Second; ; wait *= 2 {
		if f, err = s.cfg.DB.FinishAttempt(ctx, c.AttemptID, ar, s.cfg.Retry); err == nil {
			break
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
		case <-t.C:
		}
		t.Stop()
		if ctx.Err() != nil {
			log.Error("record attempt result; recovery will treat it as lost", "outcome", ar.Outcome, "err", err)
			return
		}
	}
	attrs := []any{"outcome", ar.Outcome, "state", f.State, "provider_id", res.ProviderID}
	switch {
	case f.Late && f.Applied:
		log.Warn("late result of a recovered attempt settled its unknown row", attrs...)
	case f.Late:
		log.Warn("late result of a recovered attempt; the outbox row has moved on", attrs...)
	case f.State == store.OutboxUnknown:
		log.Error("no final answer after the commit point: the message may have been delivered; "+
			"check and decide with `yuuka outbox resolve`", append(attrs, "err", sendErr)...)
	case f.State == store.OutboxDead:
		log.Error("not delivered; giving up", append(attrs, "err", ar.Error)...)
	case f.State == store.OutboxSent:
		log.Info("sent", attrs...)
	default:
		log.Warn("not delivered to everyone", append(attrs, "err", ar.Error)...)
	}
}

// errorText is outbox.error for an attempt: the transport's error, or for
// a partial delivery the recipients it did not reach.
func errorText(res *transport.SendResult, err error) string {
	if err != nil {
		return err.Error()
	}
	var refused []string
	for _, r := range res.Recipients {
		if r.Status != transport.StatusAccepted {
			refused = append(refused, strings.TrimSpace(fmt.Sprintf("%s %s: %d %s", r.Addr, r.Status, r.Code, r.Text)))
		}
	}
	return strings.Join(refused, "; ")
}

// pause takes transport name out of claiming after an exit failure and
// returns for how long. The pause doubles, up to PauseMax, while the exit
// keeps failing; a concurrent failure during a pause does not extend it.
func (s *Service) pause(name string, err error) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.cfg.Now()
	p := s.paused[name]
	if p.until.After(now) {
		return p.until.Sub(now)
	}
	d := max(p.next, s.cfg.PauseMin)
	p.until, p.next = now.Add(d), min(2*d, s.cfg.PauseMax)
	s.paused[name] = p
	s.log.Error("transport paused: the exit cannot be used, so messages wait instead of failing; "+
		"check its host, certificate and credentials", "transport", name, "for", d, "err", err)
	return d
}

// markReachable ends the pause schedule of a transport that got as far as
// MAIL: the next exit failure starts again at PauseMin.
func (s *Service) markReachable(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.paused, name)
}

// available lists the configured transports that are not paused.
func (s *Service) available() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.cfg.Now()
	out := make([]string, 0, len(s.names))
	for _, n := range s.names {
		if !s.paused[n].until.After(now) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}
