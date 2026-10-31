// Package submit is where outgoing mail enters (§8.9, §13.6). The SMTP
// submission port hands it raw MIME (SubmitRaw); REST, MCP and the WebUI
// hand it a draft (SubmitDraft). Both decide inside store.WithDecision
// and record the outcome through its PolicyTx: an outbox row when the
// decision is Queue, a draft with the hold reason otherwise. Neither
// this package nor anything else can queue mail another way: the store
// creates outbox rows only through a PolicyTx, and a PolicyTx carries a
// decision the store made itself (see store/policytx.go).
//
// Exceeding a grant is a draft, never an error. The only refusals are
// the SMTP port's (ErrRejected, ErrNoIdentity): a message that breaks
// the header rules of §13.6, or whose sender has no grant at all on its
// From address, has no draft to stop at.
//
// Nothing here logs or audits a body or a subject: they are mail
// content, and the audit log is not a copy of the mail.
package submit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"sync"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/compose"
	"github.com/yuuka-mail/yuuka/internal/notify"
	"github.com/yuuka-mail/yuuka/internal/outbox"
	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

var (
	// ErrRejected: the submission breaks a rule of §13.6, or the
	// principal has no grant on the From address (tier none). The SMTP
	// port answers 550 with the wrapped reason.
	ErrRejected = errors.New("submission rejected")
	// ErrNoIdentity: no identity covers the header From (or Sender). The
	// SMTP port answers 550.
	ErrNoIdentity = errors.New("no identity covers this address")
)

// errDraftChanged: the draft changed between reading it and locking it.
var errDraftChanged = errors.New("the draft changed while it was submitted")

// Audit log tools.
const (
	toolRaw   = "submission.raw"
	toolDraft = "submission.draft"
)

// Result is where a submission went.
type Result struct {
	// Queued: an outbox row was created in queued, to be sent.
	Queued bool
	// OutboxID is the outbox row: queued, or held when Held.
	OutboxID string
	// Held: the review passed and the row waits HoldSeconds in held for
	// a recall (§8.9). Queued is false; HoldReason is review and Detail
	// says when it goes out, so a caller that knows only Queued and
	// HoldReason still tells it apart from mail that went out.
	Held        bool
	HoldSeconds int
	// DraftID is the draft it stopped at when not Queued, or for held
	// mail the draft it goes back to if withdrawn.
	DraftID string
	// HoldReason is sendpolicy's hold reason, when not Queued.
	HoldReason string
	Detail     string
}

// heldResult is the Result of mail held for review as outbox row id.
func heldResult(id, draftID string, d sendpolicy.Decision) Result {
	return Result{
		OutboxID: id, Held: true, HoldSeconds: d.HoldSeconds, DraftID: draftID,
		HoldReason: string(sendpolicy.HoldReview),
		Detail:     fmt.Sprintf("passed the review; held %d seconds for a recall, then sent", d.HoldSeconds),
	}
}

// Config holds a Service's dependencies.
type Config struct {
	DB *store.DB
	// Outbox stores sent copies and queues them; its transports' caps
	// decide which header fields a message may carry.
	Outbox *outbox.Service
	Log    *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Notifier tells a person about mail the review tier held or
	// escalated (§8.4); nil sends nothing. Its cards go out after the
	// commit, in the background: a slow or failed card never delays the
	// submission, and never extends a recall window.
	Notifier Notifier
}

// Notifier sends the review tier's cards; notify.Cards is one.
type Notifier interface {
	Held(ctx context.Context, c notify.HeldCard) error
	Escalated(ctx context.Context, c notify.EscalatedCard) error
}

// Service submits mail.
type Service struct {
	cfg Config
	log *slog.Logger
	// cards are the review cards being sent (Wait).
	cards sync.WaitGroup
}

// New returns a Service.
func New(cfg Config) *Service {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{cfg: cfg, log: cfg.Log.With("component", "submit")}
}

