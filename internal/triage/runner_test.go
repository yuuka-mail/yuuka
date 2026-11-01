package triage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/triage/score"
)

// fakeStore holds messages built from detail() and the scores written,
// with ScoreTargets' contract: newest first, no cursor, failed messages
// stay targets.
type fakeStore struct {
	mu     sync.Mutex
	ids    []string // oldest first
	scores []score.Score
}

func newFakeStore(ids ...string) *fakeStore { return &fakeStore{ids: ids} }

func (f *fakeStore) MessageByID(_ context.Context, v *auth.Viewer, id string) (store.MessageDetail, error) {
	if v == nil {
		return store.MessageDetail{}, errors.New("no viewer")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Contains(f.ids, id) {
		return store.MessageDetail{}, store.ErrNotFound
	}
	m := detail()
	m.ID = id
	m.Raw = store.RawRef{RawID: "raw"}
	return m, nil
}

func (f *fakeStore) IsScoreTarget(context.Context, string, []string) (bool, error) {
	return true, nil
}

func (f *fakeStore) KnownSender(context.Context, *auth.Viewer, string) (bool, error) {
	return false, nil
}

func (f *fakeStore) ScoreTargets(_ context.Context, p score.Profile, _ []string, _ time.Time, limit int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, id := range slices.Backward(f.ids) {
		scored := slices.ContainsFunc(f.scores, func(s score.Score) bool { return s.MessageID == id && s.Profile == p })
		if !scored {
			out = append(out, id)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeStore) RescoreTargets(_ context.Context, _ []string, _ time.Time, after string, limit int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, id := range f.ids {
		if id > after && len(out) < limit {
			out = append(out, id)
		}
	}
	return out, nil
}

func (f *fakeStore) InsertScore(_ context.Context, s score.Score) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scores = append(f.scores, s)
	return nil
}

func (f *fakeStore) scored() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for _, s := range f.scores {
		out[s.MessageID]++
	}
	return out
}

// fakeScorer answers with fixed probabilities after delay, or with err.
type fakeScorer struct {
	delay    time.Duration
	err      error
	calls    atomic.Int32
	inFlight atomic.Int32
	maxIn    atomic.Int32
}

func (s *fakeScorer) Score(ctx context.Context, _ Input) (Result, error) {
	s.calls.Add(1)
	n := s.inFlight.Add(1)
	defer s.inFlight.Add(-1)
	for {
		m := s.maxIn.Load()
		if n <= m || s.maxIn.CompareAndSwap(m, n) {
			break
		}
	}
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	if s.err != nil {
		return Result{}, s.err
	}
	return Result{Model: "fake-1", Answers: map[string]score.Answer{
		Kind:       {P: map[string]float64{"notification": 1, "conversation": 0, "marketing": 0, "spam": 0}},
		Phishing:   {P: map[string]float64{"yes": 0, "no": 1}},
		NeedsReply: {Abstain: true},
	}}, nil
}

func ids(n int) []string {
	var out []string
	for i := range n {
		out = append(out, fmt.Sprintf("01JBXJ6H6Z%016d", i))
	}
	return out
}

var fakeProfile = score.Profile{Scorer: "fake", Questions: QuestionsVersion}

func newTestRunner(st Store, sc Scorer) *Runner {
	return NewRunner(RunnerConfig{
		Profile: fakeProfile, Endpoint: score.EndpointLocal,
		Scorer: sc, Store: st, Raw: rawMap{"raw": []byte(letter)},
		Gap: time.Millisecond, Sweep: time.Hour, Log: slog.New(slog.DiscardHandler),
	})
}

func run(t *testing.T, r *Runner) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRunnerSweeps(t *testing.T) {
	st := newFakeStore(ids(250)...)
	sc := &fakeScorer{delay: time.Millisecond}
	run(t, newTestRunner(st, sc))
	eventually(t, "every target scored", func() bool { return len(st.scored()) == 250 })
	for id, n := range st.scored() {
		if n != 1 {
			t.Errorf("%s scored %d times", id, n)
		}
	}
	if m := sc.maxIn.Load(); m > 2 {
		t.Errorf("%d scorings at once, want at most 2", m)
	}
	st.mu.Lock()
	s := st.scores[0]
	st.mu.Unlock()
	if s.Profile != fakeProfile || s.Model != "fake-1" || s.Input.Endpoint != score.EndpointLocal ||
		s.ID == "" || s.ScoredAt.IsZero() || !s.Input.Facts.Claimed || !s.Answers[NeedsReply].Abstain {
		t.Errorf("score %+v", s)
	}
}

func TestRunnerKick(t *testing.T) {
	st := newFakeStore()
	r := newTestRunner(st, &fakeScorer{})
	run(t, r)
	// After the first sweep found nothing, new mail arrives.
	time.Sleep(10 * time.Millisecond)
	st.mu.Lock()
	st.ids = ids(1)
	st.mu.Unlock()
	r.Kick(ids(1)[0])
	eventually(t, "the kicked message scored", func() bool { return st.scored()[ids(1)[0]] == 1 })
}

// staleTargets lists targets, then runs listed before answering: what
// happens between the query and the sweep reaching its ids.
type staleTargets struct {
	*fakeStore
	listed func()
}

func (s staleTargets) ScoreTargets(ctx context.Context, p score.Profile, a []string, since time.Time, limit int) ([]string, error) {
	out, err := s.fakeStore.ScoreTargets(ctx, p, a, since, limit)
	s.listed()
	return out, err
}

// gatedScorer answers once gate is closed.
type gatedScorer struct {
	fakeScorer
	started chan struct{}
	gate    chan struct{}
}

func (s *gatedScorer) Score(ctx context.Context, in Input) (Result, error) {
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-s.gate
	return s.fakeScorer.Score(ctx, in)
}

// A message being scored when the sweep lists it, and finished before the
// sweep gets to it, is not scored again from the stale list.
func TestRunnerSweepAfterScored(t *testing.T) {
	id := ids(1)[0]
	sc := &gatedScorer{started: make(chan struct{}, 1), gate: make(chan struct{})}
	var r *Runner
	st := staleTargets{fakeStore: newFakeStore(id)}
	st.listed = func() {
		close(sc.gate)
		eventually(t, "the kicked scoring done", func() bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			return !r.pending[id]
		})
	}
	r = newTestRunner(st, sc)
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	wg.Go(func() { r.work(ctx) })
	r.Kick(id)
	<-sc.started
	if err := r.sweep(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "nothing pending", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.pending) == 0
	})
	if n := st.scored()[id]; n != 1 {
		t.Errorf("scored %d times, want 1", n)
	}
}

