package notify

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Poller receives the presses of the recall button on held cards, and
// /start <code> for a binding, by long polling (getUpdates), so yuuka
// exposes no endpoint for Telegram to call and keeps no webhook secret
// (§8.4, §13.7). Updates come only from api.telegram.org over TLS with
// the bot's token; Poller then believes a press only when Authorize
// says it counts and it is on a button it made (data w:<ULID>). The only
// action there is is a recall, which is idempotent and checks the row's
// state, so a replayed or stale press does no harm: the worst a stolen
// Telegram account can do is hold mail back.
type Poller struct {
	TG *Telegram
	// Authorize says whether a press by user from in chat counts and, if
	// it does, how a recall acts for it (as whom, recorded how); a nil
	// Recall means it does not count. It runs on every press, so a
	// binding revoked a second ago no longer counts.
	Authorize func(ctx context.Context, chat, from int64) (Recall, error)
	// Start answers a message to the bot (/start <code>) with a reply,
	// "" for none. nil: the poller does not ask for messages.
	Start func(ctx context.Context, m *Message) string
	// Suspended sends the card of a grant a recall suspended; nil sends
	// none.
	Suspended func(ctx context.Context, c SuspendedCard) error
	Log       *slog.Logger
	// Timeout is the long-poll wait in seconds (default 50). The
	// client's own timeout must be longer.
	Timeout int
}

// Recall withdraws held row outboxID. answer is what the person is
// told; done says the button has served (recalled, or the row is past
// recalling), so it is taken off the card. An error is a failure worth
// retrying: the button stays.
type Recall func(ctx context.Context, outboxID string) (answer string, done bool, suspended *SuspendedCard, err error)

// Operator authorizes the presses of one user in one chat, as the
// environment configures it (YUUKA_TELEGRAM_OPERATOR_ID and the chat id):
// recall is how they act.
func Operator(user, chat int64, recall Recall) func(context.Context, int64, int64) (Recall, error) {
	return func(_ context.Context, c, from int64) (Recall, error) {
		if from != user || c != chat {
			return nil, nil
		}
		return recall, nil
	}
}

// Recaller withdraws held mail as v for a press of the recall button,
// recording args (via: telegram, who pressed). It goes through the same
// store function as the WebUI. The outcomes a person can act on are
// answers, not errors: a stale or replayed press is told what became of
// the message.
func Recaller(db *store.DB, v *auth.Viewer, args map[string]any) Recall {
	return func(ctx context.Context, id string) (string, bool, *SuspendedCard, error) {
		w, err := db.WithdrawOutbox(ctx, v, id, args, time.Now())
		switch {
		case errors.Is(err, store.ErrNotFound):
			return "No such message.", true, nil, nil
		case errors.Is(err, store.ErrOutboxState):
			e, err := db.OutboxByID(ctx, v, id)
			if err != nil {
				return "", false, nil, err
			}
			if e.State == store.OutboxWithdrawn {
				return "Already recalled.", true, nil, nil
			}
			return "Too late: it was already released for sending.", true, nil, nil
		case err != nil:
			return "", false, nil, err
		}
		if w.Suspended == nil {
			return "Recalled. It is a draft again.", true, nil, nil
		}
		card := SuspendedCardOf(*w.Suspended)
		return "Recalled. The grant is now suspended.", true, &card, nil
	}
}

// SuspendedCardOf is the card of grant s, named by address and principal.
func SuspendedCardOf(s store.Suspension) SuspendedCard {
	return SuspendedCard{
		IdentityID: s.IdentityID, Identity: s.Identity, Principal: s.Principal.Kind + ":" + s.Principal.Name,
		Recalls: s.Recalls, Withdrawn: len(s.Withdrawn),
	}
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
	allowed := []string{"callback_query"}
	if p.Start != nil {
		allowed = append(allowed, "message")
	}
	var offset int64
	backoff := time.Second
	for ctx.Err() == nil {
		us, err := p.TG.Updates(ctx, offset, timeout, allowed)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, ErrConflict) {
				log.Error("telegram: getUpdates refused; buttons and /start do not work while the bot has a webhook "+
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
			switch {
			case u.Callback != nil:
				p.press(ctx, log, u.Callback)
			case u.Message != nil && p.Start != nil:
				p.message(ctx, log, u.Message)
			}
		}
	}
}

// press acts on one press.
func (p *Poller) press(ctx context.Context, log *slog.Logger, c *CallbackQuery) {
	var recall Recall
	if c.Message != nil {
		var err error
		if recall, err = p.Authorize(ctx, c.Message.Chat.ID, c.From.ID); err != nil {
			log.Error("telegram: authorize a press", "err", err)
			_ = p.TG.Answer(ctx, c.ID, "Could not check that; try again, or use the WebUI.")
			return
		}
	}
	if recall == nil {
		// Not answered: someone else's press gets nothing back.
		log.Warn("telegram: ignored a button press that no binding accounts for", "from", c.From.ID)
		return
	}
	id, ok := strings.CutPrefix(c.Data, "w:")
	if !ok || !ulid.Valid(id) {
		log.Warn("telegram: ignored a button press with unknown data")
		_ = p.TG.Answer(ctx, c.ID, "Unknown action.")
		return
	}
	answer, done, suspended, err := recall(ctx, id)
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
	if suspended != nil && p.Suspended != nil {
		if err := p.Suspended(ctx, *suspended); err != nil {
			log.Warn("telegram: suspended card", "err", err)
		}
	}
}

// message answers one message, if Start has something to say.
func (p *Poller) message(ctx context.Context, log *slog.Logger, m *Message) {
	reply := p.Start(ctx, m)
	if reply == "" {
		return
	}
	if err := p.TG.Reply(ctx, m.Chat.ID, reply); err != nil {
		log.Warn("telegram: reply", "err", err)
	}
}

// StartCode is the code of a /start command ("/start <code>", or
// "/start@<bot> <code>" in a group), "" if text is not one.
func StartCode(text string) string {
	f := strings.Fields(text)
	if len(f) != 2 {
		return ""
	}
	cmd, _, _ := strings.Cut(f[0], "@")
	if cmd != "/start" {
		return ""
	}
	return f[1]
}
