package pull

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/notify"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// troubles is a Notifier that records the cards it gets. reached is how
// many it says it reached, err the failure it reports.
type troubles struct {
	mu      sync.Mutex
	cards   []notify.TroubleCard
	reached int
	err     error
}

func (n *troubles) AccountTrouble(_ context.Context, c notify.TroubleCard) (int, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.cards = append(n.cards, c)
	return n.reached, n.err
}

func (n *troubles) set(reached int, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.reached, n.err = reached, err
}

func (n *troubles) kinds() []notify.TroubleKind {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]notify.TroubleKind, len(n.cards))
	for i, c := range n.cards {
		out[i] = c.Kind
	}
	return out
}

func (n *troubles) last() notify.TroubleCard {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.cards[len(n.cards)-1]
}

func wantKinds(t *testing.T, n *troubles, want ...notify.TroubleKind) {
	t.Helper()
	if got := n.kinds(); !slices.Equal(got, want) {
		t.Errorf("cards %v, want %v", got, want)
	}
}

func troubleEnv(t *testing.T, srv *fakeServer) (*env, *troubles) {
	t.Helper()
	e := newEnv(t, srv)
	n := &troubles{reached: 1}
	e.cfg.Trouble = n
	return e, n
}

// A refused login is told once, whatever the rounds and the restarts of
// serve that follow; another pause, after a person resumed, is told
// again.
func TestTroubleRefused(t *testing.T) {
	t.Parallel()
	srv, _ := gmailServer(t)
	srv.refuse = true
	e, n := troubleEnv(t, srv)
	src := e.add("gmail", store.PresetGmail, nil)
	e.round(e.worker("gmail"), false)
	ctx := t.Context()

	// A push that reached nobody is tried again; it told nobody.
	n.set(0, errors.New("telegram is down"))
	m := New(e.cfg)
	m.tellTrouble(ctx)
	m.tellTrouble(ctx)
	wantKinds(t, n, notify.TroubleRefused, notify.TroubleRefused)

	// One that reached somebody is not, or the others would get it on
	// every poll for as long as one chat stays broken.
	n.set(1, errors.New("one chat is gone"))
	m.tellTrouble(ctx)
	wantKinds(t, n, notify.TroubleRefused, notify.TroubleRefused, notify.TroubleRefused)
	n.set(1, nil)
	for range 3 {
		m.tellTrouble(ctx)
	}
	// A restart forgets nothing: it is in the table.
	New(e.cfg).tellTrouble(ctx)
	wantKinds(t, n, notify.TroubleRefused, notify.TroubleRefused, notify.TroubleRefused)
	if c := n.last(); c.AccountID != src.AccountID || c.Account != "gmail" {
		t.Errorf("card %+v: the dispatcher decides who sees it by the account's id", c)
	}

	cur, err := e.db.SourceByName(ctx, "gmail")
	if err != nil {
		t.Fatal(err)
	}
	now := e.clock()
	if err := e.db.ResumeSource(ctx, "gmail", cur.Revision, store.Action{ID: ulid.New(now), Actor: store.SystemPrincipalID, Tool: "account.resume", At: now}); err != nil {
		t.Fatal(err)
	}
	m.tellTrouble(ctx)
	wantKinds(t, n, notify.TroubleRefused, notify.TroubleRefused, notify.TroubleRefused)
	e.advance(time.Minute)
	e.round(e.worker("gmail"), false)
	m.tellTrouble(ctx)
	m.tellTrouble(ctx)
	wantKinds(t, n, notify.TroubleRefused, notify.TroubleRefused, notify.TroubleRefused, notify.TroubleRefused)
}

// serve runs the watch: a refused login reaches a person with no worker
// running, and once.
func TestTroubleManager(t *testing.T) {
	t.Parallel()
	srv, _ := gmailServer(t)
	srv.refuse = true
	e, n := troubleEnv(t, srv)
	e.add("gmail", store.PresetGmail, nil)
	e.round(e.worker("gmail"), false)
	e.cfg.TroublePoll, e.cfg.Interval = 5*time.Millisecond, 5*time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	New(e.cfg).Run(ctx)
	wantKinds(t, n, notify.TroubleRefused)
}