// failing cannot read the messages in bad.
type failing struct {
	*fakeStore
	bad map[string]bool
}

func (f failing) MessageByID(ctx context.Context, v *auth.Viewer, id string) (store.MessageDetail, error) {
	if f.bad[id] {
		return store.MessageDetail{}, errors.New("raw unreadable")
	}
	return f.fakeStore.MessageByID(ctx, v, id)
}

// Failed messages stay targets and come first; they back off and must
// not keep the sweep from the older ones behind them.
func TestRunnerSweepPastFailures(t *testing.T) {
	all := ids(targetsPage + 30)
	st := failing{newFakeStore(all...), map[string]bool{}}
	for _, id := range all[len(all)-targetsPage-10:] {
		st.bad[id] = true
	}
	r := newTestRunner(st, &fakeScorer{})
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	wg.Go(func() { r.work(ctx) })
	wg.Go(func() { r.work(ctx) })
	for range 2 {
		if err := r.sweep(ctx); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "the older messages scored", func() bool { return len(st.scored()) == 20 })
	if r.br.isOpen(time.Now()) {
		t.Error("message failures opened the breaker")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if b := r.backoff[all[len(all)-1]]; b.n != 1 || time.Until(b.until) < 50*time.Second {
		t.Errorf("backoff %+v: want one failure, retried in about a minute", b)
	}
}

// A scorer that keeps failing trips the breaker: calls stop.
func TestRunnerBreaker(t *testing.T) {
	st := newFakeStore(ids(20)...)
	sc := &fakeScorer{err: ErrBackend}
	r := newTestRunner(st, sc)
	run(t, r)
	eventually(t, "the breaker to open", func() bool { return r.br.isOpen(time.Now()) && sc.inFlight.Load() == 0 })
	// A worker may have been let through just before it opened.
	time.Sleep(20 * time.Millisecond)
	calls := sc.calls.Load()
	if calls < tripAfter || calls > tripAfter+1 {
		t.Errorf("%d calls before the breaker opened", calls)
	}
	r.Kick(ids(20)[0])
	time.Sleep(20 * time.Millisecond)
	if n := sc.calls.Load(); n != calls {
		t.Errorf("%d more calls while open", n-calls)
	}
	if len(st.scored()) != 0 {
		t.Error("a failure wrote a score")
	}
	// Backend failures are not the messages' fault.
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.backoff) != 0 {
		t.Errorf("messages backing off for a scorer that is down: %v", r.backoff)
	}
}

