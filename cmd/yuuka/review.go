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
	"github.com/yuuka-mail/yuuka/internal/store"
)

// The review tier's Telegram side (§8.4, §8.9): cards for held and
// escalated mail, sent through the bot whenever it is configured, and
// the recall button, answered by long polling only when the operator is
// configured too (YUUKA_TELEGRAM_OPERATOR_ID, YUUKA_TELEGRAM_OPERATOR).

// pollClientTimeout outlasts the long poll's 50 seconds.
const pollClientTimeout = 75 * time.Second

// recallPoller returns the poller of the recall button, nil when the
// operator is not configured. The operator must be an enabled human
// principal: a recall is recorded as them, with no credential and via
// telegram (like the CLI, there is no login to name).
func (a *app) recallPoller(ctx context.Context, log *slog.Logger) (*notify.Poller, error) {
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
	return &notify.Poller{
		TG: &notify.Telegram{
			Token: a.cfg.TelegramToken, ChatID: a.cfg.TelegramChat, Client: &http.Client{Timeout: pollClientTimeout},
		},
		OperatorID: a.cfg.TelegramOperatorID, ChatID: chat,
		Recall: recaller(a.db, &auth.Viewer{PrincipalID: op.ID, PrincipalKind: op.Kind, PrincipalName: op.Name}),
		Cards:  a.cards, Log: log,
	}, nil
}

// recaller withdraws held mail as v for a press of the recall button.
// The outcomes a person can act on are answers, not errors: a stale or
// replayed press is told what became of the message.
func recaller(db *store.DB, v *auth.Viewer) func(context.Context, string, int64) (string, bool, *notify.SuspendedCard, error) {
	return func(ctx context.Context, id string, from int64) (string, bool, *notify.SuspendedCard, error) {
		w, err := db.WithdrawOutbox(ctx, v, id, map[string]any{"via": "telegram", "telegram_user_id": from}, time.Now())
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
		card := &notify.SuspendedCard{Recalls: w.Suspended.Recalls, Withdrawn: len(w.Suspended.Withdrawn)}
		if i, err := db.IdentityByID(ctx, w.Suspended.IdentityID); err == nil {
			card.Identity = i.Address()
		}
		if p, err := db.Principal(ctx, w.Suspended.PrincipalID); err == nil {
			card.Principal = p.Kind + ":" + p.Name
		}
		return "Recalled. The grant is now suspended.", true, card, nil
	}
}
