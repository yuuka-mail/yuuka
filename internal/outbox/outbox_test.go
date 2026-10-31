package outbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/textproto"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/contenthash"
	"github.com/yuuka-mail/yuuka/internal/ingest"
	"github.com/yuuka-mail/yuuka/internal/localraw"
	"github.com/yuuka-mail/yuuka/internal/r2"
	"github.com/yuuka-mail/yuuka/internal/r2/r2fake"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/transport"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

const testRaw = "From: me@example.com\r\n" +
	"To: a@example.net\r\n" +
	"Message-ID: <x@example.com>\r\n" +
	"Return-Path: <me@example.com>\r\n" +
	"Subject: hello\r\n" +
	"\r\n" +
	"Hello.\r\n"

// sendFunc scripts one Send of the fake transport.
type sendFunc func(ctx context.Context, env transport.Envelope, raw []byte, commit func(context.Context) error) (*transport.SendResult, error)

type fakeTransport struct {
	caps transport.Caps
	mu   sync.Mutex
	fn   sendFunc
	envs []transport.Envelope
	raws [][]byte
}

func (f *fakeTransport) Caps() transport.Caps { return f.caps }

func (f *fakeTransport) Send(ctx context.Context, env transport.Envelope, raw []byte, commit func(context.Context) error) (*transport.SendResult, error) {
	f.mu.Lock()
	f.envs = append(f.envs, env)
	f.raws = append(f.raws, raw)
	fn := f.fn
	f.mu.Unlock()
	return fn(ctx, env, raw, commit)
}

func (f *fakeTransport) set(fn sendFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fn = fn
}

func (f *fakeTransport) sent() []transport.Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.envs)
}

// reply answers per recipient: an entry "addr": status/code, the rest
// accepted with 250. Past the RCPT stage it commits first, like SMTP.
func reply(per map[string]transport.RecipientResult, final error) sendFunc {
	return func(ctx context.Context, env transport.Envelope, _ []byte, commit func(context.Context) error) (*transport.SendResult, error) {
		res := &transport.SendResult{}
		accepted := 0
		for _, to := range env.To {
			r, ok := per[to]
			if !ok {
				r = transport.RecipientResult{Status: transport.StatusAccepted, Code: 250, Text: "2.0.0 Ok <id@example.com>"}
			}
			r.Addr = to
			if r.Status == transport.StatusAccepted || r.Status == transport.StatusUnknown {
				accepted++
			}
			res.Recipients = append(res.Recipients, r)
		}
		if accepted > 0 {
			if err := commit(ctx); err != nil {
				for i := range res.Recipients {
					res.Recipients[i].Status = transport.StatusDeferred
				}
				res.Outcome = transport.OutcomeFailed
				return res, &transport.Error{Stage: transport.StageCommit, Err: err}
			}
			res.Committed = true
			res.ProviderID = "id@example.com"
			res.MessageID = "<id@example.com>"
		}
		res.Outcome = outcomeOf(res.Recipients)
		if res.Outcome == transport.OutcomeAccepted || res.Outcome == transport.OutcomePartial {
			return res, final
		}
		if final == nil {
			final = errors.New("transport: not delivered")
		}
		return res, final
	}
}

func outcomeOf(rs []transport.RecipientResult) transport.Outcome {
	var a, d, u int
	for _, r := range rs {
		switch r.Status {
		case transport.StatusAccepted:
			a++
		case transport.StatusDeferred:
			d++
		case transport.StatusUnknown:
			u++
		}
	}
	switch {
	case u > 0:
		return transport.OutcomeUnknown
	case a == len(rs):
		return transport.OutcomeAccepted
	case a > 0:
		return transport.OutcomePartial
	case d > 0:
		return transport.OutcomeFailed
	}
	return transport.OutcomeRejected
}

var (
	deferred = transport.RecipientResult{Status: transport.StatusDeferred, Code: 450, Text: "4.2.1 Mailbox busy"}
	rejected = transport.RecipientResult{Status: transport.StatusRejected, Code: 550, Text: "5.1.1 No such user"}
)

