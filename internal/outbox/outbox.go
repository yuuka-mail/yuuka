// Package outbox puts outgoing mail on the outbox (§4.6) and runs the
// executor that sends it through the §4.5 state machine.
//
// Enqueue stores the bytes first, write-once under sent/ (R2 and the local
// copy), and then records the sent copy, the outbox row and its recipients
// through a store.PolicyTx, in the transaction of the decision that let
// the message go. The executor claims due rows, hands the bytes to the
// identity's transport with a commit callback that records committed_at
// before the point of no return, and records what came back. Whether a
// failure may be retried is decided only by what was recorded, never by
// whether a process is still alive: see store.RecoverAttempts.
//
// Whether a principal may send at all is not decided here: a row can only
// be queued through a store.PolicyTx whose decision (sendpolicy.Decide,
// §8.9) is Queue, and that decision fixes the identity, the submitter and
// the approver. internal/submit is the caller.
package outbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/yuuka-mail/yuuka/internal/localraw"
	"github.com/yuuka-mail/yuuka/internal/r2"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/transport"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Transport is what the executor needs from an exit; transport.SMTP is
// one. Defined here, by its consumer (§10.2).
type Transport interface {
	Caps() transport.Caps
	Send(ctx context.Context, env transport.Envelope, raw []byte, commit func(context.Context) error) (*transport.SendResult, error)
}

// RawStore is the raw bucket: sent copies are created there, and read
// back when the local copy is missing. r2.RawBucket is one.
type RawStore interface {
	Get(ctx context.Context, key string) (r2.Object, error)
	PutSent(ctx context.Context, key string, body []byte) error
}

// Config holds a Service's dependencies and tuning. Zero tuning fields take
// the defaults below.
type Config struct {
	DB    *store.DB
	Raw   RawStore
	Local localraw.Dir
	// Transports by the name identities.transport uses. A row whose
	// identity names another one is parked, not attempted.
	Transports map[string]Transport
	// IndexSent parses a sent copy into messages (§4.6), so it shows up in
	// threads and search. Enqueue calls it after the commit; a failure is
	// logged and does not undo the enqueue, so the indexer must also pick
	// up sent copies it missed (its backlog pass). Nil skips it.
	IndexSent func(ctx context.Context, rawID string) error

	// Workers is how many attempts run at once (default 2).
	Workers int
	// PollInterval is how often an idle worker looks for due rows; Enqueue
	// in the same process wakes one at once (default 30s).
	PollInterval time.Duration
	// Lease is an attempt's lease, renewed every third of it while the
	// attempt runs (default 2m).
	Lease time.Duration
	// SendTimeout bounds one attempt (default transport.DefaultTimeout).
	SendTimeout time.Duration
	// RecoverInterval is how often expired leases are recovered and rows
	// of unconfigured transports parked (default 1m).
	RecoverInterval time.Duration
	// Retry decides retries of deferred recipients (default DefaultRetry).
	Retry store.RetryPolicy
	// PauseMin and PauseMax bound how long a transport is paused after an
	// exit failure; the pause doubles while the exit keeps failing
	// (defaults 1m and 30m).
	PauseMin, PauseMax time.Duration
	// Owner names this executor in send_attempts.owner (default
	// host:pid:random).
	Owner string
	Log   *slog.Logger
	// Now is the clock for pauses; nil means time.Now.
	Now func() time.Time
}

// DefaultRetry gives a message 10 failed or partial attempts, waiting
// Backoff in between: about a day and a half from the first to the last.
// The exit is a submission server that retries delivery itself, so a
// deferral here is its own trouble (rate limits, a full queue), not a
// remote mailbox's; a day and a half outlasts an outage of an evening,
// and a person sees the dead row the next day rather than a week later.
var DefaultRetry = store.RetryPolicy{MaxFailures: 10, Delay: Backoff}

// Backoff is 5m·3^(n-1) after the n-th failure, capped at 6h: 5m, 15m,
// 45m, 2h15m, then every 6h. The first retry is soon enough to pass
// greylisting and a short rate limit.
func Backoff(n int) time.Duration {
	d := 5 * time.Minute
	for i := 1; i < n && d < 6*time.Hour; i++ {
		d *= 3
	}
	return min(d, 6*time.Hour)
}

// Retry delays the executor uses for abandoned attempts.
const (
	// recoverRetry: after an executor was lost before the commit point.
	recoverRetry = time.Minute
	// commitRetry: after the commit point could not be recorded, e.g.
	// because the database was unreachable.
	commitRetry = time.Minute
	// storageRetry: after the sent copy could not be read or did not
	// match its recorded hash.
	storageRetry = 10 * time.Minute
	// unconfiguredRetry: how long a row whose transport is missing waits
	// before it is checked again.
	unconfiguredRetry = 10 * time.Minute
)

