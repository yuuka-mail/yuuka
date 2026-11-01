package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/notify"
	"github.com/yuuka-mail/yuuka/internal/outbox"
	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// Notifications (§8.4, §17). Two sources, never both at once:
//
//   - the database's channels, bindings and subscriptions (§17), when
//     serve has the key that opens bot tokens (YUUKA_KEK_FILE) and the
//     environment's bot, if any, is one of those channels. The import at
//     start (telegram_import.go) makes it one. A notify.Dispatcher then
//     sends every event and a notify.Manager polls every channel;
//   - otherwise YUUKA_TELEGRAM_*, as before §17: one bot, one chat, and
//     the recall button when the operator is configured. These variables
//     are deprecated and go one release after the import shipped (§17.7).
//
// Commands other than serve keep using the environment's bot when it is
// set: they do not read the key, so a broken KEK file cannot stop
// `yuuka reconcile`.

// pollClientTimeout outlasts the long poll's 50 seconds.
const pollClientTimeout = 75 * time.Second

// telegramAPI is the Bot API's base URL ("" is api.telegram.org); tests
// point it at a fake.
var telegramAPI string

// cardSender sends the review tier's cards: notify.Cards to the
// environment's chat, or notify.Dispatcher to the bindings subscribed.
type cardSender interface {
	Held(ctx context.Context, c notify.HeldCard) error
	Escalated(ctx context.Context, c notify.EscalatedCard) error
	Suspended(ctx context.Context, c notify.SuspendedCard) error
}

// openNotify decides where notifications go and sets a.mail, a.alerter,
// a.cards and a.notifyRun. Every command but serve uses the
// environment's bot, if any; serve calls it after importEnvTelegram, so
// that a bot just imported comes from the database at once. sealer is
// serve's (nil: no key).
func (a *app) openNotify(ctx context.Context, log *slog.Logger, sealer *secret.Sealer, serve bool) error {
	env := a.cfg.TelegramToken != ""
	if serve {
		var err error
		fromDB := sealer != nil
		if fromDB && env {
			if fromDB, err = a.envBotIsChannel(ctx); err != nil {
				return err
			}
		}
		if fromDB || !env {
			d := notify.NewDispatcher(notify.DispatcherConfig{
				DB: a.db, Sealer: sealer, Origin: a.cfg.Web.Origin, Log: log, BaseURL: telegramAPI,
			})
			a.notifyRun = (&notify.Manager{D: d}).Run
			if sealer == nil {
				// No key and no environment bot: the manager only
				// reports the channels that are locked.
				return nil
			}
			if env {
				log.Warn("YUUKA_TELEGRAM_* are ignored: their bot is a channel in the database now. " +
					"Remove them; they are deprecated (see `yuuka channel ls`)")
			}
			a.mail, a.alerter, a.cards, a.channels = d, d, d, d
			return nil
		}
		if sealer == nil {
			log.Warn("YUUKA_TELEGRAM_* are deprecated: set YUUKA_KEK_FILE and serve moves them into the database " +
				"(`yuuka channel`); until then they work as before")
		}
	}
	if !env {
		return nil
	}
	tg := &notify.Telegram{
		Token: a.cfg.TelegramToken, ChatID: a.cfg.TelegramChat, BaseURL: telegramAPI,
		Client: &http.Client{Timeout: 15 * time.Second},
	}
	a.mail, a.alerter = tg, tg
	a.cards = &notify.Cards{TG: tg, Origin: a.cfg.Web.Origin, Recall: a.cfg.TelegramOperatorID != 0}
	if serve {
		p, err := a.recallPoller(ctx)
		if err != nil {
			return err
		}
		if p != nil {
			a.notifyRun = func(ctx context.Context) {
				log.Info("telegram: answering the recall button of held mail", "operator", a.cfg.TelegramOperator)
				p.Log = log
				p.Run(ctx)
			}
		}
	}
	return nil
}

// secretAlerter is where the outbox raises the security alert of a
// transport secret that fails to open as sealed: the dispatcher when
// channels come from the database, nil (logged only) otherwise.
var _ outbox.SecurityAlerter = (*notify.Dispatcher)(nil)

func (a *app) secretAlerter() outbox.SecurityAlerter {
	if sa, ok := a.alerter.(outbox.SecurityAlerter); ok {
		return sa
	}
	return nil
}

// NewMail relays new mail to where openNotify decided. Ingest holds the
// app rather than a notifier, so that serve can decide after its imports.
func (a *app) NewMail(ctx context.Context, m notify.Mail) error {
	if a.mail == nil {
		return nil
	}
	return a.mail.NewMail(ctx, m)
}

// recallPoller returns the environment's poller of the recall button,
// nil when the operator is not configured. The operator must be an
// enabled human principal: a recall is recorded as them, with no
// credential and via telegram (like the CLI, there is no login to name).
func (a *app) recallPoller(ctx context.Context) (*notify.Poller, error) {
	if a.cfg.TelegramOperatorID == 0 {
		return nil, nil
	}
	kind, name, err := parsePrincipal(a.cfg.TelegramOperator)
	if err != nil {
		return nil, err
	}
	op, err := a.db.PrincipalByName(ctx, kind, name)
	if errors.Is(err, store.ErrNotFound) || err == nil && op.DisabledAt != nil {
		return nil, fmt.Errorf("YUUKA_TELEGRAM_OPERATOR: %s is not an enabled principal", a.cfg.TelegramOperator)
	}
	if err != nil {
		return nil, err
	}
	chat, err := strconv.ParseInt(a.cfg.TelegramChat, 10, 64)
	if err != nil {
		return nil, err
	}
	v := &auth.Viewer{PrincipalID: op.ID, PrincipalKind: op.Kind, PrincipalName: op.Name}
	from := a.cfg.TelegramOperatorID
	return &notify.Poller{
		TG: &notify.Telegram{
			Token: a.cfg.TelegramToken, ChatID: a.cfg.TelegramChat, BaseURL: telegramAPI,
			Client: &http.Client{Timeout: pollClientTimeout},
		},
		Authorize: notify.Operator(from, chat,
			notify.Recaller(a.db, v, map[string]any{"via": "telegram", "telegram_user_id": from})),
		Suspended: a.cards.Suspended,
	}, nil
}

// suspendCards sends the card of a grant a recall through REST
// suspended, as the recall button's poller does for its own.
type suspendCards struct {
	cards cardSender
	log   *slog.Logger
}

// suspendCardTimeout bounds sending one card. The recall has committed;
// the client going away does not take the card with it.
const suspendCardTimeout = 30 * time.Second

func (c suspendCards) Suspended(ctx context.Context, s store.Suspension) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), suspendCardTimeout)
	defer cancel()
	if err := c.cards.Suspended(ctx, notify.SuspendedCardOf(s)); err != nil {
		c.log.Warn("suspended card not sent", "identity", s.IdentityID, "principal", s.PrincipalID, "err", err)
	}
}
