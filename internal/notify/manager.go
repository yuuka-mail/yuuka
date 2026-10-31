package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yuuka-mail/yuuka/internal/store"
)

// Manager runs one Poller per enabled Telegram channel (§17.5). It reads
// the channel table every Interval and restarts a channel's poller when
// the channel or its secret moved to another revision; a new channel or
// a new token takes at most that long. No reload signal, no
// LISTEN/NOTIFY: channels change on a scale of days.
//
// Without a key (the dispatcher is locked) nothing runs: every token is
// sealed. The manager then says so at start and every LockedEvery while
// any channel is enabled (§17.3).
type Manager struct {
	D *Dispatcher
	// Interval is how often the table is read (default 15 s).
	Interval time.Duration
	// LockedEvery is how often the locked state is logged (default 10 min).
	LockedEvery time.Duration
	// PollClient is the pollers' HTTP client; its timeout must outlast
	// the long poll (default 75 s).
	PollClient *http.Client
	// PollTimeout is the long-poll wait in seconds (default 50).
	PollTimeout int
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// running is a poller of one channel revision.
type running struct {
	channelRev, secretRev int64
	stop                  context.CancelFunc
	done                  chan struct{}
}

// Run manages the pollers until ctx ends, then stops them all.
func (m *Manager) Run(ctx context.Context) {
	log := m.D.log
	interval, lockedEvery := m.Interval, m.LockedEvery
	if interval <= 0 {
		interval = 15 * time.Second
	}
	if lockedEvery <= 0 {
		lockedEvery = 10 * time.Minute
	}
	if m.D.Locked() {
		m.locked(ctx, log, lockedEvery)
		return
	}
	pollers := map[string]*running{}
	defer func() {
		for _, r := range pollers {
			r.stop()
			<-r.done
		}
	}()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if err := m.reconcile(ctx, pollers); err != nil && ctx.Err() == nil {
			log.Error("channels: read", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// locked reports the locked state while any channel is enabled.
func (m *Manager) locked(ctx context.Context, log *slog.Logger, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if cs, err := m.D.cfg.DB.Channels(ctx, false); err == nil && len(cs) > 0 {
			log.Error("channels are locked: YUUKA_KEK_FILE is not set, so no bot token can be opened; "+
				"no channel polls or sends", "channels", len(cs))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// reconcile starts, restarts and stops pollers to match the table.
func (m *Manager) reconcile(ctx context.Context, pollers map[string]*running) error {
	cs, err := m.D.cfg.DB.EnabledChannels(ctx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, c := range cs {
		seen[c.ID] = true
		r := pollers[c.ID]
		if r != nil && r.channelRev == c.Revision && r.secretRev == c.Secret.Revision {
			continue
		}
		if r != nil {
			r.stop()
			<-r.done
			delete(pollers, c.ID)
		}
		tg, err := m.D.Bot(ctx, c)
		if err != nil {
			continue // logged by Bot, once per revision
		}
		pctx, stop := context.WithCancel(ctx)
		r = &running{channelRev: c.Revision, secretRev: c.Secret.Revision, stop: stop, done: make(chan struct{})}
		pollers[c.ID] = r
		p := m.poller(c.Channel, tg)
		go func() {
			defer close(r.done)
			p.Run(pctx)
		}()
		m.D.log.Info("channel: polling", "channel", c.Name, "revision", c.Revision)
	}
	for id, r := range pollers {
		if !seen[id] {
			r.stop()
			<-r.done
			delete(pollers, id)
		}
	}
	return nil
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// poller is the Poller of channel c. Everything it acts on is read when
// the update arrives: who may press, which code is open.
func (m *Manager) poller(c store.Channel, tg *Telegram) *Poller {
	client := m.PollClient
	if client == nil {
		client = &http.Client{Timeout: 75 * time.Second}
	}
	pt := *tg
	pt.Client = client
	db, log := m.D.cfg.DB, m.D.log.With("channel", c.Name)
	return &Poller{
		TG: &pt, Log: log, Timeout: m.PollTimeout,
		// §13.7: the bot, chat.id, from.id, a binding that is not
		// revoked and an enabled human, all read on every press. The
		// recall is the binding's person's, recorded with the binding.
		Authorize: func(ctx context.Context, chat, from int64) (Recall, error) {
			b, err := db.BindingForPress(ctx, c.ID, itoa(chat), itoa(from), m.now())
			if errors.Is(err, store.ErrNotFound) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			return Recaller(db, b.Viewer(), map[string]any{
				"via": "telegram", "telegram_user_id": from, "binding_id": b.ID,
			}), nil
		},
		Start: func(ctx context.Context, msg *Message) string {
			code := StartCode(msg.Text)
			if code == "" || msg.From == nil {
				return ""
			}
			seen := store.Seen{Address: itoa(msg.Chat.ID), ActorID: itoa(msg.From.ID), Label: seenLabel(msg), ChatKind: msg.Chat.Type}
			_, err := db.RedeemBindCode(ctx, c.ID, code, seen, m.now())
			switch {
			case errors.Is(err, store.ErrBindCode):
				return "This code is not valid, has expired or was already used. Ask yuuka for a new one."
			case err != nil:
				log.Error("telegram: redeem bind code", "err", err)
				return "Something went wrong; send it again."
			}
			return fmt.Sprintf("Received. Nothing is bound yet: confirm in yuuka that this is you (%s, user id %s, %s chat).",
				seen.Label, seen.ActorID, seen.ChatKind)
		},
		Suspended: m.D.Suspended,
	}
}

// seenLabel names who sent a /start: "Alice Liddell (@alice)", and the
// group's title if it was sent in one. It is shown to the person who
// confirms, so it is cleaned like any text from outside.
func seenLabel(m *Message) string {
	name := strings.TrimSpace(m.From.FirstName + " " + m.From.LastName)
	if m.From.Username != "" {
		name = strings.TrimSpace(name + " (@" + m.From.Username + ")")
	}
	if m.Chat.Type != "private" && m.Chat.Title != "" {
		name += " in " + m.Chat.Title
	}
	return clean(name)
}