// SubmitRaw submits a message received on the SMTP submission port
// (§13.6) by v, with envelope mailFrom and rcpts.
//
// The header rules come first and refuse with ErrRejected or
// ErrNoIdentity: exactly one From address, covered by an identity
// (ResolveIdentity); MAIL FROM equal to it; a Sender, if any, covered by
// an identity v has a grant on. Then the decision, under the grant's
// lock: tier none refuses (ErrRejected), Queue stores the normalized
// bytes as a sent copy and queues them, anything else stores them under
// sent/ too and stops at a new draft with the hold reason. The
// recipients are the envelope's, never the header's.
//
// Normalizing (raw.go) makes line endings CRLF, rewrites From (and
// Sender) with the identity's display name, removes Bcc, adds Date and,
// where the exit delivers ours, Message-ID when missing, adds
// Auto-Submitted when the decision asks for it, and removes the fields
// the exit does not let the caller set (transport.StripHeaders).
func (s *Service) SubmitRaw(ctx context.Context, v *auth.Viewer, mailFrom string, rcpts []string, raw []byte) (Result, error) {
	if v == nil {
		return Result{}, errors.New("submit: no viewer")
	}
	now := s.cfg.Now()
	args := map[string]any{"mail_from": mailFrom, "recipients": rcpts, "size": len(raw)}
	reject := func(err error) (Result, error) {
		a := store.Action{
			ID: ulid.New(now), Actor: v.PrincipalID, CredentialID: v.CredentialID, Tool: toolRaw,
			Args: args, Result: map[string]any{"rejected": err.Error()}, At: now,
		}
		if aerr := s.cfg.DB.RecordAction(ctx, a); aerr != nil {
			s.log.Error("record rejected submission", "principal", v.String(), "err", aerr)
		}
		s.log.Info("submission rejected", "principal", v.String(), "err", err)
		return Result{}, err
	}

	m, err := readRaw(raw)
	if err != nil {
		return reject(err)
	}
	args["from"] = m.from.Address
	if err := m.checkEnvelope(mailFrom, rcpts); err != nil {
		return reject(err)
	}
	ident, fromAddr, err := s.cfg.DB.ResolveIdentity(ctx, m.from.Address)
	if errors.Is(err, store.ErrNoIdentity) {
		return reject(fmt.Errorf("%w: From %s", ErrNoIdentity, m.from.Address))
	}
	if err != nil {
		return Result{}, fmt.Errorf("submit: %w", err)
	}
	sender, err := s.checkSender(ctx, v, m, ident, rcpts, now)
	if errors.Is(err, ErrRejected) || errors.Is(err, ErrNoIdentity) {
		return reject(err)
	}
	if err != nil {
		return Result{}, fmt.Errorf("submit: %w", err)
	}
	replyTo, err := s.repliedTo(ctx, v, m.inReplyTo)
	if err != nil {
		return Result{}, fmt.Errorf("submit: %w", err)
	}

	// The review reads the parts and the subject, which normalizing
	// does not change: it rewrites only header fields the checks do not
	// read (From, Sender, Bcc, Date, Message-ID, Auto-Submitted).
	content := readOutgoing(m.data, nil)
	sub := store.Submission{FromAddr: fromAddr, Recipients: rcpts, InReplyToID: replyTo, Now: now, Content: &content.review}
	var res Result
	var rejected error
	err = s.cfg.DB.WithDecision(ctx, v, sub, func(ctx context.Context, p *store.PolicyTx) error {
		f, d := p.Facts(), p.Decision()
		args["identity_id"] = f.Identity.ID
		if d.Tier == sendpolicy.None {
			rejected = fmt.Errorf("%w: %s has no grant to send as %s", ErrRejected, v, f.FromAddr)
			return p.Audit(ctx, toolRaw, args, map[string]any{"rejected": rejected.Error()}, now)
		}
		caps, err := s.cfg.Outbox.Caps(f.Identity.Transport)
		if err != nil {
			return err
		}
		n := normalization{
			From: mail.Address{Name: f.Identity.DisplayName, Address: f.FromAddr}, Sender: sender,
			AutoSubmitted: d.AutoSubmitted, Date: now, Caps: caps,
		}
		if caps.CallerMessageID {
			n.MessageID = compose.NewMessageID(now, domainOf(f.FromAddr))
		}
		out := m.normalize(n)
		recipients := m.recipients(rcpts)
		dr := store.Draft{FromAddr: f.FromAddr, InReplyToID: replyTo, Subject: m.subject}
		for _, r := range recipients {
			f := store.FoldAddr(r.Addr)
			switch r.Kind {
			case store.RecipientTo:
				dr.To = append(dr.To, store.Recipient{Name: m.to[f], Addr: r.Addr})
			case store.RecipientCc:
				dr.Cc = append(dr.Cc, store.Recipient{Name: m.cc[f], Addr: r.Addr})
			default:
				dr.Bcc = append(dr.Bcc, store.Recipient{Addr: r.Addr})
			}
		}
		result := map[string]any{}
		switch {
		case d.Held:
			// A held row needs a draft to go back to: the submission's,
			// backed by the same sent copy, made with the row.
			q, err := s.cfg.Outbox.Enqueue(ctx, p, outbox.Submission{Raw: out, Recipients: recipients, HeldDraft: &dr})
			if err != nil {
				return err
			}
			res, result["outbox_id"] = heldResult(q.OutboxID, q.DraftID, d), q.OutboxID
			s.afterHeld(p, v, dr, content, res)
		case d.Queue:
			q, err := s.cfg.Outbox.Enqueue(ctx, p, outbox.Submission{Raw: out, Recipients: recipients})
			if err != nil {
				return err
			}
			res, result["outbox_id"] = Result{Queued: true, OutboxID: q.OutboxID}, q.OutboxID
		default:
			sr, err := s.cfg.Outbox.StoreSent(ctx, out)
			if err != nil {
				return err
			}
			if dr, err = p.CreateDraft(ctx, dr, &sr, out, now); err != nil {
				return err
			}
			res = Result{DraftID: dr.ID, HoldReason: string(d.HoldReason), Detail: d.Detail}
			result["draft_id"] = dr.ID
			s.afterEscalated(p, v, dr, d)
		}
		return p.Audit(ctx, toolRaw, args, result, now)
	})
	switch {
	case errors.Is(err, store.ErrNoIdentity):
		// The identity went away between resolving and deciding.
		return reject(fmt.Errorf("%w: From %s", ErrNoIdentity, m.from.Address))
	case err != nil:
		return Result{}, fmt.Errorf("submit: %w", err)
	case rejected != nil:
		s.log.Info("submission rejected", "principal", v.String(), "err", rejected)
		return Result{}, rejected
	}
	s.logResult("raw", v, res)
	return res, nil
}