// Service enqueues and sends. Enqueue may be called concurrently, also
// while Run is running and without it.
type Service struct {
	cfg   Config
	log   *slog.Logger
	names []string
	wake  chan struct{}

	mu     sync.Mutex
	paused map[string]pause
}

type pause struct {
	until time.Time
	next  time.Duration
}

// New returns a Service.
func New(cfg Config) *Service {
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 30 * time.Second
	}
	if cfg.Lease <= 0 {
		cfg.Lease = 2 * time.Minute
	}
	if cfg.SendTimeout <= 0 {
		cfg.SendTimeout = transport.DefaultTimeout
	}
	if cfg.RecoverInterval <= 0 {
		cfg.RecoverInterval = time.Minute
	}
	if cfg.Retry.Delay == nil {
		cfg.Retry = DefaultRetry
	}
	if cfg.PauseMin <= 0 {
		cfg.PauseMin = time.Minute
	}
	if cfg.PauseMax < cfg.PauseMin {
		cfg.PauseMax = max(30*time.Minute, cfg.PauseMin)
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Owner == "" {
		host, _ := os.Hostname()
		cfg.Owner = fmt.Sprintf("%s:%d:%s", host, os.Getpid(), ulid.New(time.Now())[10:])
	}
	names := make([]string, 0, len(cfg.Transports))
	for n := range cfg.Transports {
		names = append(names, n)
	}
	return &Service{
		cfg:    cfg,
		log:    cfg.Log.With("component", "outbox"),
		names:  names,
		wake:   make(chan struct{}, 1),
		paused: map[string]pause{},
	}
}

// Submission is a message to queue. Who sends it, as what and approved
// by whom is not in it: that is the decision of the store.PolicyTx it is
// queued through.
type Submission struct {
	// DraftID is the draft it came from, if any.
	DraftID string
	// RawID, when set, is the existing sent copy of the held SMTP
	// submission DraftID, queued as it was stored; Raw must be its bytes.
	// Otherwise Raw is stored as a new sent copy.
	RawID string
	// Raw is the composed message. Header fields the transport does not
	// let the caller set are removed before it is stored
	// (transport.StripHeaders), so the sent copy is what the exit gets.
	Raw []byte
	// Recipients are the envelope recipients, verbatim. Spellings that
	// differ only in case are one recipient, the first spelling wins
	// (§4.6). They must be the ones the decision was made for.
	Recipients []store.OutboxRecipient
}

// Enqueued names what Enqueue created.
type Enqueued struct {
	OutboxID string
	RawID    string
}

// ErrUnconfigured is returned when an identity names a transport this
// process does not have.
var ErrUnconfigured = errors.New("transport not configured")

// Caps returns the caps of the transport called name, ErrUnconfigured if
// this process has none by that name.
func (s *Service) Caps(name string) (transport.Caps, error) {
	tr, ok := s.cfg.Transports[name]
	if !ok {
		return transport.Caps{}, fmt.Errorf("%w: %q", ErrUnconfigured, name)
	}
	return tr.Caps(), nil
}

// Enqueue stores sub's bytes as a sent copy and queues it for sending
// (§4.6) through p, the submission's decision, which must be Queue
// (store.ErrNotApproved otherwise). The bytes are written once, under
// sent/ in R2 and in the local copy, before p records the raw object,
// the outbox row (queued, due now), its recipients and a change. If
// anything fails nothing is queued; bytes already written stay as an
// orphan, which is not lost mail since nothing was sent. Once p's
// transaction commits, an idle worker wakes and the sent copy is indexed.
func (s *Service) Enqueue(ctx context.Context, p *store.PolicyTx, sub Submission) (Enqueued, error) {
	if err := checkSubmission(sub); err != nil {
		return Enqueued{}, fmt.Errorf("enqueue: %w", err)
	}
	// p.Enqueue below is the check; this one only saves storing bytes
	// for a submission that cannot be queued.
	if d := p.Decision(); !d.Queue {
		return Enqueued{}, fmt.Errorf("enqueue: %w: %s", store.ErrNotApproved, d.HoldReason)
	}
	name := p.Facts().Identity.Transport
	caps, err := s.Caps(name)
	if err != nil {
		// Its caps decide which header fields to strip, so the bytes
		// cannot be prepared without it.
		return Enqueued{}, fmt.Errorf("enqueue: identity %s: %w", p.Facts().Identity.ID, err)
	}
	raw := transport.StripHeaders(sub.Raw, caps)

	var sr store.SentRaw
	if sub.RawID != "" {
		sr = store.SentRaw{ID: sub.RawID}
	} else if sr, err = s.StoreSent(ctx, raw); err != nil {
		return Enqueued{}, fmt.Errorf("enqueue: %w", err)
	}
	e := Enqueued{OutboxID: ulid.New(s.cfg.Now()), RawID: sr.ID}
	err = p.Enqueue(ctx, store.NewOutbox{
		ID: e.OutboxID, DraftID: sub.DraftID, Raw: sr, Message: raw, Recipients: sub.Recipients,
	})
	if err != nil {
		return Enqueued{}, err
	}
	p.AfterCommit(func(ctx context.Context) {
		s.Wake()
		s.log.Info("queued", "outbox", e.OutboxID, "raw", e.RawID, "transport", name)
		if s.cfg.IndexSent != nil {
			if err := s.cfg.IndexSent(ctx, e.RawID); err != nil {
				s.log.Warn("index sent copy; the backlog pass will retry", "raw", e.RawID, "err", err)
			}
		}
	})
	return e, nil
}

