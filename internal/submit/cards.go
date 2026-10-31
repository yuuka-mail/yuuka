package submit

import (
	"context"
	"fmt"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/compose"
	"github.com/yuuka-mail/yuuka/internal/notify"
	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// preview is what the review reads of draft d: for a held SMTP
// submission its stored bytes, otherwise d composed as it will be,
// minus what the decision adds (Auto-Submitted, Message-ID, threading
// headers), none of which the checks read.
func (s *Service) preview(ctx context.Context, d store.Draft, now time.Time) (*outgoing, error) {
	if d.RawID != "" {
		sr, err := s.cfg.DB.SentRawByID(ctx, d.RawID)
		if err != nil {
			return nil, err
		}
		raw, err := s.cfg.Outbox.ReadSent(ctx, sr)
		if err != nil {
			return nil, err
		}
		return readOutgoing(raw, d.Cites), nil
	}
	ident, err := s.cfg.DB.IdentityByID(ctx, d.IdentityID)
	if err != nil {
		return nil, err
	}
	raw, err := compose.Compose(&compose.Message{
		From: compose.Address{Name: ident.DisplayName, Addr: d.FromAddr},
		To:   addresses(d.To), Cc: addresses(d.Cc), Bcc: addresses(d.Bcc),
		Subject: d.Subject, Date: now, Body: d.BodyMD,
		Signature: ident.Signature, StripMarkup: ident.PlainStripMarkup,
	})
	if err != nil {
		return nil, fmt.Errorf("compose: %w", err)
	}
	return readOutgoing(raw, d.Cites), nil
}

// cardTimeout bounds sending one card.
const cardTimeout = 30 * time.Second

// afterHeld sends the held card of d (held as res) once p commits.
func (s *Service) afterHeld(p *store.PolicyTx, v *auth.Viewer, d store.Draft, o *outgoing, res Result) {
	if s.cfg.Notifier == nil || o == nil {
		return
	}
	now := s.cfg.Now()
	c := notify.HeldCard{
		OutboxID: res.OutboxID, From: d.FromAddr, To: d.To, Cc: d.Cc, Bcc: d.Bcc, Submitter: v.String(),
		Subject: o.subject, Attachments: o.attachments, Excerpt: o.excerpt,
		SendAt: now.Add(time.Duration(res.HoldSeconds) * time.Second), Now: now,
	}
	if r := p.Decision().Review; r != nil {
		c.Passed = r.Passed()
	}
	p.AfterCommit(func(ctx context.Context) {
		s.card(ctx, "held", res.OutboxID, func(ctx context.Context) error {
			if d.InReplyToID != "" {
				if m, err := s.cfg.DB.MessageByID(ctx, v, d.InReplyToID); err == nil {
					c.ThreadSubject = m.Subject
				}
			}
			return s.cfg.Notifier.Held(ctx, c)
		})
	})
}

// afterEscalated sends the card of d, escalated by the review, once p
// commits; nothing for any other hold.
func (s *Service) afterEscalated(p *store.PolicyTx, v *auth.Viewer, d store.Draft, dec sendpolicy.Decision) {
	if s.cfg.Notifier == nil || dec.Review == nil || dec.HoldReason != sendpolicy.HoldReview {
		return
	}
	c := notify.EscalatedCard{DraftID: d.ID, From: d.FromAddr, To: d.To, Cc: d.Cc, Bcc: d.Bcc, Submitter: v.String(), Subject: d.Subject}
	for _, ch := range dec.Review.Checks {
		if !ch.Pass {
			c.Reasons = append(c.Reasons, ch.Detail)
		}
	}
	p.AfterCommit(func(ctx context.Context) {
		s.card(ctx, "escalated", d.ID, func(ctx context.Context) error { return s.cfg.Notifier.Escalated(ctx, c) })
	})
}

// card sends one card in the background. A card that fails is logged
// and nothing else: the window it would announce runs anyway (§8.9).
func (s *Service) card(ctx context.Context, kind, id string, send func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cardTimeout)
	s.cards.Go(func() {
		defer cancel()
		if err := send(ctx); err != nil {
			s.log.Warn("review card not sent", "card", kind, "id", id, "err", err)
		}
	})
}

// Wait returns once the cards in flight are sent or have failed.
func (s *Service) Wait() { s.cards.Wait() }