// A pause by the operator is the operator's own doing: nothing to tell.
func TestTroubleOperatorPause(t *testing.T) {
	t.Parallel()
	srv, _ := gmailServer(t)
	e, n := troubleEnv(t, srv)
	src := e.add("gmail", store.PresetGmail, nil)
	now := e.clock()
	if err := e.db.PauseSource(t.Context(), "gmail", src.Revision, store.PauseOperator, store.Action{ID: ulid.New(now), Actor: store.SystemPrincipalID, Tool: "account.pause", At: now}); err != nil {
		t.Fatal(err)
	}
	New(e.cfg).tellTrouble(t.Context())
	wantKinds(t, n)
}

// Passed-over mail is told when it is passed, not while it is retried,
// and at most once a day, with what is new since.
func TestTroubleFailures(t *testing.T) {
	t.Parallel()
	srv, all := gmailServer(t)
	e, n := troubleEnv(t, srv)
	e.cfg.MaxAttempts = 2
	e.add("gmail", store.PresetGmail, nil)
	w := e.worker("gmail")
	ctx := t.Context()
	m := New(e.cfg)
	e.round(w, false)
	bad := srv.add(all, fakeMsg{raw: letter(1, "a"), date: e.clock(), gmID: 1})
	srv.set(func() { srv.noBody[bad] = true })

	e.round(w, false)
	m.tellTrouble(ctx)
	wantKinds(t, n) // still being retried
	e.round(w, false)
	m.tellTrouble(ctx)
	m.tellTrouble(ctx)
	wantKinds(t, n, notify.TroubleFailures)
	if c := n.last(); c.New != 1 || c.Open != 1 {
		t.Errorf("card %+v", c)
	}

	// Another one soon after waits for tomorrow.
	worse := srv.add(all, fakeMsg{raw: letter(2, "b"), date: e.clock(), gmID: 2})
	srv.set(func() { srv.noBody[worse] = true })
	e.round(w, false)
	e.round(w, false)
	m.tellTrouble(ctx)
	wantKinds(t, n, notify.TroubleFailures)
	e.advance(23 * time.Hour)
	m.tellTrouble(ctx)
	wantKinds(t, n, notify.TroubleFailures)
	e.advance(time.Hour)
	m.tellTrouble(ctx)
	New(e.cfg).tellTrouble(ctx)
	wantKinds(t, n, notify.TroubleFailures, notify.TroubleFailures)
	if c := n.last(); c.New != 1 || c.Open != 2 {
		t.Errorf("card %+v", c)
	}

	// A retry that fails again is no news; one that works lowers the
	// count, so the next failure is news.
	e.advance(24 * time.Hour)
	e.round(w, true)
	m.tellTrouble(ctx)
	wantKinds(t, n, notify.TroubleFailures, notify.TroubleFailures)
	srv.set(func() { delete(srv.noBody, bad); delete(srv.noBody, worse) })
	e.round(w, true)
	m.tellTrouble(ctx)
	wantKinds(t, n, notify.TroubleFailures, notify.TroubleFailures)
	worst := srv.add(all, fakeMsg{raw: letter(3, "c"), date: e.clock(), gmID: 3})
	srv.set(func() { srv.noBody[worst] = true })
	e.round(w, false)
	e.round(w, false)
	m.tellTrouble(ctx)
	wantKinds(t, n, notify.TroubleFailures, notify.TroubleFailures, notify.TroubleFailures)
	if c := n.last(); c.New != 1 || c.Open != 1 {
		t.Errorf("card %+v", c)
	}
}

// closedPort is a port nothing listens on.
func closedPort(t *testing.T) int {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close() //nolint:errcheck // only its port is wanted
	return l.Addr().(*net.TCPAddr).Port
}