// A message the scorer refuses backs off; the scorer is not blamed.
func TestRunnerRejected(t *testing.T) {
	st := newFakeStore(ids(2)...)
	sc := &fakeScorer{err: ErrRejected}
	r := newTestRunner(st, sc)
	ctx := t.Context()
	for range 2 {
		for _, id := range ids(2) {
			if r.claim(id) {
				_ = r.handle(ctx, id)
				r.done(id)
			}
		}
	}
	if n := sc.calls.Load(); n != 2 {
		t.Errorf("%d calls, want each message once before its backoff ends", n)
	}
	if r.br.isOpen(time.Now()) {
		t.Error("refusals opened the breaker")
	}
}

func TestRescore(t *testing.T) {
	st := newFakeStore(ids(150)...)
	r := newTestRunner(st, &fakeScorer{})
	ctx := t.Context()
	if _, err := r.Rescore(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}
	stats, err := r.Rescore(ctx, time.Time{})
	if err != nil || stats.Scored != 150 || stats.Failed != 0 {
		t.Fatalf("%+v, %v", stats, err)
	}
	for id, n := range st.scored() {
		if n != 2 {
			t.Errorf("%s: %d rows, want the old one kept beside the new", id, n)
		}
	}
}

func TestRateCap(t *testing.T) {
	st := newFakeStore(ids(5)...)
	r := newTestRunner(st, &fakeScorer{})
	r.cfg.Gap = 30 * time.Millisecond
	start := time.Now()
	if _, err := r.Rescore(t.Context(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 4*30*time.Millisecond {
		t.Errorf("5 scorings in %s with a 30ms gap", d)
	}
}

func TestBreakerStates(t *testing.T) {
	var b breaker
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for range tripAfter - 1 {
		b.failure(now)
	}
	if !b.allow(now) || b.isOpen(now) {
		t.Fatal("open before tripAfter failures")
	}
	b.failure(now)
	if b.allow(now) || !b.isOpen(now) {
		t.Fatal("not open after tripAfter failures")
	}
	// After the cool-down one probe, and only one.
	now = now.Add(coolMin)
	if b.isOpen(now) || !b.allow(now) || b.allow(now) {
		t.Fatal("not exactly one probe after the cool-down")
	}
	b.failure(now)
	if b.allow(now.Add(coolMin)) || !b.allow(now.Add(2*coolMin)) {
		t.Fatal("a failed probe did not double the cool-down")
	}
	// A probe that taught nothing frees the next.
	b.release()
	if !b.allow(now.Add(2 * coolMin)) {
		t.Fatal("release did not free the probe")
	}
	b.success()
	if !b.allow(now) || b.isOpen(now) {
		t.Fatal("success did not close")
	}
	// The cool-down stops growing.
	for range 20 {
		b.failure(now)
		now = now.Add(time.Hour)
		b.allow(now)
	}
	if b.cool != coolMax {
		t.Errorf("cool-down %s, want %s", b.cool, coolMax)
	}
}
