package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/yuuka-mail/yuuka/internal/manage"
	"github.com/yuuka-mail/yuuka/internal/notify"
	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// The one-time import of YUUKA_TELEGRAM_* into the database (§17.7). The
// four variables become a channel named "telegram", one binding for the
// operator (YUUKA_TELEGRAM_OPERATOR, else the only human) with the
// recall button when YUUKA_TELEGRAM_OPERATOR_ID is set, subscribed to
// every event the environment's bot got. It is the only binding made
// without the two-sided confirmation of §13.7: the environment is the
// host's own word. Recorded as config.import by system, via env.
//
// Without a key there is nothing to seal the token with: serve then keeps
// using the variables as before. Once imported they are ignored, with a
// warning, and one release later they are gone (§17.9 P7).

// importedChannel is the name the import gives the environment's bot.
const importedChannel = "telegram"

// envBotIsChannel reports whether the environment's bot is an enabled
// channel in the database, imported or added by hand: then the database
// is where notifications come from.
func (a *app) envBotIsChannel(ctx context.Context) (bool, error) {
	id := manage.TokenBotID(a.cfg.TelegramToken)
	cs, err := a.db.Channels(ctx, false)
	if err != nil {
		return false, err
	}
	for _, c := range cs {
		if c.Kind == store.ChannelTelegram && c.BotID() == id && id != 0 {
			return true, nil
		}
	}
	return false, nil
}

// importEnvTelegram is serve's import of YUUKA_TELEGRAM_*, run before
// openNotify decides where notifications go. It never fails serve: a
// failed import leaves the variables working, and without a key
// (sealer nil) openNotify says they are deprecated.
func (a *app) importEnvTelegram(ctx context.Context, log *slog.Logger, sealer *secret.Sealer) {
	if a.cfg.TelegramToken == "" || sealer == nil {
		return
	}
	if err := a.importTelegram(ctx, log, sealer); err != nil {
		log.Error("telegram: import from the environment failed; YUUKA_TELEGRAM_* keep working as before", "err", err)
	}
}

// importTelegram imports the environment's bot, once. Anything that
// keeps it from doing so leaves the variables working as before and says
// why; only a failed write is an error.
func (a *app) importTelegram(ctx context.Context, log *slog.Logger, sealer *secret.Sealer) error {
	c := a.cfg
	botID := manage.TokenBotID(c.TelegramToken)
	if botID == 0 {
		log.Warn("telegram: YUUKA_TELEGRAM_BOT_TOKEN does not look like <bot id>:<secret>; not imported")
		return nil
	}
	if in, err := a.envBotIsChannel(ctx); err != nil || in {
		return err
	}
	if _, err := a.db.ChannelByName(ctx, importedChannel); err == nil {
		log.Warn("telegram: a channel named " + importedChannel + " exists for another bot; YUUKA_TELEGRAM_* not imported")
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	who, err := a.importOperator(ctx)
	if err != nil {
		log.Warn("telegram: not imported yet: "+err.Error()+"; YUUKA_TELEGRAM_* keep working as before", "err", err)
		return nil
	}

	// getMe confirms the bot and gives its username for bind links. If
	// Telegram cannot be reached the token's own id stands, and links
	// say /start <code> instead: an upgrade must not wait on Telegram.
	settings := map[string]string{}
	gctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	bot, err := (&notify.Telegram{Token: c.TelegramToken, BaseURL: telegramAPI, Client: &http.Client{Timeout: 10 * time.Second}}).GetMe(gctx)
	cancel()
	switch {
	case err != nil:
		log.Warn("telegram: getMe failed; importing the bot without its username", "err", err)
	case bot.ID != botID:
		log.Warn("telegram: getMe names another bot than the token does; not imported", "token_bot", botID, "getme_bot", bot.ID)
		return nil
	default:
		settings["username"] = bot.Username
	}

	now := time.Now()
	aud, err := secret.TelegramBot(botID)
	if err != nil {
		return err
	}
	sealed, err := sealer.Seal(ctx, ulid.New(now), store.SecretTelegramBotToken, aud, []byte(c.TelegramToken))
	if err != nil {
		return err
	}
	actor := ""
	if c.TelegramOperatorID != 0 {
		actor = strconv.FormatInt(c.TelegramOperatorID, 10)
	}
	imp := store.ImportedTelegram{
		Channel: store.ChannelConfig{Name: importedChannel, Kind: store.ChannelTelegram, ExternalID: strconv.FormatInt(botID, 10), Settings: settings},
		Token:   sealed, Principal: who.ID, Address: c.TelegramChat, ActorID: actor,
		Label: "imported from YUUKA_TELEGRAM_*", Events: store.Events,
	}
	ok, err := a.db.ImportTelegram(ctx, imp, store.Action{
		ID: ulid.New(now), Actor: store.SystemPrincipalID, Tool: "config.import", At: now,
		Args: map[string]any{
			"via": "env", "kind": "channel", "name": importedChannel, "external_id": imp.Channel.ExternalID,
			"username": settings["username"], "audience": aud, "secret_id": sealed.ID, "wrapper": sealed.Wrapper,
			"key_ref": sealed.KeyRef, "principal": "human:" + who.Name, "chat": c.TelegramChat, "button_user": actor,
			"events": store.Events,
		},
	})
	switch {
	case err != nil:
		return err
	case ok:
		log.Info("telegram: imported YUUKA_TELEGRAM_* as channel "+importedChannel+"; remove the variables, they are deprecated",
			"principal", "human:"+who.Name)
	}
	return nil
}

// importOperator is who the environment's chat belongs to:
// YUUKA_TELEGRAM_OPERATOR, else the only human.
func (a *app) importOperator(ctx context.Context) (store.Principal, error) {
	if a.cfg.TelegramOperator != "" {
		kind, name, err := parsePrincipal(a.cfg.TelegramOperator)
		if err != nil {
			return store.Principal{}, err
		}
		p, err := a.db.PrincipalByName(ctx, kind, name)
		if err != nil || p.Kind != "human" || p.DisabledAt != nil {
			return p, fmt.Errorf("YUUKA_TELEGRAM_OPERATOR %s is not an enabled human", a.cfg.TelegramOperator)
		}
		return p, nil
	}
	hs, err := a.db.PrincipalsOfKind(ctx, "human")
	if err != nil {
		return store.Principal{}, err
	}
	if len(hs) != 1 {
		return store.Principal{}, fmt.Errorf("%d humans and no YUUKA_TELEGRAM_OPERATOR to say whose chat it is", len(hs))
	}
	return hs[0], nil
}
