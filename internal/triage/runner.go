package triage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/yuuka-mail/yuuka/internal/triage/score"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Scorer answers Questions about one state: Client, or a fake.
type Scorer interface {
	Score(ctx context.Context, in Input) (Result, error)
}

// Store is what the runner needs from the store (store.DB): the input
// builder's reads, the messages to score, and the one place a score is
// written.
type Store interface {
	Messages
	// ScoreTargets lists up to limit messages of accounts received since
	// then that p has not scored, newest first; a failed one stays a
	// target.
	ScoreTargets(ctx context.Context, p score.Profile, accounts []string, since time.Time, limit int) ([]string, error)
	// RescoreTargets pages through every target of accounts since then,
	// scored or not, in id order.
	RescoreTargets(ctx context.Context, accounts []string, since time.Time, after string, limit int) ([]string, error)
	// InsertScore appends one score; scores are never changed.
	InsertScore(ctx context.Context, s score.Score) error
}

// RunnerConfig configures a Runner. Zero values take the defaults.
type RunnerConfig struct {
	Profile score.Profile
	// Endpoint is score.EndpointLocal or score.EndpointHosted, recorded
	// in every score.
	Endpoint string
	Scorer   Scorer
	Store    Store
	Raw      RawReader
	// MaxChars bounds the content of a state (DefaultMaxChars).
	MaxChars int
	// Concurrency is how many scorings run at once (2).
	Concurrency int
	// Gap is the least time between the starts of two scorings: the rate
	// cap (250ms, at most 4 a second).
	Gap time.Duration
	// Sweep is how often unscored mail is looked for (10m).
	Sweep time.Duration
	// Lookback bounds the sweep (DefaultLookback).
	Lookback time.Duration
	// Accounts are the accounts whose mail may be scored, by name
	// (Config.Accounts); nil is the default, the Cloudflare accounts.
	Accounts []string
	Log      *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// ErrUnavailable: the circuit breaker is open.
var ErrUnavailable = errors.New("triage: scorer unavailable, not asked")

// Runner scores mail in serve: messages kicked after ingest first, then
// whatever the periodic sweep finds unscored. It never blocks ingest
// and nothing waits on it. A failure writes nothing and is retried with
// backoff; while the scorer keeps failing a circuit breaker stops
// calling it, and the sweep picks the mail up once it answers again.
type Runner struct {
	cfg RunnerConfig
	br  breaker
	rl  limiter

	hot  chan string // kicked after ingest
	cold chan string // found by the sweep

	mu      sync.Mutex
	pending map[string]bool // queued or being scored
	backoff map[string]retry
}

type retry struct {
	n     int
	until time.Time
}

const (
	hotQueue    = 256
	targetsPage = 100
	backoffMin  = time.Minute
	backoffMax  = 6 * time.Hour
)

// NewRunner returns a Runner; Run starts it.
func NewRunner(cfg RunnerConfig) *Runner {
	if cfg.MaxChars <= 0 {
		cfg.MaxChars = DefaultMaxChars
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 2
	}
	if cfg.Gap <= 0 {
		cfg.Gap = 250 * time.Millisecond
	}
	if cfg.Sweep <= 0 {
		cfg.Sweep = 10 * time.Minute
	}
	if cfg.Lookback <= 0 {
		cfg.Lookback = DefaultLookback
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Runner{
		cfg:     cfg,
		hot:     make(chan string, hotQueue),
		cold:    make(chan string),
		pending: map[string]bool{},
		backoff: map[string]retry{},
	}
}

// Kick queues message id ahead of the sweep, so that a new message is
// scored within seconds. It never blocks: when the queue is full, or
// the breaker open, the message is left for the sweep.
func (r *Runner) Kick(id string) {
	if r.br.isOpen(r.cfg.Now()) || !r.claim(id) {
		return
	}
	select {
	case r.hot <- id:
	default:
		r.done(id)
	}
}

// Run scores until ctx is done: Concurrency workers, and the sweep at
// start and every Sweep.
func (r *Runner) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for range r.cfg.Concurrency {
		wg.Go(func() { r.work(ctx) })
	}
	tick := time.NewTicker(r.cfg.Sweep)
	defer tick.Stop()
	for {
		if err := r.sweep(ctx); err != nil && ctx.Err() == nil {
			r.cfg.Log.Error("triage sweep", "err", err)
		}
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-tick.C:
		}
	}
}

func (r *Runner) work(ctx context.Context) {
	for {
		var id string
		// Kicked mail first: it is the newest.
		select {
		case id = <-r.hot:
		default:
			select {
			case id = <-r.hot:
			case id = <-r.cold:
			case <-ctx.Done():
				return
			}
		}
		_ = r.handle(ctx, id)
		r.done(id)
	}
}