// Rounds that keep failing are told once StallAfter has passed, once;
// a round that works ends it, and a new run is told again.
func TestTroubleStalled(t *testing.T) {
	t.Parallel()
	srv, _ := gmailServer(t)
	e, n := troubleEnv(t, srv)
	e.add("gmail", store.PresetGmail, nil)
	w := e.worker("gmail")
	ctx := t.Context()
	port := w.src.Port
	fail := func() {
		w.src.Port = closedPort(t)
		e.round(w, false)
		w.tellRound(ctx)
	}

	fail()
	wantKinds(t, n)
	e.advance(5 * time.Hour)
	fail()
	wantKinds(t, n)

	// Nobody reached: asked again after the next failure.
	e.advance(time.Hour)
	n.set(0, errors.New("telegram is down"))
	fail()
	wantKinds(t, n, notify.TroubleStalled)
	n.set(1, nil)
	fail()
	wantKinds(t, n, notify.TroubleStalled, notify.TroubleStalled)
	fail()
	e.advance(24 * time.Hour)
	fail()
	wantKinds(t, n, notify.TroubleStalled, notify.TroubleStalled)
	if c := n.last(); c.Why != "connection" || c.For < 6*time.Hour {
		t.Errorf("card %+v", c)
	}

	w.src.Port = port
	e.round(w, false)
	w.tellRound(ctx)
	fail()
	wantKinds(t, n, notify.TroubleStalled, notify.TroubleStalled)
	e.advance(6 * time.Hour)
	fail()
	wantKinds(t, n, notify.TroubleStalled, notify.TroubleStalled, notify.TroubleStalled)
}

// A limit is told as the round it stopped ends, one a day by its nature.
// The share of the budget backfill may use is no trouble: new mail still
// comes in.
func TestTroubleLimits(t *testing.T) {
	t.Parallel()
	srv, all := gmailServer(t)
	e, n := troubleEnv(t, srv)
	size := len(letter(10, "backfill"))
	e.add("gmail", store.PresetGmail, func(c *store.SourceConfig) { c.DailyByteBudget = int64(10 * size) })
	for i := range 20 {
		srv.add(all, fakeMsg{raw: letter(10+i, "backfill"), date: e.clock().Add(-time.Hour), gmID: uint64(100 + i)})
	}
	w := e.worker("gmail")
	ctx := t.Context()
	e.round(w, false)
	w.tellRound(ctx)
	wantKinds(t, n)

	for i := range 3 {
		srv.add(all, fakeMsg{raw: letter(40+i, "new"), date: e.clock(), gmID: uint64(200 + i)})
	}
	e.round(w, false)
	w.tellRound(ctx)
	e.round(w, false) // waiting for tomorrow
	w.tellRound(ctx)
	wantKinds(t, n, notify.TroubleBudget)

	e.advance(24 * time.Hour)
	srv.set(func() { srv.overquota = true })
	srv.add(all, fakeMsg{raw: letter(50, "x"), date: e.clock(), gmID: 300})
	e.round(w, false)
	w.tellRound(ctx)
	wantKinds(t, n, notify.TroubleBudget, notify.TroubleLimit)
}

// A key that is not loaded idles the worker, told once.
func TestTroubleLocked(t *testing.T) {
	t.Parallel()
	srv, _ := gmailServer(t)
	e, n := troubleEnv(t, srv)
	e.add("gmail", store.PresetGmail, nil)
	e.cfg.Raw = e.raw
	e.cfg.Sealer = testSealer(t) // another key: nothing opens
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	New(e.cfg).Run(ctx)
	wantKinds(t, n, notify.TroubleLocked)
}

// A password the row no longer matches says so, and goes nowhere: the
// server named by a row someone rewrote never hears a login. The card
// must not read as a mere chore, or a person seals the password for that
// server by following it (§17.3).
func TestTroubleAltered(t *testing.T) {
	t.Parallel()
	for name, alter := range map[string]string{
		"another server":       `UPDATE account_sources SET username = 'someone-else'`,
		"another sealed value": `UPDATE secrets SET payload = set_byte(payload, 1, get_byte(payload, 1) # 1)`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv, _ := gmailServer(t)
			e, n := troubleEnv(t, srv)
			e.add("gmail", store.PresetGmail, nil)
			if _, err := e.pool.Exec(t.Context(), alter); err != nil {
				t.Fatal(err)
			}
			e.cfg.Raw = e.raw
			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()
			New(e.cfg).Run(ctx)
			wantKinds(t, n, notify.TroubleAltered)
			if logins := srv.count(func() int { return srv.logins }); logins != 0 {
				t.Errorf("%d logins with a password that did not open", logins)
			}
		})
	}
}
