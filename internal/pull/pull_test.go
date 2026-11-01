package pull

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/ingest"
	"github.com/yuuka-mail/yuuka/internal/localraw"
	"github.com/yuuka-mail/yuuka/internal/notify"
	"github.com/yuuka-mail/yuuka/internal/r2"
	"github.com/yuuka-mail/yuuka/internal/r2/r2fake"
	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// env is a database with one pull account on a fake server, and a worker
// for it that the test drives one round at a time.
type env struct {
	t      testing.TB
	db     *store.DB
	pool   *pgxpool.Pool
	srv    *fakeServer
	bucket *r2fake.Bucket
	raw    RawBucket
	local  localraw.Dir
	in     *ingest.Ingester
	sealer *secret.Sealer
	cfg    Config

	mu       sync.Mutex
	now      time.Time
	notified []string // summaries
	kicked   []string // messages queued for scoring
	// afterFold, if set, runs after each content step.
	afterFold func()
}

func (e *env) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = e.now.Add(d)
}

func (e *env) NewMail(_ context.Context, m notify.Mail) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.notified = append(e.notified, m.Text)
	return nil
}

// content wraps the ingester, as serve hands it to the engine.
type content struct{ e *env }

func (c content) FoldPulled(ctx context.Context, ref store.RawRef, raw []byte, score bool) (store.Folded, error) {
	f, err := c.e.in.FoldPulled(ctx, ref, raw, score)
	if c.e.afterFold != nil {
		c.e.afterFold()
	}
	return f, err
}

func (c content) Notify(ctx context.Context, m notify.Mail) { c.e.in.Notify(ctx, m) }

func newEnv(t testing.TB, srv *fakeServer) *env {
	t.Helper()
	db, pool := storetest.New(t)
	e := &env{
		t: t, db: db, pool: pool, srv: srv, bucket: r2fake.New(), local: localraw.Dir{Root: t.TempDir()},
		now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), sealer: testSealer(t),
	}
	e.raw = e.bucket
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	e.in = ingest.New(ingest.Config{
		Raw: e.bucket, Local: e.local, DB: db, Account: "cloudflare", TrustedAuthservID: "mx.cloudflare.net",
		Notifier: e, Log: quiet, Now: e.clock,
		NewMessage: func(id string) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.kicked = append(e.kicked, id)
		},
	})
	e.cfg = Config{
		DB: db, Local: e.local, Sealer: e.sealer, Content: content{e}, TriageAccounts: []string{"gmail"},
		Log: quiet, Now: e.clock, RootCAs: srv.pool, BackfillSlice: time.Hour, RequestPoll: time.Millisecond,
		Interval: 10 * time.Millisecond,
	}
	return e
}