// sweep queues the unscored targets of the lookback, waiting for the
// workers, until a query yields nothing new. ScoreTargets has no cursor
// and keeps failed messages, so each query asks for as many more as are
// backing off: they cannot fill the page and stall the sweep.
func (r *Runner) sweep(ctx context.Context) error {
	for {
		if r.br.isOpen(r.cfg.Now()) {
			return nil
		}
		r.mu.Lock()
		limit := targetsPage + len(r.backoff) + len(r.pending)
		r.mu.Unlock()
		ids, err := r.cfg.Store.ScoreTargets(ctx, r.cfg.Profile, r.cfg.Accounts, r.cfg.Now().Add(-r.cfg.Lookback), limit)
		if err != nil {
			return err
		}
		queued := 0
		for _, id := range ids {
			if r.br.isOpen(r.cfg.Now()) {
				return nil
			}
			if !r.claim(id) {
				continue
			}
			select {
			case r.cold <- id:
				queued++
			case <-ctx.Done():
				r.done(id)
				return ctx.Err()
			}
		}
		if queued == 0 || len(ids) < limit {
			return nil
		}
	}
}

// claim marks id pending unless it is already, or backing off.
func (r *Runner) claim(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending[id] || r.cfg.Now().Before(r.backoff[id].until) {
		return false
	}
	r.pending[id] = true
	return true
}

func (r *Runner) done(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pending, id)
}

// handle scores one message and keeps the backoff.
func (r *Runner) handle(ctx context.Context, id string) error {
	if r.br.isOpen(r.cfg.Now()) {
		return ErrUnavailable
	}
	if err := r.rl.wait(ctx, r.cfg.Gap); err != nil {
		return err
	}
	err := r.scoreOne(ctx, id)
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case err == nil, errors.Is(err, ErrNotTarget):
		delete(r.backoff, id)
	case ctx.Err() != nil:
	case errors.Is(err, ErrBackend), errors.Is(err, ErrUnavailable):
		// The breaker deals with a scorer that is down; the message is
		// not to blame.
	default:
		b := r.backoff[id]
		b.n++
		b.until = r.cfg.Now().Add(min(backoffMin<<min(b.n-1, 16), backoffMax))
		r.backoff[id] = b
	}
	if err != nil && !errors.Is(err, ErrNotTarget) && !errors.Is(err, ErrUnavailable) && ctx.Err() == nil {
		// The error names the message and the status, never content.
		r.cfg.Log.Warn("triage: scoring failed", "message", id, "err", err)
	}
	return err
}

// scoreOne builds, scores and records message id, asking the breaker
// right before the call and telling it how the call went.
func (r *Runner) scoreOne(ctx context.Context, id string) error {
	b, err := Build(ctx, r.cfg.Store, r.cfg.Raw, id, r.cfg.MaxChars, r.cfg.Accounts)
	if err != nil {
		return err
	}
	if !r.br.allow(r.cfg.Now()) {
		return ErrUnavailable
	}
	res, err := r.cfg.Scorer.Score(ctx, Input{State: b.State})
	switch {
	case err == nil, errors.Is(err, ErrRejected):
		// The scorer answered, even if only to refuse this input.
		r.br.success()
	case errors.Is(err, ErrBackend):
		r.br.failure(r.cfg.Now())
	default:
		// Ours, or cancelled: nothing learnt about the scorer.
		r.br.release()
	}
	if err != nil {
		return err
	}
	b.Input.Endpoint = r.cfg.Endpoint
	now := r.cfg.Now()
	s := score.Score{
		ID: ulid.New(now), MessageID: b.MessageID, Profile: r.cfg.Profile, Model: res.Model,
		Answers: res.Answers, Input: b.Input, ScoredAt: now,
	}
	if err := r.cfg.Store.InsertScore(ctx, s); err != nil {
		return fmt.Errorf("record score of %s: %w", b.MessageID, err)
	}
	return nil
}

// RescoreStats counts what Rescore did.
type RescoreStats struct {
	Scored, Skipped, Failed int
}

// Rescore scores every target received since since again, writing new
// rows beside the old ones (§15.2), with Concurrency workers at the
// rate cap. It stops when the breaker opens.
func (r *Runner) Rescore(ctx context.Context, since time.Time) (RescoreStats, error) {
	var st RescoreStats
	var mu sync.Mutex
	ids := make(chan string)
	var wg sync.WaitGroup
	for range r.cfg.Concurrency {
		wg.Go(func() {
			for id := range ids {
				err := r.handle(ctx, id)
				mu.Lock()
				switch {
				case err == nil:
					st.Scored++
				case errors.Is(err, ErrNotTarget):
					st.Skipped++
				default:
					st.Failed++
				}
				mu.Unlock()
			}
		})
	}
	err := func() error {
		defer close(ids)
		for after := ""; ; {
			page, err := r.cfg.Store.RescoreTargets(ctx, r.cfg.Accounts, since, after, targetsPage)
			if err != nil {
				return err
			}
			for _, id := range page {
				if r.br.isOpen(r.cfg.Now()) {
					return errors.New("triage: the scorer keeps failing; stopped")
				}
				select {
				case ids <- id:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			if len(page) < targetsPage {
				return nil
			}
			after = page[len(page)-1]
		}
	}()
	wg.Wait()
	return st, err
}

// limiter spaces the starts of scorings by a gap.
type limiter struct {
	mu   sync.Mutex
	next time.Time
}

func (l *limiter) wait(ctx context.Context, gap time.Duration) error {
	l.mu.Lock()
	now := time.Now()
	at := l.next
	if at.Before(now) {
		at = now
	}
	l.next = at.Add(gap)
	l.mu.Unlock()
	if d := at.Sub(now); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