// checkSender applies the Sender rule of §13.6: the address must be
// covered by an identity, and v must have a grant on it (any tier but
// none); then its display name is the identity's too. nil without a
// Sender field.
func (s *Service) checkSender(ctx context.Context, v *auth.Viewer, m *rawMessage, from store.Identity, rcpts []string, now time.Time) (*mail.Address, error) {
	if m.sender == nil {
		return nil, nil
	}
	ident, addr, err := s.cfg.DB.ResolveIdentity(ctx, m.sender.Address)
	if errors.Is(err, store.ErrNoIdentity) {
		return nil, fmt.Errorf("%w: Sender %s", ErrNoIdentity, m.sender.Address)
	}
	if err != nil {
		return nil, err
	}
	if ident.ID != from.ID {
		f, err := s.cfg.DB.PolicyInput(ctx, v, store.Submission{FromAddr: addr, Recipients: rcpts, Now: now})
		if err != nil {
			return nil, err
		}
		if sendpolicy.Decide(f.Input).Tier == sendpolicy.None {
			return nil, fmt.Errorf("%w: %s has no grant to send as Sender %s", ErrRejected, v, addr)
		}
	}
	return &mail.Address{Name: ident.DisplayName, Address: addr}, nil
}

// repliedTo returns the message a raw submission's In-Reply-To names, as
// v sees it, or "" if none: then it is decided as a new thread, which
// recipient scope thread holds as a draft.
func (s *Service) repliedTo(ctx context.Context, v *auth.Viewer, headerID string) (string, error) {
	if headerID == "" {
		return "", nil
	}
	id, err := s.cfg.DB.MessageByHeaderID(ctx, v, headerID)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	return id, err
}