type env struct {
	t        *testing.T
	db       *store.DB
	pool     *pgxpool.Pool
	bucket   *r2fake.Bucket
	local    localraw.Dir
	tr       *fakeTransport
	svc      *Service
	identity string
	// operator owns the identities: the default grant lets a human send
	// as their own identity, so each enqueue is approved by Decide.
	operator *auth.Viewer
	clock    *clock
	indexed  []string
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// testRetry makes retry delays easy to recognise: n hours after the n-th
// failure, three failures at most.
var testRetry = store.RetryPolicy{MaxFailures: 3, Delay: func(n int) time.Duration { return time.Duration(n) * time.Hour }}

func newEnv(t *testing.T, mut func(*Config)) *env {
	t.Helper()
	db, pool := storetest.New(t)
	e := &env{
		t: t, db: db, pool: pool, bucket: r2fake.New(), local: localraw.Dir{Root: t.TempDir()},
		tr: &fakeTransport{caps: transport.CloudflareCaps()}, clock: &clock{t: time.Now()},
	}
	e.tr.set(reply(nil, nil))
	e.operator = e.addOperator()
	e.identity = e.addIdentity("me", "cf")
	cfg := Config{
		DB: db, Raw: e.bucket, Local: e.local,
		Transports: map[string]Transport{"cf": e.tr},
		IndexSent: func(ctx context.Context, rawID string) error {
			// Called after the commit: the row is there.
			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE raw_id = $1`, rawID).Scan(&n); err != nil || n != 1 {
				t.Errorf("IndexSent(%s) before the outbox row was committed: %d, %v", rawID, n, err)
			}
			e.indexed = append(e.indexed, rawID)
			return nil
		},
		Retry:        testRetry,
		PollInterval: 20 * time.Millisecond,
		Lease:        time.Minute,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:          e.clock.now,
		Owner:        "test-executor",
	}
	if mut != nil {
		mut(&cfg)
	}
	e.svc = New(cfg)
	return e
}

func (e *env) addOperator() *auth.Viewer {
	e.t.Helper()
	now := time.Now()
	p := store.Principal{ID: ulid.New(now), Kind: "human", Name: "operator", DisplayName: "operator", CreatedAt: now}
	if err := e.db.CreatePrincipal(e.t.Context(), p, store.Action{ID: ulid.New(now), Tool: "principal.create", At: now}); err != nil {
		e.t.Fatal(err)
	}
	return &auth.Viewer{PrincipalID: p.ID, PrincipalKind: p.Kind, PrincipalName: p.Name, Scopes: []auth.Scope{auth.SubmissionSend}}
}

// addIdentity adds prefix@example.com, owned by the operator.
func (e *env) addIdentity(prefix, transportName string) string {
	e.t.Helper()
	id := ulid.New(time.Now())
	if _, err := e.pool.Exec(e.t.Context(), `
		INSERT INTO identities (id, account_id, zone, prefix, owner_id)
		VALUES ($1, $2, 'example.com', $3, $4)`,
		id, storetest.Account(e.t, e.db), prefix, e.operator.PrincipalID); err != nil {
		e.t.Fatal(err)
	}
	storetest.Route(e.t, e.pool, id, transportName)
	return id
}

// submit queues sub from from as v, through the decision of the send
// policy like every submission: there is no other way into the outbox.
func (e *env) submit(v *auth.Viewer, from string, sub Submission) (Enqueued, error) {
	var rcpts []string
	for _, r := range sub.Recipients {
		rcpts = append(rcpts, r.Addr)
	}
	var q Enqueued
	err := e.db.WithDecision(e.t.Context(), v, store.Submission{FromAddr: from, Recipients: rcpts, Now: time.Now()},
		func(ctx context.Context, p *store.PolicyTx) error {
			var err error
			q, err = e.svc.Enqueue(ctx, p, sub)
			return err
		})
	return q, err
}

func to(rcpts ...string) []store.OutboxRecipient {
	var out []store.OutboxRecipient
	for _, r := range rcpts {
		out = append(out, store.OutboxRecipient{Addr: r, Kind: store.RecipientTo})
	}
	return out
}

func (e *env) enqueue(rcpts ...string) Enqueued {
	e.t.Helper()
	q, err := e.submit(e.operator, "me@example.com", Submission{Raw: []byte(testRaw), Recipients: to(rcpts...)})
	if err != nil {
		e.t.Fatal(err)
	}
	return q
}

// step runs one claim-and-send and reports whether a row was due.
func (e *env) step() bool {
	e.t.Helper()
	did, err := e.svc.step(e.t.Context())
	if err != nil {
		e.t.Fatal(err)
	}
	return did
}

func (e *env) due(id string) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.t.Context(), `UPDATE outbox SET next_retry_at = now() WHERE id = $1 AND next_retry_at IS NOT NULL`, id); err != nil {
		e.t.Fatal(err)
	}
}

type row struct {
	state   string
	next    *time.Duration // until next_retry_at, from now
	err     string
	rcpts   map[string]string
	codes   map[string]*int16
	outcome []string // attempt outcomes, oldest first
}

func (e *env) row(id string) row {
	e.t.Helper()
	ctx := e.t.Context()
	var r row
	var nextMS *float64
	if err := e.pool.QueryRow(ctx, `
		SELECT state, extract(epoch FROM next_retry_at - now()) * 1000, coalesce(error, '') FROM outbox WHERE id = $1`, id).
		Scan(&r.state, &nextMS, &r.err); err != nil {
		e.t.Fatal(err)
	}
	if nextMS != nil {
		d := time.Duration(*nextMS) * time.Millisecond
		r.next = &d
	}
	r.rcpts, r.codes = map[string]string{}, map[string]*int16{}
	rows, err := e.pool.Query(ctx, `SELECT addr, state, smtp_code FROM outbox_recipients WHERE outbox_id = $1`, id)
	if err != nil {
		e.t.Fatal(err)
	}
	for rows.Next() {
		var a, s string
		var c *int16
		if err := rows.Scan(&a, &s, &c); err != nil {
			e.t.Fatal(err)
		}
		r.rcpts[a], r.codes[a] = s, c
	}
	rows.Close()
	rows, err = e.pool.Query(ctx, `SELECT coalesce(outcome, 'open') FROM send_attempts WHERE outbox_id = $1 ORDER BY started_at, id`, id)
	if err != nil {
		e.t.Fatal(err)
	}
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			e.t.Fatal(err)
		}
		r.outcome = append(r.outcome, o)
	}
	rows.Close()
	return r
}

func near(d *time.Duration, want time.Duration) bool {
	return d != nil && *d > want-time.Minute && *d <= want+time.Second
}

func TestEnqueue(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	ctx := t.Context()
	q := e.enqueue("Bob@Example.net", "a@example.net", "bob@example.NET", "c@example.org")

	var key, local string
	var size int64
	var sum, ch []byte
	if err := e.pool.QueryRow(ctx, `SELECT r2_key, local_path, size, sha256, content_hash FROM raw_objects WHERE id = $1`, q.RawID).
		Scan(&key, &local, &size, &sum, &ch); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "sent/") || !strings.HasSuffix(key, "/"+q.RawID+".eml") || local != key {
		t.Errorf("key %q, local path %q", key, local)
	}
	stored, err := e.bucket.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	// Cloudflare caps: Message-ID and Return-Path are not ours to set,
	// and the sent copy is what the exit will get.
	want := strings.NewReplacer("Message-ID: <x@example.com>\r\n", "", "Return-Path: <me@example.com>\r\n", "").Replace(testRaw)
	if string(stored.Body) != want {
		t.Errorf("stored %q", stored.Body)
	}
	if b, err := e.local.Get(key); err != nil || !bytes.Equal(b, stored.Body) {
		t.Errorf("local copy %q, %v", b, err)
	}
	wantSum, wantCH := sha256.Sum256([]byte(want)), contenthash.V1([]byte(want))
	if size != int64(len(want)) || !bytes.Equal(sum, wantSum[:]) || !bytes.Equal(ch, wantCH[:]) {
		t.Errorf("raw_objects size %d sha256 %x content_hash %x", size, sum, ch)
	}

	r := e.row(q.OutboxID)
	if r.state != store.OutboxQueued || r.next == nil || *r.next > 0 {
		t.Errorf("row %+v", r)
	}
	// Folded duplicates: the first spelling wins.
	wantRcpts := map[string]string{"Bob@Example.net": "pending", "a@example.net": "pending", "c@example.org": "pending"}
	if fmt.Sprint(r.rcpts) != fmt.Sprint(wantRcpts) {
		t.Errorf("recipients %v", r.rcpts)
	}
	var folds []string
	rows, _ := e.pool.Query(ctx, `SELECT addr_fold FROM outbox_recipients WHERE outbox_id = $1 ORDER BY 1`, q.OutboxID)
	for rows.Next() {
		var f string
		_ = rows.Scan(&f)
		folds = append(folds, f)
	}
	rows.Close()
	if !slices.Equal(folds, []string{"a@example.net", "bob@example.net", "c@example.org"}) {
		t.Errorf("addr_fold %v", folds)
	}
	var changes int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM changes WHERE kind = 'outbox' AND target_id = $1`, q.OutboxID).Scan(&changes); err != nil || changes != 1 {
		t.Errorf("changes %d, %v", changes, err)
	}
	if !slices.Equal(e.indexed, []string{q.RawID}) {
		t.Errorf("indexed %v", e.indexed)
	}

	// Refusals happen before anything is recorded.
	for name, sub := range map[string]Submission{
		"no recipients": {Raw: []byte(testRaw)},
		"bad recipient": {Raw: []byte(testRaw), Recipients: to("a@example.net>\r\nRCPT TO:<b@example.net")},
	} {
		if _, err := e.submit(e.operator, "me@example.com", sub); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Only what was decided on is queued.
	noSend := *e.operator
	noSend.Scopes = nil
	for name, c := range map[string]struct {
		v    *auth.Viewer
		from string
		sub  Submission
	}{
		"held decision":   {&noSend, "me@example.com", Submission{Raw: []byte(testRaw), Recipients: to("a@example.net")}},
		"another From":    {e.operator, "me@example.com", Submission{Raw: []byte(strings.Replace(testRaw, "me@", "you@", 1)), Recipients: to("a@example.net")}},
		"two From fields": {e.operator, "me@example.com", Submission{Raw: []byte("From: me@example.com\r\n" + testRaw), Recipients: to("a@example.net")}},
	} {
		if _, err := e.submit(c.v, c.from, c.sub); !errors.Is(err, store.ErrNotApproved) {
			t.Errorf("%s: %v", name, err)
		}
	}
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d outbox rows after the refusals, %v", n, err)
	}
	e.addIdentity("gone", "gone")
	_, err = e.submit(e.operator, "gone@example.com", Submission{
		Raw: []byte(strings.Replace(testRaw, "me@", "gone@", 1)), Recipients: to("a@example.net"),
	})
	if !errors.Is(err, ErrUnconfigured) {
		t.Errorf("unconfigured transport: %v", err)
	}
}

// landedRetry acts like an R2 PUT that landed but whose response was lost:
// the client's retry sees the key taken.
type landedRetry struct {
	*r2fake.Bucket
	body []byte // what landed; nil means the caller's bytes
}

func (l landedRetry) PutSent(ctx context.Context, key string, body []byte) error {
	landed := body
	if l.body != nil {
		landed = l.body
	}
	if err := l.Bucket.PutSent(ctx, key, landed); err != nil {
		return err
	}
	return fmt.Errorf("put %s: %w", key, r2.ErrExists)
}

func TestEnqueueReadsBackAnExistingCopy(t *testing.T) {
	t.Parallel()
	same := newEnv(t, func(c *Config) { c.Raw = landedRetry{Bucket: r2fake.New()} })
	same.enqueue("a@example.net")

	other := newEnv(t, func(c *Config) {
		c.Raw = landedRetry{Bucket: r2fake.New(), body: []byte("Subject: other\r\n\r\nx\r\n")}
	})
	_, err := other.submit(other.operator, "me@example.com", Submission{Raw: []byte(testRaw), Recipients: to("a@example.net")})
	if err == nil || !strings.Contains(err.Error(), "different content") {
		t.Errorf("different bytes under the key: %v", err)
	}
	var n int
	if err := other.pool.QueryRow(t.Context(), `SELECT count(*) FROM outbox`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d outbox rows after a failed enqueue, %v", n, err)
	}
}

func TestSendAccepted(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	q := e.enqueue("a@example.net", "b@example.net")
	next := reply(nil, nil)
	e.tr.set(func(ctx context.Context, env transport.Envelope, raw []byte, commit func(context.Context) error) (*transport.SendResult, error) {
		// Inside the attempt the row is sending, the attempt open.
		if r := e.row(q.OutboxID); r.state != store.OutboxSending || !slices.Equal(r.outcome, []string{"open"}) {
			t.Errorf("during send: %+v", r)
		}
		return next(ctx, env, raw, func(ctx context.Context) error {
			if err := commit(ctx); err != nil {
				return err
			}
			// committed_at is committed before the commit callback
			// returns: another connection sees it.
			var committed bool
			if err := e.pool.QueryRow(ctx, `SELECT committed_at IS NOT NULL FROM send_attempts WHERE outbox_id = $1`, q.OutboxID).Scan(&committed); err != nil || !committed {
				t.Errorf("committed_at not visible after commit: %v %v", committed, err)
			}
			return nil
		})
	})
	if !e.step() {
		t.Fatal("nothing claimed")
	}
	if e.step() {
		t.Error("claimed a sent row again")
	}
	r := e.row(q.OutboxID)
	if r.state != store.OutboxSent || r.next != nil || r.err != "" || !slices.Equal(r.outcome, []string{"accepted"}) ||
		r.rcpts["a@example.net"] != "accepted" || *r.codes["b@example.net"] != 250 {
		t.Errorf("row %+v", r)
	}
	var provider, msgID string
	if err := e.pool.QueryRow(t.Context(), `SELECT provider_id, message_id FROM send_attempts WHERE outbox_id = $1`, q.OutboxID).
		Scan(&provider, &msgID); err != nil || provider != "id@example.com" || msgID != "<id@example.com>" {
		t.Errorf("provider id %q, message id %q, %v", provider, msgID, err)
	}
	sent := e.tr.sent()
	if len(sent) != 1 || sent[0].From != "me@example.com" || !slices.Equal(sent[0].To, []string{"a@example.net", "b@example.net"}) {
		t.Errorf("envelopes %+v", sent)
	}
	if strings.Contains(string(e.tr.raws[0]), "Message-ID") {
		t.Error("sent a Message-ID through an exit that sets its own")
	}
}

func TestPartialRetriesDeferredOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	q := e.enqueue("ok@example.net", "later@example.net", "bad@example.net")
	e.tr.set(reply(map[string]transport.RecipientResult{"later@example.net": deferred, "bad@example.net": rejected}, nil))
	e.step()
	r := e.row(q.OutboxID)
	if r.state != store.OutboxPartial || !near(r.next, time.Hour) || r.rcpts["later@example.net"] != "deferred" ||
		r.rcpts["bad@example.net"] != "rejected" || *r.codes["bad@example.net"] != 550 || !strings.Contains(r.err, "later@example.net") {
		t.Fatalf("after the first attempt: %+v", r)
	}
	if e.step() {
		t.Fatal("claimed before the backoff ran out")
	}
	e.due(q.OutboxID)
	e.tr.set(reply(nil, nil))
	e.step()
	sent := e.tr.sent()
	if len(sent) != 2 || !slices.Equal(sent[1].To, []string{"later@example.net"}) {
		t.Fatalf("retry went to %+v", sent)
	}
	r = e.row(q.OutboxID)
	// Everyone is settled, one of them rejected: partial for good.
	if r.state != store.OutboxPartial || r.next != nil || r.rcpts["later@example.net"] != "accepted" ||
		!slices.Equal(r.outcome, []string{"partial", "accepted"}) {
		t.Errorf("after the retry: %+v", r)
	}
}

func TestBackoffAndDead(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	q := e.enqueue("a@example.net")
	e.tr.set(reply(map[string]transport.RecipientResult{"a@example.net": deferred}, nil))
	for n := 1; n <= 2; n++ {
		e.step()
		if r := e.row(q.OutboxID); r.state != store.OutboxFailed || !near(r.next, time.Duration(n)*time.Hour) {
			t.Fatalf("after failure %d: %+v", n, r)
		}
		e.due(q.OutboxID)
	}
	e.step()
	r := e.row(q.OutboxID)
	if r.state != store.OutboxDead || r.next != nil || !strings.HasPrefix(r.err, "gave up after 3 failed attempts") ||
		r.rcpts["a@example.net"] != "deferred" {
		t.Errorf("after the cap: %+v", r)
	}

	// A permanent refusal is dead at once.
	q = e.enqueue("bad@example.net")
	e.tr.set(reply(map[string]transport.RecipientResult{"bad@example.net": rejected}, nil))
	e.step()
	if r := e.row(q.OutboxID); r.state != store.OutboxDead || !slices.Equal(r.outcome, []string{"rejected"}) {
		t.Errorf("rejected: %+v", r)
	}
}

func TestBackoffSchedule(t *testing.T) {
	want := []time.Duration{5 * time.Minute, 15 * time.Minute, 45 * time.Minute, 135 * time.Minute, 6 * time.Hour, 6 * time.Hour}
	for i, w := range want {
		if got := Backoff(i + 1); got != w {
			t.Errorf("Backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}

// An exit failure pauses the transport: the message is not attempted
// again until the pause ends, does not burn its retries, and other
// messages for the exit wait too.
func TestTransportPause(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(c *Config) { c.PauseMin, c.PauseMax = 10*time.Minute, 40*time.Minute })
	q1 := e.enqueue("a@example.net")
	e.tr.set(func(context.Context, transport.Envelope, []byte, func(context.Context) error) (*transport.SendResult, error) {
		return &transport.SendResult{Outcome: transport.OutcomeRejected, Recipients: []transport.RecipientResult{{Addr: "a@example.net", Status: transport.StatusRejected, Code: 535}}},
			&transport.Error{Stage: transport.StageAuth, Err: &textproto.Error{Code: 535, Msg: "5.7.8 Authentication credentials invalid"}}
	})
	e.step()
	r := e.row(q1.OutboxID)
	if r.state != store.OutboxQueued || !near(r.next, 10*time.Minute) || r.rcpts["a@example.net"] != "pending" ||
		!slices.Equal(r.outcome, []string{"abandoned"}) || !strings.Contains(r.err, "535") {
		t.Fatalf("after AUTH 535: %+v", r)
	}
	q2 := e.enqueue("b@example.net")
	e.due(q1.OutboxID)
	if e.step() {
		t.Fatal("claimed through a paused transport")
	}
	e.clock.add(11 * time.Minute)
	if !e.step() {
		t.Fatal("pause did not end")
	}
	// Still failing: the pause doubles.
	if d := e.svc.pauses[e.transportID("cf")].until.Sub(e.clock.now()); d != 20*time.Minute {
		t.Errorf("second pause %v", d)
	}
	e.clock.add(21 * time.Minute)
	e.tr.set(reply(nil, nil))
	e.due(q1.OutboxID)
	e.due(q2.OutboxID)
	for e.step() {
	}
	for _, id := range []string{q1.OutboxID, q2.OutboxID} {
		if r := e.row(id); r.state != store.OutboxSent {
			t.Errorf("%s: %+v", id, r)
		}
	}
	if _, ok := e.svc.pauses[e.transportID("cf")]; ok {
		t.Error("pause schedule not reset by a working exit")
	}
}

// The same through the real SMTP transport: connection refused is an
// exit failure.
func TestTransportPauseConnectionRefused(t *testing.T) {
	t.Parallel()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	smtp, err := transport.NewSMTP(transport.Config{Name: "cf", Host: "127.0.0.1", Port: port, Caps: transport.CloudflareCaps()})
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(c *Config) { c.Transports = map[string]Transport{"cf": smtp} })
	q := e.enqueue("a@example.net")
	e.step()
	if r := e.row(q.OutboxID); r.state != store.OutboxQueued || !near(r.next, time.Minute) || !slices.Equal(r.outcome, []string{"abandoned"}) {
		t.Errorf("after connection refused: %+v", r)
	}
	if rs, err := e.svc.resolve(t.Context()); err != nil || len(rs.usable) != 0 || len(rs.live) != 1 {
		t.Errorf("usable %v, live %v, %v", rs.usable, rs.live, err)
	}
}

// A chain without a usable member parks the row: it waits with the reason
// in outbox.error, opens no attempt, and stays due, so the claim after the
// configuration is fixed sends it.
func TestUnconfiguredTransportIsParked(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	q := e.enqueue("a@example.net")
	storetest.Transports(t, e.db, "gone")
	if _, err := e.pool.Exec(t.Context(), `UPDATE identity_transports SET transport_id = (SELECT id FROM transports WHERE name = 'gone')
		WHERE identity_id = $1`, e.identity); err != nil {
		t.Fatal(err)
	}
	if e.step() {
		t.Fatal("claimed a row whose transport is not configured")
	}
	e.svc.housekeep(t.Context())
	r := e.row(q.OutboxID)
	if r.state != store.OutboxQueued || r.next == nil || *r.next > 0 || len(r.outcome) != 0 ||
		r.err != "no usable transport: gone: not configured" {
		t.Errorf("parked: %+v", r)
	}
	e.svc.cfg.Transports["gone"] = e.tr
	if !e.step() {
		t.Fatal("not claimed once its transport was configured")
	}
	if r := e.row(q.OutboxID); r.state != store.OutboxSent || r.err != "" {
		t.Errorf("after configuring: %+v", r)
	}
}

// Crash recovery (§4.5): an attempt whose lease ran out is abandoned and
// queued again without committed_at, and unknown with it.
func TestRecovery(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	ctx := t.Context()
	expire := func(attempt string) {
		t.Helper()
		if _, err := e.pool.Exec(ctx, `UPDATE send_attempts SET lease_until = now() - interval '1 second' WHERE id = $1`, attempt); err != nil {
			t.Fatal(err)
		}
	}
	claim := func() *store.Claimed {
		t.Helper()
		c, err := e.db.ClaimOutbox(ctx, "crashed", storetest.Usable(t, e.db, "cf"), time.Minute)
		if err != nil || c == nil {
			t.Fatalf("claim: %v %v", c, err)
		}
		return c
	}

	// Lost before the commit point.
	q := e.enqueue("a@example.net")
	c := claim()
	expire(c.AttemptID)
	// The late executor can no longer commit, before or after recovery.
	if err := e.db.CommitAttempt(ctx, c.AttemptID, "crashed"); !errors.Is(err, store.ErrLeaseLost) {
		t.Errorf("commit with an expired lease: %v", err)
	}
	rec, err := e.db.RecoverAttempts(ctx, time.Minute)
	if err != nil || len(rec) != 1 || rec[0].Outcome != store.AttemptAbandoned {
		t.Fatalf("recover: %+v %v", rec, err)
	}
	r := e.row(q.OutboxID)
	if r.state != store.OutboxQueued || !near(r.next, time.Minute) || r.rcpts["a@example.net"] != "pending" || !slices.Equal(r.outcome, []string{"abandoned"}) {
		t.Errorf("abandoned: %+v", r)
	}
	if ok, err := e.db.RenewLease(ctx, c.AttemptID, "crashed", time.Minute); ok || err != nil {
		t.Errorf("renewed a recovered lease: %v %v", ok, err)
	}
	// Its late answer is ignored.
	if f, err := e.db.FinishAttempt(ctx, c.AttemptID, store.AttemptResult{Outcome: store.AttemptFailed}, testRetry); err != nil || !f.Late || f.Applied {
		t.Errorf("late answer to an abandoned attempt: %+v %v", f, err)
	}
	e.due(q.OutboxID)
	e.step()
	if r := e.row(q.OutboxID); r.state != store.OutboxSent {
		t.Errorf("resent after abandon: %+v", r)
	}

	// Lost after the commit point: unknown, and never retried by itself.
	q = e.enqueue("b@example.net", "c@example.net")
	c = claim()
	if err := e.db.CommitAttempt(ctx, c.AttemptID, "crashed"); err != nil {
		t.Fatal(err)
	}
	expire(c.AttemptID)
	rec, err = e.db.RecoverAttempts(ctx, time.Minute)
	if err != nil || len(rec) != 1 || rec[0].Outcome != store.AttemptUnknown {
		t.Fatalf("recover: %+v %v", rec, err)
	}
	r = e.row(q.OutboxID)
	if r.state != store.OutboxUnknown || r.next != nil || r.rcpts["b@example.net"] != "unknown" || !slices.Equal(r.outcome, []string{"unknown"}) {
		t.Errorf("unknown: %+v", r)
	}
	if e.step() {
		t.Error("claimed an unknown row")
	}
	// The stuck executor's late answer is reconciliation evidence.
	f, err := e.db.FinishAttempt(ctx, c.AttemptID, store.AttemptResult{
		Outcome: store.AttemptPartial, ProviderID: "late@example.com",
		Recipients: []store.RecipientResult{
			{Addr: "b@example.net", State: store.RecipientAccepted, Code: 250},
			{Addr: "c@example.net", State: store.RecipientDeferred, Code: 451},
		},
	}, testRetry)
	if err != nil || !f.Late || !f.Applied || f.State != store.OutboxPartial {
		t.Fatalf("late result: %+v %v", f, err)
	}
	r = e.row(q.OutboxID)
	if r.rcpts["b@example.net"] != "accepted" || r.rcpts["c@example.net"] != "deferred" || !near(r.next, time.Hour) ||
		!slices.Equal(r.outcome, []string{"partial"}) {
		t.Errorf("after the late result: %+v", r)
	}
}

// Resolutions of unknown (§4.5), each recorded in actions.
func TestResolve(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	ctx := t.Context()
	e.tr.set(reply(map[string]transport.RecipientResult{"a@example.net": {Status: transport.StatusUnknown}}, errors.New("transport: final reply: timeout")))
	unknown := func() string {
		q := e.enqueue("a@example.net")
		for e.step() { // a row resent before is due too
		}
		if r := e.row(q.OutboxID); r.state != store.OutboxUnknown || r.next != nil {
			t.Fatalf("not unknown: %+v", r)
		}
		return q.OutboxID
	}
	action := func() store.Action {
		return store.Action{ID: ulid.New(time.Now()), Actor: store.SystemPrincipalID, Tool: "outbox.resolve", At: time.Now(), Args: map[string]any{"via": "test"}}
	}
	for how, want := range map[string]string{
		store.ResolveSent:    store.OutboxSent,
		store.ResolveResend:  store.OutboxQueued,
		store.ResolveAbandon: store.OutboxDead,
	} {
		id := unknown()
		got, err := e.db.ResolveOutbox(ctx, id, how, action(), testRetry)
		if err != nil || got != want {
			t.Errorf("%s: %s, %v", how, got, err)
		}
		r := e.row(id)
		if r.state != want {
			t.Errorf("%s: %+v", how, r)
		}
		var args map[string]any
		if err := e.pool.QueryRow(ctx, `SELECT args FROM actions WHERE tool = 'outbox.resolve' AND args->>'outbox_id' = $1`, id).Scan(&args); err != nil ||
			args["resolution"] != how || args["from"] != "unknown" || args["to"] != want || args["via"] != "test" {
			t.Errorf("%s: action %v, %v", how, args, err)
		}
		if how == store.ResolveResend && (r.rcpts["a@example.net"] != "pending" || r.next == nil) {
			t.Errorf("resend: %+v", r)
		}
		if _, err := e.db.ResolveOutbox(ctx, id, how, action(), testRetry); !errors.Is(err, store.ErrOutboxState) {
			t.Errorf("%s twice: %v", how, err)
		}
	}
	if _, err := e.db.ResolveOutbox(ctx, ulid.New(time.Now()), store.ResolveSent, action(), testRetry); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing row: %v", err)
	}
}

// Claims are exclusive under concurrency: every row is claimed once.
func TestClaimExclusive(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	const n = 12
	for i := range n {
		e.enqueue(fmt.Sprintf("r%d@example.net", i))
	}
	usable := storetest.Usable(t, e.db, "cf")
	var mu sync.Mutex
	claimed := map[string]int{}
	var wg sync.WaitGroup
	for w := range 6 {
		wg.Go(func() {
			for {
				c, err := e.db.ClaimOutbox(t.Context(), fmt.Sprintf("w%d", w), usable, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if c == nil {
					return
				}
				mu.Lock()
				claimed[c.OutboxID]++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(claimed) != n {
		t.Errorf("claimed %d of %d rows", len(claimed), n)
	}
	for id, k := range claimed {
		if k != 1 {
			t.Errorf("%s claimed %d times", id, k)
		}
	}
}

// Run sends what is queued, and on shutdown abandons an attempt that has
// not reached its commit point rather than leaving it to recovery.
func TestRunAndShutdown(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.svc.Run(ctx)
	}()
	q := e.enqueue("a@example.net")
	waitFor(t, func() bool { return e.row(q.OutboxID).state == store.OutboxSent })

	started := make(chan struct{})
	e.tr.set(func(sctx context.Context, env transport.Envelope, raw []byte, commit func(context.Context) error) (*transport.SendResult, error) {
		close(started)
		<-sctx.Done() // shutdown cancels an attempt short of the commit point
		if err := commit(context.WithoutCancel(sctx)); err == nil {
			t.Error("committed while shutting down")
		}
		return &transport.SendResult{
			Outcome:    transport.OutcomeFailed,
			Recipients: []transport.RecipientResult{{Addr: env.To[0], Status: transport.StatusDeferred}},
		}, &transport.Error{Stage: transport.StageData, Err: sctx.Err()}
	})
	q = e.enqueue("b@example.net")
	<-started
	cancel()
	<-done
	r := e.row(q.OutboxID)
	if r.state != store.OutboxQueued || r.next == nil || *r.next > time.Second || !slices.Equal(r.outcome, []string{"abandoned"}) ||
		r.rcpts["b@example.net"] != "pending" {
		t.Errorf("after shutdown: %+v", r)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestOutboxListNeedsViewer(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	q := e.enqueue("a@example.net", "b@example.net")
	if _, err := e.db.OutboxList(t.Context(), nil, store.OutboxQuery{}); err == nil {
		t.Error("listed without a viewer")
	}
	v := &auth.Viewer{PrincipalID: store.SystemPrincipalID, PrincipalKind: "system", PrincipalName: "system"}
	rows, err := e.db.OutboxList(t.Context(), v, store.OutboxQuery{State: store.OutboxQueued})
	if err != nil || len(rows) != 1 || rows[0].ID != q.OutboxID || len(rows[0].Recipients) != 2 || rows[0].Attempts != 0 ||
		!strings.HasSuffix(rows[0].Identity, "@example.com") {
		t.Errorf("list: %+v %v", rows, err)
	}
	if rows, err := e.db.OutboxList(t.Context(), v, store.OutboxQuery{State: store.OutboxSent}); err != nil || len(rows) != 0 {
		t.Errorf("list sent: %+v %v", rows, err)
	}
}

// The enqueuer's sent copy is what the indexer expects (§4.6): wired to
// ingest.IndexSent, the message exists right after Enqueue, linked by the
// fold key.
func TestEnqueueIndexesTheSentCopy(t *testing.T) {
	t.Parallel()
	var in *ingest.Ingester
	e := newEnv(t, func(c *Config) {
		in = ingest.New(ingest.Config{Raw: c.Raw.(*r2fake.Bucket), Local: c.Local, DB: c.DB, Log: c.Log})
		c.IndexSent = func(ctx context.Context, rawID string) error { return in.IndexSent(ctx, rawID) }
	})
	q := e.enqueue("a@example.net")
	var subject string
	err := e.pool.QueryRow(t.Context(), `
		SELECT m.subject FROM messages m JOIN raw_objects r ON r.content_hash = m.content_hash WHERE r.id = $1`, q.RawID).Scan(&subject)
	if err != nil || subject != "hello" {
		t.Errorf("message of the sent copy: %q, %v", subject, err)
	}
}