func testSealer(t testing.TB) *secret.Sealer {
	t.Helper()
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	key, err := secret.ParseKey([]byte(base64.StdEncoding.EncodeToString(kek)))
	if err != nil {
		t.Fatal(err)
	}
	ring, err := secret.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	s, err := secret.NewSealer(ring)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// add makes the pull account name on the fake server, its address
// claimed so its mail escapes the §4.3 downgrade, and returns it.
func (e *env) add(name, preset string, edit func(*store.SourceConfig)) store.Source {
	e.t.Helper()
	ctx := e.t.Context()
	at := e.clock()
	op := store.Principal{ID: ulid.New(at), Kind: "human", Name: "operator", DisplayName: "operator", CreatedAt: at}
	if _, err := e.db.PrincipalByName(ctx, "human", "operator"); errors.Is(err, store.ErrNotFound) {
		if err := e.db.CreatePrincipal(ctx, op, store.Action{ID: ulid.New(at), Tool: "principal.create", At: at}); err != nil {
			e.t.Fatal(err)
		}
	} else if p, err := e.db.PrincipalByName(ctx, "human", "operator"); err == nil {
		op = p
	}
	c := store.SourceConfig{
		Address: "me@example.com", Host: "127.0.0.1", Port: e.srv.addr.Port, Auth: store.SourceAuthPlain,
		Username: e.srv.user, Preset: preset, MaxMessageBytes: 1 << 20, PollSeconds: 60,
	}
	if edit != nil {
		edit(&c)
	}
	aud, err := secret.IMAPS(c.Username, c.Host, c.Port)
	if err != nil {
		e.t.Fatal(err)
	}
	sealed, err := e.sealer.Seal(ctx, ulid.New(at), store.SecretIMAPPassword, aud, []byte(e.srv.pass))
	if err != nil {
		e.t.Fatal(err)
	}
	src, err := e.db.AddPullAccount(ctx, name, c, sealed, op.ID, store.Action{ID: ulid.New(at), Actor: op.ID, Tool: "account.add", At: at})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO prefixes (zone, prefix, first_seen, last_seen, state)
		VALUES ('example.com', 'me', $1, $1, 1) ON CONFLICT (zone, prefix) DO UPDATE SET state = 1`, at); err != nil {
		e.t.Fatal(err)
	}
	return src
}

// worker is a worker for the account's current revision, its password
// open, as the manager starts it.
func (e *env) worker(name string) *worker {
	e.t.Helper()
	ss, err := e.db.EnabledSources(e.t.Context())
	if err != nil {
		e.t.Fatal(err)
	}
	e.cfg.Raw = e.raw
	e.cfg.defaults()
	for _, s := range ss {
		if s.AccountName == name {
			w := &worker{cfg: &e.cfg, rounds: make(chan struct{}, 1), src: s, log: e.cfg.Log}
			if !w.open(e.t.Context()) {
				e.t.Fatal("password does not open")
			}
			e.t.Cleanup(w.password.Close)
			w.reconciled = map[string]time.Time{}
			w.scored = s.AccountName == "gmail"
			return w
		}
	}
	e.t.Fatalf("no enabled source %s", name)
	return nil
}

// round runs one round and waits for its notifications.
func (e *env) round(w *worker, retry bool) time.Duration {
	e.t.Helper()
	d := w.round(e.t.Context(), retry)
	e.in.Wait()
	return d
}

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.t.Context(), q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) mailbox(account, name string) store.Mailbox {
	e.t.Helper()
	ms, err := e.db.SourceMailboxes(e.t.Context(), account)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, m := range ms {
		if m.Name == name {
			return m
		}
	}
	e.t.Fatalf("no mailbox %s", name)
	return store.Mailbox{}
}

func (e *env) snapshot() (notified, kicked int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.notified), len(e.kicked)
}

// letter is a synthetic message.
func letter(n int, subject string) []byte {
	return fmt.Appendf(nil, "From: Sender %d <sender%d@example.net>\r\nTo: me@example.com\r\nSubject: %s\r\n"+
		"Message-ID: <%d@example.net>\r\nDate: Sun, 01 Nov 2026 10:00:00 +0000\r\n\r\nBody %d.\r\n", n, n, subject, n, n)
}

func gmailServer(t testing.TB) (*fakeServer, *fakeFolder) {
	srv := newFakeServer(t, "X-GM-EXT-1", "LIST-STATUS", "LIST-EXTENDED", "SPECIAL-USE", "UNSELECT", "ID")
	srv.folder("INBOX", 1, `\HasNoChildren`)
	all := srv.folder("[Gmail]/All Mail", 11, `\HasNoChildren`, `\All`)
	srv.folder("[Gmail]/Spam", 12, `\HasNoChildren`, `\Junk`)
	return srv, all
}

// A Gmail mailbox (§10.1): only \All is pulled; what is there on the
// first sync is backfill, which neither notifies nor queues for scoring;
// drafts are counted, not stored; labels are kept; new mail notifies
// unless it is old, \Sent or our own; an unchanged folder is not opened.
func TestGmail(t *testing.T) {
	t.Parallel()
	srv, all := gmailServer(t)
	e := newEnv(t, srv)
	src := e.add("gmail", store.PresetGmail, nil)
	now := e.clock()
	srv.add(all, fakeMsg{raw: letter(1, "old one"), date: now.Add(-48 * time.Hour), gmID: 0x101, labels: []string{`\Inbox`, "Travel"}})
	srv.add(all, fakeMsg{raw: letter(2, "a draft"), date: now.Add(-time.Hour), gmID: 0x102, flags: []string{`\Draft`}, labels: []string{`\Draft`}})
	srv.add(all, fakeMsg{raw: letter(3, "recent"), date: now.Add(-time.Hour), gmID: 0x103, labels: []string{`\Inbox`}})
	w := e.worker("gmail")

	if d := e.round(w, false); d != time.Minute {
		t.Fatalf("first round asks to wait %v", d)
	}
	m := e.mailbox(src.AccountID, "[Gmail]/All Mail")
	if m.KeyKind != store.KeyGmail || m.High != 3 || m.Low != 1 || m.DraftsSkipped != 1 || m.UIDNextSeen != 4 || m.SpecialUse != `\All` {
		t.Errorf("after the first sync: %+v", m)
	}
	if n := e.count(`SELECT count(*) FROM deliveries WHERE account_id = $1`, src.AccountID); n != 2 {
		t.Errorf("%d deliveries, want 2 (the draft is not stored)", n)
	}
	if n := e.count(`SELECT count(*) FROM deliveries WHERE source_key = $1 AND remote_labels = '{"\\Inbox",Travel}'`, store.GmailSourceKey(0x101)); n != 1 {
		t.Error("labels not kept")
	}
	if n := e.count(`SELECT count(*) FROM source_mailboxes WHERE name = 'INBOX' OR name LIKE '%Spam'`); n != 0 {
		t.Error("a folder other than \\All was synced")
	}
	if n, k := e.snapshot(); n != 0 || k != 0 {
		t.Errorf("backfill notified %d, queued %d", n, k)
	}
	if n := e.count(`SELECT count(*) FROM deliveries d JOIN messages m ON m.id = d.message_id WHERE d.account_id = $1`, src.AccountID); n != 2 {
		t.Error("the content step did not run")
	}
	if used, _ := e.db.AddBudget(t.Context(), src.AccountID, now, 0); used != int64(len(letter(1, "old one"))+len(letter(3, "recent"))) {
		t.Errorf("budget used %d", used)
	}

	// New mail: one to notify, one sent from Gmail, one the server says
	// arrived two days ago.
	srv.add(all, fakeMsg{raw: letter(4, "new"), date: now.Add(-time.Minute), gmID: 0x104, labels: []string{`\Inbox`}})
	srv.add(all, fakeMsg{raw: letter(5, "sent by me"), date: now.Add(-time.Minute), gmID: 0x105, labels: []string{`\Sent`}})
	srv.add(all, fakeMsg{raw: letter(6, "late"), date: now.Add(-49 * time.Hour), gmID: 0x106, labels: []string{`\Inbox`}})
	e.round(w, false)
	e.mu.Lock()
	notified, kicked := append([]string(nil), e.notified...), len(e.kicked)
	e.mu.Unlock()
	if len(notified) != 1 || !strings.Contains(notified[0], "new") {
		t.Errorf("notified %q, want only the new one", notified)
	}
	if kicked != 3 {
		t.Errorf("%d queued for scoring, want the 3 new ones", kicked)
	}
	if m := e.mailbox(src.AccountID, "[Gmail]/All Mail"); m.High != 6 || m.UIDNextSeen != 7 {
		t.Errorf("after catch-up: %+v", m)
	}

	// Nothing changed: the folder is not opened.
	before := srv.count(func() int { return srv.examines })
	e.round(w, false)
	if after := srv.count(func() int { return srv.examines }); after != before {
		t.Errorf("an unchanged folder was opened (%d EXAMINEs)", after-before)
	}
	for uid, n := range srv.bodies {
		if n != 1 {
			t.Errorf("UID %d fetched %d times", uid, n)
		}
	}
}

// Too large: the header only, marked, and nothing over the limit is read
// (§10.1.2).
func TestOversize(t *testing.T) {
	t.Parallel()
	srv, all := gmailServer(t)
	e := newEnv(t, srv)
	src := e.add("gmail", store.PresetGmail, func(c *store.SourceConfig) { c.MaxMessageBytes = 512 })
	w := e.worker("gmail")
	e.round(w, false)
	big := append(letter(1, "large"), bytes.Repeat([]byte("attachment line\r\n"), 200)...)
	srv.add(all, fakeMsg{raw: big, date: e.clock(), gmID: 1})
	// A server that understates a size: a little, and the header is
	// asked for after all; a lot, and the literal limit cuts it off
	// unread, and the message is recorded.
	little := srv.add(all, fakeMsg{raw: big, date: e.clock(), gmID: 2})
	huge := srv.add(all, fakeMsg{raw: append(letter(3, "huge"), bytes.Repeat([]byte("x"), 100<<10)...), date: e.clock(), gmID: 3})
	srv.set(func() { srv.lieSize[little], srv.lieSize[huge] = 100, 100 })
	e.round(w, false)
	if n := e.count(`SELECT count(*) FROM deliveries d JOIN raw_objects r ON r.id = d.raw_id
		WHERE d.account_id = $1 AND d.remote_omitted = 1 AND r.completeness = 2 AND r.content_hash IS NULL AND r.size < 512`, src.AccountID); n != 2 {
		t.Errorf("%d header-only deliveries, want 2", n)
	}
	fails, err := e.db.PullFailures(t.Context(), src.AccountID, false)
	if err != nil || len(fails) != 1 || fails[0].UID != huge {
		t.Errorf("failures %+v, %v", fails, err)
	}
	if m := e.mailbox(src.AccountID, "[Gmail]/All Mail"); m.High != 2 {
		t.Errorf("cursor at %d, want held below the failing UID", m.High)
	}
}

// A crash after the commit and before the cursor moved: the next round
// skips the committed mail by its key and fetches only the rest
// (§10.1.3). A crash after the raw bucket write and before the commit
// leaves an orphan object, never a lost message.
func TestCrash(t *testing.T) {
	t.Parallel()
	srv, all := gmailServer(t)
	e := newEnv(t, srv)
	src := e.add("gmail", store.PresetGmail, nil)
	w := e.worker("gmail")
	e.round(w, false)
	for i := range 3 {
		srv.add(all, fakeMsg{raw: letter(i, "new"), date: e.clock(), gmID: uint64(10 + i)})
	}
	ctx, cancel := context.WithCancel(t.Context())
	e.afterFold = cancel // the first message is committed and folded, then the process dies
	w.round(ctx, false)
	e.in.Wait()
	e.afterFold = nil
	if m := e.mailbox(src.AccountID, "[Gmail]/All Mail"); m.High != 0 {
		t.Fatalf("cursor moved to %d", m.High)
	}
	if n := e.count(`SELECT count(*) FROM deliveries`); n != 1 {
		t.Fatalf("%d deliveries after the crash", n)
	}

	// The raw bucket write lands and the commit never happens.
	e.raw = &landThenFail{Bucket: e.bucket}
	w2 := e.worker("gmail")
	e.round(w2, false)
	e.raw = e.bucket
	w3 := e.worker("gmail")
	e.round(w3, false)
	if n := e.count(`SELECT count(*) FROM deliveries`); n != 3 {
		t.Errorf("%d deliveries, want 3", n)
	}
	if m := e.mailbox(src.AccountID, "[Gmail]/All Mail"); m.High != 3 {
		t.Errorf("cursor at %d", m.High)
	}
	if n := srv.count(func() int { return srv.bodies[1] }); n != 1 {
		t.Errorf("the committed message was fetched %d times", n)
	}
	objects := 0
	for _, err := range e.bucket.List(t.Context(), "pull/") {
		if err != nil {
			t.Fatal(err)
		}
		objects++
	}
	if objects != 4 || e.count(`SELECT count(*) FROM raw_objects`) != 3 {
		t.Errorf("%d pull/ objects, want 3 and the orphan", objects)
	}
}

// landThenFail stores the first object and then reports failure, as a
// crash between the write and the commit would leave it.
type landThenFail struct {
	*r2fake.Bucket
	done bool
}

func (l *landThenFail) PutPull(ctx context.Context, key string, body []byte, meta r2.PullMeta) error {
	if err := l.Bucket.PutPull(ctx, key, body, meta); err != nil || l.done {
		return err
	}
	l.done = true
	return errors.New("crash after the write")
}

// A refused login pauses the source and is not retried (§10.1.3): a
// wrong password repeated gets a mailbox locked. The manager starts no
// worker for it.
func TestLoginRefused(t *testing.T) {
	t.Parallel()
	srv, _ := gmailServer(t)
	srv.refuse = true
	e := newEnv(t, srv)
	e.add("gmail", store.PresetGmail, nil)
	w := e.worker("gmail")
	e.round(w, false)
	s, err := e.db.SourceByName(t.Context(), "gmail")
	if err != nil || s.PauseReason != store.PauseAuthFailed {
		t.Fatalf("source %+v, %v", s, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	New(e.cfg).Run(ctx)
	if n := srv.count(func() int { return srv.logins }); n != 1 {
		t.Errorf("%d logins, want the one that was refused", n)
	}
}

// Without a key nothing is pulled and nothing crashes (§17.3).
func TestLocked(t *testing.T) {
	t.Parallel()
	srv, all := gmailServer(t)
	e := newEnv(t, srv)
	e.add("gmail", store.PresetGmail, nil)
	srv.add(all, fakeMsg{raw: letter(1, "x"), date: e.clock(), gmID: 1})
	var logs bytes.Buffer
	cfg := e.cfg
	cfg.Sealer = nil
	cfg.Raw = e.bucket
	cfg.Log = slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	New(cfg).Run(ctx)
	if n := srv.count(func() int { return srv.logins }); n != 0 {
		t.Errorf("%d logins while locked", n)
	}
	if n := strings.Count(logs.String(), "locked"); n != 1 {
		t.Errorf("locked said %d times:\n%s", n, logs.String())
	}
}

// The manager runs a worker that pulls, and a sync request is taken.
func TestManager(t *testing.T) {
	t.Parallel()
	srv, all := gmailServer(t)
	e := newEnv(t, srv)
	src := e.add("gmail", store.PresetGmail, nil)
	srv.add(all, fakeMsg{raw: letter(1, "x"), date: e.clock(), gmID: 1})
	e.cfg.Raw = e.bucket
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		New(e.cfg).Run(ctx)
	}()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(10 * time.Second)
	for e.count(`SELECT count(*) FROM deliveries WHERE account_id = $1`, src.AccountID) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the manager pulled nothing")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// `account sync` cuts the wait short.
	srv.add(all, fakeMsg{raw: letter(2, "y"), date: e.clock(), gmID: 2})
	if err := e.db.RequestSync(t.Context(), "gmail", false, e.clock()); err != nil {
		t.Fatal(err)
	}
	for e.count(`SELECT count(*) FROM deliveries WHERE account_id = $1`, src.AccountID) != 2 {
		if time.Now().After(deadline) {
			t.Fatal("the sync request was not taken")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A message the server keeps refusing holds the cursor until it has
// failed MaxAttempts times, then is passed with its failure recorded,
// never silently; --retry-failed takes it once it can be fetched.
func TestFailures(t *testing.T) {
	t.Parallel()
	srv, all := gmailServer(t)
	e := newEnv(t, srv)
	e.cfg.MaxAttempts = 2
	src := e.add("gmail", store.PresetGmail, nil)
	w := e.worker("gmail")
	e.round(w, false)
	srv.add(all, fakeMsg{raw: letter(1, "a"), date: e.clock(), gmID: 1})
	bad := srv.add(all, fakeMsg{raw: letter(2, "b"), date: e.clock(), gmID: 2})
	srv.add(all, fakeMsg{raw: letter(3, "c"), date: e.clock(), gmID: 3})
	srv.set(func() { srv.noBody[bad] = true })

	e.round(w, false)
	if m := e.mailbox(src.AccountID, "[Gmail]/All Mail"); m.High != 1 {
		t.Errorf("cursor at %d after one failure, want 1", m.High)
	}
	if n := e.count(`SELECT count(*) FROM deliveries`); n != 2 {
		t.Errorf("%d deliveries: the mail after a failing one must still arrive", n)
	}
	e.round(w, false)
	if m := e.mailbox(src.AccountID, "[Gmail]/All Mail"); m.High != 3 {
		t.Errorf("cursor at %d after MaxAttempts, want past it", m.High)
	}
	fails, _ := e.db.PullFailures(t.Context(), src.AccountID, false)
	if len(fails) != 1 || fails[0].UID != bad || fails[0].Attempts != 2 {
		t.Fatalf("failures %+v", fails)
	}
	// Passed, it is not tried again by itself.
	e.round(w, false)
	if fails, _ := e.db.PullFailures(t.Context(), src.AccountID, false); fails[0].Attempts != 2 {
		t.Errorf("a passed failure was retried unasked: %+v", fails[0])
	}
	srv.set(func() { delete(srv.noBody, bad) })
	e.round(w, true)
	if fails, _ := e.db.PullFailures(t.Context(), src.AccountID, false); len(fails) != 0 {
		t.Errorf("open failures after the retry: %+v", fails)
	}
	if n := e.count(`SELECT count(*) FROM deliveries`); n != 3 {
		t.Errorf("%d deliveries after the retry", n)
	}
}

// The daily budget (§10.1.3): backfill uses at most nine tenths of it,
// catch-up the rest; then nothing until the next UTC day. A server's
// own download limit stops the same way, with no failure counted.
func TestBudget(t *testing.T) {
	t.Parallel()
	srv, all := gmailServer(t)
	e := newEnv(t, srv)
	size := len(letter(10, "backfill"))
	src := e.add("gmail", store.PresetGmail, func(c *store.SourceConfig) { c.DailyByteBudget = int64(10 * size) })
	for i := range 20 {
		srv.add(all, fakeMsg{raw: letter(10+i, "backfill"), date: e.clock().Add(-time.Hour), gmID: uint64(100 + i)})
	}
	w := e.worker("gmail")
	e.round(w, false)
	if n := e.count(`SELECT count(*) FROM deliveries`); n != 9 {
		t.Errorf("backfill took %d, want 9 of a budget of 10", n)
	}
	for i := range 3 {
		srv.add(all, fakeMsg{raw: letter(40+i, "backfill"), date: e.clock(), gmID: uint64(200 + i)})
	}
	if d := e.round(w, false); d < time.Hour {
		t.Errorf("waits %v after the budget ran out", d)
	}
	if n := e.count(`SELECT count(*) FROM deliveries`); n != 10 {
		t.Errorf("%d deliveries, want the last tenth spent on new mail", n)
	}
	logins := srv.count(func() int { return srv.logins })
	e.round(w, false)
	if srv.count(func() int { return srv.logins }) != logins {
		t.Error("a round ran with the budget spent")
	}
	e.advance(24 * time.Hour)
	e.round(w, false)
	// Catch-up first (2), then backfill up to its share again (7).
	if n := e.count(`SELECT count(*) FROM deliveries`); n != 19 {
		t.Errorf("%d deliveries the next day, want 19", n)
	}
	if m := e.mailbox(src.AccountID, "[Gmail]/All Mail"); m.High != 23 {
		t.Errorf("cursor %+v", m)
	}

	// The server's limit.
	srv.set(func() { srv.overquota = true })
	srv.add(all, fakeMsg{raw: letter(50, "x"), date: e.clock(), gmID: 300})
	if d := e.round(w, false); d < time.Hour {
		t.Errorf("waits %v after the server's limit", d)
	}
	if fails, _ := e.db.PullFailures(t.Context(), src.AccountID, false); len(fails) != 0 {
		t.Errorf("a server limit counted as failures: %+v", fails)
	}
}

// UIDVALIDITY changes under a Gmail folder (§10.1.3): the deliveries are
// relocated by their stable keys, no body is fetched again, and new mail
// arrives as usual.
func TestUIDValidityGmail(t *testing.T) {
	t.Parallel()
	srv, all := gmailServer(t)
	e := newEnv(t, srv)
	src := e.add("gmail", store.PresetGmail, nil)
	for i := range 3 {
		srv.add(all, fakeMsg{raw: letter(i, "x"), date: e.clock(), gmID: uint64(1 + i)})
	}
	w := e.worker("gmail")
	e.round(w, false)
	srv.renumber(all, 99, 50)
	srv.add(all, fakeMsg{raw: letter(9, "new"), date: e.clock(), gmID: 9})
	fetched := len(srv.bodies)
	e.round(w, false)
	if n := e.count(`SELECT count(*) FROM deliveries WHERE remote_uidvalidity = 99 AND remote_uid BETWEEN 50 AND 53`); n != 4 {
		t.Errorf("%d deliveries at the new location, want 4", n)
	}
	if got := srv.count(func() int { return len(srv.bodies) }); got != fetched+1 {
		t.Errorf("fetched %d bodies after the change, want only the new one", got-fetched)
	}
	if m := e.mailbox(src.AccountID, "[Gmail]/All Mail"); m.UIDValidity != 99 || m.Low != 1 {
		t.Errorf("mailbox %+v", m)
	}
}

// UIDVALIDITY changes on a server without stable ids: the folder is
// downloaded again and matched by sha256, each delivery's location and
// key rewritten in place, the bytes not stored twice (§10.1.3).
func TestUIDValidityLocation(t *testing.T) {
	t.Parallel()
	srv := newFakeServer(t, "LIST-STATUS", "LIST-EXTENDED", "SPECIAL-USE")
	inbox := srv.folder("INBOX", 5)
	srv.folder("Trash", 6, `\Trash`)
	e := newEnv(t, srv)
	src := e.add("work", "", nil)
	for i := range 3 {
		srv.add(inbox, fakeMsg{raw: letter(i, "x"), date: e.clock()})
	}
	w := e.worker("work")
	e.round(w, false)
	if n := e.count(`SELECT count(*) FROM deliveries WHERE source_key LIKE 'loc:5:%:INBOX'`); n != 3 {
		t.Fatalf("%d location-keyed deliveries", n)
	}
	srv.renumber(inbox, 6, 1)
	srv.add(inbox, fakeMsg{raw: letter(9, "new"), date: e.clock()})
	e.round(w, false)
	if n := e.count(`SELECT count(*) FROM deliveries WHERE source_key LIKE 'loc:6:%:INBOX' AND remote_uidvalidity = 6`); n != 4 {
		t.Errorf("%d deliveries under the new UIDVALIDITY, want 4", n)
	}
	if n := e.count(`SELECT count(*) FROM raw_objects`); n != 4 {
		t.Errorf("%d raw objects: the rescanned bytes were stored again", n)
	}
	if m := e.mailbox(src.AccountID, "INBOX"); m.RescanUIDValidity != 0 || m.Low != 1 {
		t.Errorf("rescan not finished: %+v", m)
	}
}

// OBJECTID servers key by EMAILID.
func TestObjectID(t *testing.T) {
	t.Parallel()
	srv := newFakeServer(t, "OBJECTID")
	inbox := srv.folder("INBOX", 5)
	e := newEnv(t, srv)
	e.add("work", "", nil)
	srv.add(inbox, fakeMsg{raw: letter(1, "x"), date: e.clock(), emailID: "M6d99ac3275bb4e"})
	e.round(e.worker("work"), false)
	if n := e.count(`SELECT count(*) FROM deliveries WHERE source_key = 'oid:M6d99ac3275bb4e'`); n != 1 {
		t.Error("not keyed by EMAILID")
	}
}

// The weekly reconciliation takes mail below the cursor that has no
// delivery: a server that once said less than it had (§10.1.3).
func TestReconcile(t *testing.T) {
	t.Parallel()
	srv, all := gmailServer(t)
	e := newEnv(t, srv)
	src := e.add("gmail", store.PresetGmail, nil)
	for i := range 3 {
		srv.add(all, fakeMsg{raw: letter(i, "x"), date: e.clock(), gmID: uint64(1 + i)})
	}
	w := e.worker("gmail")
	e.round(w, false)
	// The cursor passed a message: the server said UIDNEXT 6 with UID
	// 4 not yet visible, say.
	srv.add(all, fakeMsg{raw: letter(7, "hidden"), date: e.clock(), gmID: 77})
	srv.set(func() { all.uidnext = 6 })
	if _, err := e.db.AdvanceHigh(t.Context(), src.AccountID, "[Gmail]/All Mail", 11, 5, 0); err != nil {
		t.Fatal(err)
	}
	e.round(w, false)
	if n := e.count(`SELECT count(*) FROM deliveries`); n != 3 {
		t.Fatalf("reconciled before it was due: %d", n)
	}
	e.advance(8 * 24 * time.Hour)
	e.round(w, false)
	if n := e.count(`SELECT count(*) FROM deliveries WHERE source_key = $1`, store.GmailSourceKey(77)); n != 1 {
		t.Error("the reconciliation did not take the missing message")
	}
	if n, _ := e.snapshot(); n != 0 {
		t.Error("the reconciliation notified")
	}
}