// SubmitDraft submits draft id as v: the author sending their own
// draft, or a human confirming someone else's held draft (§8.9 step 2),
// whose author is then the submitter and v the confirmer. Under the
// grant's lock the draft is locked too, so it is queued at most once
// however often send is pressed (store.ErrInFlight after the first).
//
// Queue composes the draft (internal/compose: the identity's display
// name and signature, its plain-text option, In-Reply-To and References
// from the replied-to message's headers, a Message-ID only where the
// exit delivers ours, Auto-Submitted when the decision asks for it) and
// queues it; a held SMTP submission is queued with its stored bytes,
// normalized again only if the decision now asks for Auto-Submitted.
// Anything else records the hold reason on the draft.
func (s *Service) SubmitDraft(ctx context.Context, v *auth.Viewer, draftID string) (Result, error) {
	if v == nil {
		return Result{}, errors.New("submit: no viewer")
	}
	// A draft edited between the read and the lock is decided again; an
	// edit loop that keeps winning gets an error, not a decision on
	// content nobody saw.
	for range 3 {
		res, err := s.submitDraft(ctx, v, draftID)
		if !errors.Is(err, errDraftChanged) {
			return res, err
		}
	}
	return Result{}, fmt.Errorf("submit draft %s: %w", draftID, errDraftChanged)
}

func (s *Service) submitDraft(ctx context.Context, v *auth.Viewer, id string) (Result, error) {
	d, err := s.cfg.DB.DraftByID(ctx, v, id)
	if err != nil {
		return Result{}, fmt.Errorf("submit draft: %w", err)
	}
	if d.OutboxID != "" {
		return Result{}, fmt.Errorf("submit draft %s: %w", id, store.ErrInFlight)
	}
	now := s.cfg.Now()
	recipients := store.DraftRecipients(d)
	sub := store.Submission{
		FromAddr: d.FromAddr, IdentityID: d.IdentityID, InReplyToID: d.InReplyToID, Now: now,
	}
	for _, r := range recipients {
		sub.Recipients = append(sub.Recipients, r.Addr)
	}
	args := map[string]any{"draft_id": id, "identity_id": d.IdentityID, "from": d.FromAddr, "recipients": sub.Recipients}
	var content *outgoing
	if d.PrincipalID != v.PrincipalID {
		sub.DraftAuthorID = d.PrincipalID
		args["author"] = d.PrincipalID
	} else {
		// Only the author's own submission can meet the review; a
		// person confirming is the review. The draft is read again
		// under its lock below and must not have changed, so what is
		// reviewed here is what is queued.
		if content, err = s.preview(ctx, d, now); err != nil {
			return Result{}, fmt.Errorf("submit draft %s: %w", id, err)
		}
		sub.Content = &content.review
	}

	var res Result
	err = s.cfg.DB.WithDecision(ctx, v, sub, func(ctx context.Context, p *store.PolicyTx) error {
		cur, err := p.LockDraft(ctx, id)
		if err != nil {
			return err
		}
		if !cur.UpdatedAt.Equal(d.UpdatedAt) {
			return errDraftChanged
		}
		dec := p.Decision()
		result := map[string]any{}
		if !dec.Queue {
			if err := p.HoldDraft(ctx, id, now); err != nil {
				return err
			}
			res = Result{DraftID: id, HoldReason: string(dec.HoldReason), Detail: dec.Detail}
			s.afterEscalated(p, v, d, dec)
		} else {
			var q outbox.Enqueued
			if d.RawID != "" {
				q, err = s.queueHeld(ctx, p, d, recipients, now)
			} else {
				q, err = s.queueComposed(ctx, p, d, recipients, now)
			}
			if err != nil {
				return err
			}
			res, result["outbox_id"] = Result{Queued: true, OutboxID: q.OutboxID}, q.OutboxID
			if dec.Held {
				res = heldResult(q.OutboxID, id, dec)
				s.afterHeld(p, v, d, content, res)
			}
		}
		return p.Audit(ctx, toolDraft, args, result, now)
	})
	if err != nil {
		if errors.Is(err, errDraftChanged) {
			return Result{}, err
		}
		return Result{}, fmt.Errorf("submit draft %s: %w", id, err)
	}
	s.logResult("draft", v, res)
	return res, nil
}

