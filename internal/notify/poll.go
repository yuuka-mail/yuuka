package notify

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Poller receives the presses of the recall button on held cards by long
// polling (getUpdates), so yuuka exposes no endpoint for Telegram to call
// and keeps no webhook secret (§8.4, §13.7). Updates come only from
// api.telegram.org over TLS with the bot's token; Poller then believes
// a press only when it is the configured operator's (from.id), in the
// configured chat (chat.id), on a button it made (data w:<ULID>). The
// only action there is is a recall, which is idempotent and checks the
// row's state, so a replayed or stale press does no harm: the worst a
// stolen Telegram account can do is hold mail back.
type Poller struct {
	TG *Telegram
	// OperatorID is the Telegram user id whose presses count; ChatID the
	// chat the cards go to.
	OperatorID int64
	ChatID     int64
	// Recall withdraws held row outboxID for the operator. answer is what
	// the person is told; done says the button has served (recalled, or
	// the row is past recalling), so it is taken off the card. An error
	// is a failure worth retrying: the button stays.
	Recall func(ctx context.Context, outboxID string, from int64) (answer string, done bool, suspended *SuspendedCard, err error)
	// Cards sends the suspended card when a recall trips the breaker.
	Cards *Cards
	Log   *slog.Logger
	// Timeout is the long-poll wait in seconds (default 50). The
	// client's own timeout must be longer.
	Timeout int
}

// Run polls until ctx ends.
func (p *Poller) Run(ctx context.Context) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 50
	}
	log := p.Log
	if log == nil {
		log = slog.Default()
	}
	var offset int64
	backoff := time.Second
	for ctx.Err() == nil {
		us, err := p.TG.Updates(ctx, offset, timeout)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, ErrConflict) {
				log.Error("telegram: getUpdates refused; recall buttons do not work while the bot has a webhook "+
					"(deleteWebhook) or another process polls it", "err", err)
				backoff = time.Minute
			} else {
				log.Warn("telegram: getUpdates", "err", err)
			}
			t := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
			case <-t.C:
			}
			t.Stop()
			backoff = min(2*backoff, time.Minute)
			continue
		}
		backoff = time.Second
		for _, u := range us {
			offset = max(offset, u.ID+1)
			if u.Callback != nil {
				p.handle(ctx, log, u.Callback)
			}
		}
	}
}

// handle acts on one press.
func (p *Poller) handle(ctx context.Context, log *slog.Logger, c *CallbackQuery) {
	if c.From.ID != p.OperatorID || c.Message == nil || c.Message.Chat.ID != p.ChatID {
		// Not answered: someone else's press gets nothing back.
		log.Warn("telegram: ignored a button press from outside the operator's chat", "from", c.From.ID)
		return
	}
	id, ok := strings.CutPrefix(c.Data, "w:")
	if !ok || !ulid.Valid(id) {
		log.Warn("telegram: ignored a button press with unknown data")
		_ = p.TG.Answer(ctx, c.ID, "Unknown action.")
		return
	}
	answer, done, suspended, err := p.Recall(ctx, id, c.From.ID)
	if err != nil {
		log.Error("telegram: recall", "outbox", id, "err", err)
		_ = p.TG.Answer(ctx, c.ID, "Could not recall it; try again, or use the WebUI.")
		return
	}
	if err := p.TG.Answer(ctx, c.ID, answer); err != nil {
		log.Warn("telegram: answer", "err", err)
	}
	if done {
		if err := p.TG.RemoveKeyboard(ctx, c.Message.Chat.ID, c.Message.ID); err != nil {
			log.Warn("telegram: remove keyboard", "err", err)
		}
	}
	if suspended != nil && p.Cards != nil {
		if err := p.Cards.Suspended(ctx, *suspended); err != nil {
			log.Warn("telegram: suspended card", "err", err)
		}
	}
}