// StoreSent writes raw as a new sent copy, write-once under sent/ (the
// local copy, then R2), and returns its description for the row that
// will point at it. Storing bytes sends nothing: sent/ holds every byte
// string composed or normalized for sending, sent or not (§4.6).
func (s *Service) StoreSent(ctx context.Context, raw []byte) (store.SentRaw, error) {
	now := s.cfg.Now()
	id := ulid.New(now)
	key := r2.SentKey(now, id)
	// The local copy first: it is cheap and fails on a full disk before
	// anything leaves the host.
	if err := s.cfg.Local.Put(key, raw); err != nil {
		return store.SentRaw{}, err
	}
	if err := s.putSent(ctx, key, raw); err != nil {
		return store.SentRaw{}, err
	}
	return store.NewSentRaw(id, key, raw, now), nil
}

// ReadSent returns the bytes of sent copy r, preferring the local copy,
// checked against the size and sha256 recorded when they were stored.
func (s *Service) ReadSent(ctx context.Context, r store.SentRaw) ([]byte, error) {
	b, err := s.cfg.Local.Get(r.Key)
	if errors.Is(err, fs.ErrNotExist) {
		obj, gerr := s.cfg.Raw.Get(ctx, r.Key)
		b, err = obj.Body, gerr
	}
	if err != nil {
		return nil, err
	}
	if sum := sha256.Sum256(b); int64(len(b)) != r.Size || sum != r.SHA256 {
		return nil, fmt.Errorf("%s does not match its recorded size and sha256", r.Key)
	}
	return b, nil
}

// putSent writes a sent copy create-only. A write that finds the key
// taken is a retry whose first try landed, or a bug: the stored bytes
// decide (§2.2).
func (s *Service) putSent(ctx context.Context, key string, raw []byte) error {
	err := s.cfg.Raw.PutSent(ctx, key, raw)
	if !errors.Is(err, r2.ErrExists) {
		return err
	}
	obj, gerr := s.cfg.Raw.Get(ctx, key)
	if gerr != nil {
		return fmt.Errorf("%w; reading it back: %w", err, gerr)
	}
	if a, b := sha256.Sum256(obj.Body), sha256.Sum256(raw); len(obj.Body) != len(raw) || !bytes.Equal(a[:], b[:]) {
		return fmt.Errorf("sent copy %s already exists with different content", key)
	}
	return nil
}

func checkSubmission(sub Submission) error {
	switch {
	case len(sub.Raw) == 0:
		return errors.New("empty message")
	case len(sub.Recipients) == 0:
		return errors.New("no recipients")
	}
	for _, r := range sub.Recipients {
		if err := checkAddr(r.Addr); err != nil {
			return fmt.Errorf("recipient: %w", err)
		}
		switch r.Kind {
		case store.RecipientTo, store.RecipientCc, store.RecipientBcc:
		default:
			return fmt.Errorf("recipient %q: kind %q", r.Addr, r.Kind)
		}
	}
	return nil
}

// checkAddr accepts a bare address that can go into MAIL or RCPT as is.
// The server judges the rest, per recipient.
func checkAddr(a string) error {
	at := strings.LastIndexByte(a, '@')
	if at <= 0 || at == len(a)-1 {
		return fmt.Errorf("%q is not local@domain", a)
	}
	if strings.ContainsFunc(a, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || r == '<' || r == '>' }) {
		return fmt.Errorf("%q contains spaces, control characters or angle brackets", a)
	}
	return nil
}

// Wake makes an idle worker look for due rows now.
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