// queueComposed composes Markdown draft d and queues it through p.
func (s *Service) queueComposed(ctx context.Context, p *store.PolicyTx, d store.Draft, rs []store.OutboxRecipient, now time.Time) (outbox.Enqueued, error) {
	f, dec := p.Facts(), p.Decision()
	caps, err := s.cfg.Outbox.Caps(f.Identity.Transport)
	if err != nil {
		return outbox.Enqueued{}, err
	}
	m := &compose.Message{
		From:          compose.Address{Name: f.Identity.DisplayName, Addr: f.FromAddr},
		To:            addresses(d.To),
		Cc:            addresses(d.Cc),
		Bcc:           addresses(d.Bcc),
		Subject:       d.Subject,
		Date:          now,
		AutoSubmitted: dec.AutoSubmitted,
		Body:          d.BodyMD,
		Signature:     f.Identity.Signature,
		StripMarkup:   f.Identity.PlainStripMarkup,
	}
	// A transport that sets its own (CallerMessageID=false) gets none,
	// rather than one it throws away (§10.4.1).
	if caps.CallerMessageID {
		m.MessageID = compose.NewMessageID(now, domainOf(f.FromAddr))
	}
	if r := f.Reply; r != nil {
		m.Parent = &compose.Parent{MessageID: r.HeaderID, InReplyTo: r.InReplyTo, References: r.References}
	}
	raw, err := compose.Compose(m)
	if err != nil {
		return outbox.Enqueued{}, fmt.Errorf("compose: %w", err)
	}
	return s.cfg.Outbox.Enqueue(ctx, p, outbox.Submission{DraftID: d.ID, Raw: raw, Recipients: rs})
}

// queueHeld queues held SMTP submission d through p: its stored bytes,
// or, if normalizing them under this decision changes them (it now asks
// for Auto-Submitted, or the identity's display name changed), the
// normalized bytes as a new sent copy.
func (s *Service) queueHeld(ctx context.Context, p *store.PolicyTx, d store.Draft, rs []store.OutboxRecipient, now time.Time) (outbox.Enqueued, error) {
	f, dec := p.Facts(), p.Decision()
	sr, err := s.cfg.DB.SentRawByID(ctx, d.RawID)
	if err != nil {
		return outbox.Enqueued{}, err
	}
	stored, err := s.cfg.Outbox.ReadSent(ctx, sr)
	if err != nil {
		return outbox.Enqueued{}, err
	}
	m, err := readRaw(stored)
	if err != nil {
		return outbox.Enqueued{}, fmt.Errorf("held submission %s: %w", d.RawID, err)
	}
	caps, err := s.cfg.Outbox.Caps(f.Identity.Transport)
	if err != nil {
		return outbox.Enqueued{}, err
	}
	out := m.normalize(normalization{
		From:          mail.Address{Name: f.Identity.DisplayName, Address: f.FromAddr},
		AutoSubmitted: dec.AutoSubmitted, Date: now, Caps: caps,
	})
	sub := outbox.Submission{DraftID: d.ID, Raw: out, Recipients: rs}
	if string(out) == string(stored) {
		sub.RawID = d.RawID
	}
	return s.cfg.Outbox.Enqueue(ctx, p, sub)
}

// addresses are a draft's recipients as compose writes them, names and
// all; the names go into To and Cc only (§10.2.1).
func addresses(rs []store.Recipient) []compose.Address {
	out := make([]compose.Address, 0, len(rs))
	for _, r := range rs {
		out = append(out, compose.Address{Name: r.Name, Addr: r.Addr})
	}
	return out
}

func (s *Service) logResult(kind string, v *auth.Viewer, r Result) {
	if r.Held {
		s.log.Info("submission held for review", "kind", kind, "principal", v.String(), "outbox", r.OutboxID,
			"hold_seconds", r.HoldSeconds)
		return
	}
	if r.Queued {
		s.log.Info("submission queued", "kind", kind, "principal", v.String(), "outbox", r.OutboxID)
		return
	}
	s.log.Info("submission held", "kind", kind, "principal", v.String(), "draft", r.DraftID, "hold_reason", r.HoldReason)
}
