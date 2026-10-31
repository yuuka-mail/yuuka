package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// Channels from the database (§17, §8.4). The Dispatcher sends every
// event to the bindings that should have it, looked up when the event
// happens: no cache of bindings or subscriptions, so a revocation counts
// from the next event on (§17.5). What a binding may be told is decided
// by its person's visibility, through the same read paths as the WebUI
// (§13.8). The Manager runs one Poller per enabled Telegram channel.
//
// Bot tokens are opened here and nowhere else, for the audience
// recomputed from the channel row (telegram:bot<id>, §17.2). A client
// is kept per channel and rebuilt when the channel or its secret moves
// to another revision; that cache is the only one. The token then lives
// in the client as a string, as the Bot API's URL needs it: §17.2's
// in-memory hygiene ends at the HTTP client, as it does for SMTP.

// DispatcherConfig configures a Dispatcher.
type DispatcherConfig struct {
	DB *store.DB
	// Sealer opens bot tokens; nil is the locked state, in which nothing
	// is sent.
	Sealer *secret.Sealer
	// Origin is the WebUI's origin, for the buttons that open it.
	Origin string
	// Location is where cards tell the time; nil is the local zone.
	Location *time.Location
	Log      *slog.Logger
	// BaseURL and Client reach the Bot API; tests set BaseURL. Client
	// defaults to one with a 15 s timeout.
	BaseURL string
	Client  *http.Client
}

// Dispatcher sends events to the bindings subscribed to them, and
// security alerts to administrators and the person they are about. It
// is the ingest Notifier, the submission service's card sender and the
// login's Alerter when channels come from the database.
type Dispatcher struct {
	cfg DispatcherConfig
	log *slog.Logger

	mu   sync.Mutex
	bots map[string]openBot
}

// openBot is a channel's client as of a channel and secret revision, or
// why it could not be built.
type openBot struct {
	channelRev, secretRev int64
	tg                    *Telegram
	err                   error
}

// NewDispatcher returns a Dispatcher.
func NewDispatcher(cfg DispatcherConfig) *Dispatcher {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 15 * time.Second}
	}
	return &Dispatcher{cfg: cfg, log: cfg.Log, bots: map[string]openBot{}}
}

// Locked reports whether the dispatcher has no key to open tokens with.
func (d *Dispatcher) Locked() bool { return d.cfg.Sealer == nil }

// Bot returns the client of channel c, opening its token once per
// revision. A token that fails to authenticate raises a security alert
// once per revision, through whatever other channel still works (§17.3).
func (d *Dispatcher) Bot(ctx context.Context, c store.ChannelWithSecret) (*Telegram, error) {
	d.mu.Lock()
	o, ok := d.bots[c.ID]
	d.mu.Unlock()
	if ok && o.channelRev == c.Revision && o.secretRev == c.Secret.Revision {
		return o.tg, o.err
	}
	o = openBot{channelRev: c.Revision, secretRev: c.Secret.Revision}
	o.tg, o.err = d.open(ctx, c)
	d.mu.Lock()
	d.bots[c.ID] = o
	d.mu.Unlock()
	if o.err != nil {
		d.log.Error("channel locked: its token cannot be opened", "channel", c.Name, "wrapper", c.Secret.Wrapper,
			"key_ref", c.Secret.KeyRef, "err", o.err)
		if errors.Is(o.err, secret.ErrTampered) {
			d.alertOthers(ctx, c.ID, fmt.Sprintf("yuuka: the bot token of channel %s failed authentication: "+
				"the database was altered or corrupted. The channel is locked; set its token again with `yuuka channel add`.", c.Name))
		}
	}
	return o.tg, o.err
}

func (d *Dispatcher) open(ctx context.Context, c store.ChannelWithSecret) (*Telegram, error) {
	aud, err := c.Audience()
	if err != nil {
		return nil, err
	}
	v, err := d.cfg.Sealer.Open(ctx, c.Secret.Record, aud)
	if err != nil {
		return nil, err
	}
	defer v.Close()
	return &Telegram{Token: string(v.Reveal()), BaseURL: d.cfg.BaseURL, Client: d.cfg.Client}, nil
}

// channels returns the enabled channels by id.
func (d *Dispatcher) channels(ctx context.Context) (map[string]store.ChannelWithSecret, error) {
	cs, err := d.cfg.DB.EnabledChannels(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]store.ChannelWithSecret, len(cs))
	for _, c := range cs {
		out[c.ID] = c
	}
	return out, nil
}

// send calls f for each binding in bs that may see the event (visible,
// asked with the binding's person as viewer; nil for always), with the
// binding's chat as client. A binding that fails does not stop the
// others; the errors are joined.
func (d *Dispatcher) send(ctx context.Context, bs []store.Binding, visible func(*auth.Viewer) (bool, error),
	f func(*Telegram, store.Binding) error,
) error {
	if len(bs) == 0 || d.Locked() {
		return nil
	}
	chans, err := d.channels(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, b := range bs {
		if visible != nil {
			ok, err := visible(b.Viewer())
			if err != nil {
				errs = append(errs, fmt.Errorf("binding %s: %w", b.ID, err))
				continue
			}
			if !ok {
				continue
			}
		}
		c, ok := chans[b.ChannelID]
		if !ok {
			continue // disabled since the bindings were read
		}
		tg, err := d.Bot(ctx, c)
		if err != nil {
			errs = append(errs, fmt.Errorf("binding %s: channel %s locked", b.ID, c.Name))
			continue
		}
		if err := f(tg.To(b.Address), b); err != nil {
			errs = append(errs, fmt.Errorf("binding %s: %w", b.ID, err))
		}
	}
	return errors.Join(errs...)
}

// event sends an event to its subscribers.
func (d *Dispatcher) event(ctx context.Context, event string, visible func(*auth.Viewer) (bool, error),
	f func(*Telegram, store.Binding) error,
) error {
	if d.Locked() {
		return nil
	}
	bs, err := d.cfg.DB.SubscribedBindings(ctx, event)
	if err != nil {
		return err
	}
	return d.send(ctx, bs, visible, f)
}

// NewMail sends the summary of new mail (mail.new) to the bindings whose
// person may see the delivery: the summary names its sender, subject and
// prefix, so it is a read path (§8.4, §13.8).
func (d *Dispatcher) NewMail(ctx context.Context, m Mail) error {
	return d.event(ctx, store.EventMailNew, func(v *auth.Viewer) (bool, error) {
		return d.cfg.DB.DeliveryVisible(ctx, v, m.DeliveryID)
	}, func(tg *Telegram, _ store.Binding) error { return tg.NewMail(ctx, m) })
}

// cards is the card sender of binding b: the recall button only where a
// button user is bound.
func (d *Dispatcher) cards(tg *Telegram, b store.Binding) *Cards {
	return &Cards{TG: tg, Origin: d.cfg.Origin, Location: d.cfg.Location, Recall: b.ActorID != ""}
}

// Held sends the card of a held message (send.held) to the bindings
// whose person may see its outbox row.
func (d *Dispatcher) Held(ctx context.Context, c HeldCard) error {
	return d.event(ctx, store.EventSendHeld, func(v *auth.Viewer) (bool, error) {
		return found(d.cfg.DB.OutboxByID(ctx, v, c.OutboxID))
	}, func(tg *Telegram, b store.Binding) error { return d.cards(tg, b).Held(ctx, c) })
}

// Escalated sends the card of an escalated submission (send.escalated)
// to the bindings whose person may see the draft.
func (d *Dispatcher) Escalated(ctx context.Context, c EscalatedCard) error {
	return d.event(ctx, store.EventSendEscalated, func(v *auth.Viewer) (bool, error) {
		return found(d.cfg.DB.DraftByID(ctx, v, c.DraftID))
	}, func(tg *Telegram, b store.Binding) error { return d.cards(tg, b).Escalated(ctx, c) })
}

// Suspended sends the card of a suspended grant (grant.suspended) to the
// bindings whose person may see the identity's mail.
func (d *Dispatcher) Suspended(ctx context.Context, c SuspendedCard) error {
	return d.event(ctx, store.EventGrantSuspended, func(v *auth.Viewer) (bool, error) {
		return d.cfg.DB.IdentityMailVisible(ctx, v, c.IdentityID)
	}, func(tg *Telegram, b store.Binding) error { return d.cards(tg, b).Suspended(ctx, c) })
}

func found[T any](_ T, err error) (bool, error) {
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Alert is a security alert (§8.4): a new passkey, a used recovery
// code, a widening, a locked or tampered secret.
type Alert struct {
	// Subject is the person it is about, "" for none: they get it on all
	// their bindings, administrator or not.
	Subject string
	// Except is a binding left out: the one just made, which may be the
	// attacker's (§13.7).
	Except string
	Text   string
}

// Security sends a to every live binding of every administrator and of
// a.Subject. It cannot be subscribed to or away from.
func (d *Dispatcher) Security(ctx context.Context, a Alert) error {
	if d.Locked() {
		return nil
	}
	bs, err := d.cfg.DB.SecurityBindings(ctx, a.Subject)
	if err != nil {
		return err
	}
	keep := bs[:0]
	for _, b := range bs {
		if b.ID != a.Except {
			keep = append(keep, b)
		}
	}
	return d.send(ctx, keep, nil, func(tg *Telegram, _ store.Binding) error { return tg.Notify(ctx, a.Text) })
}

// Notify sends text as a security alert to the administrators: the
// Alerter of logins and of widenings in the WebUI, whose subjects are
// administrators themselves today. It is also the seam for the outbox's
// alert on a tampered transport secret.
func (d *Dispatcher) Notify(ctx context.Context, text string) error {
	return d.Security(ctx, Alert{Text: text})
}

// TransportSecretAlert is the outbox's security alert (outbox.
// SecurityAlerter) for a transport secret that does not open as sealed:
// tampered, or sealed for another destination than the row names. It
// goes to the administrators, in the background: the outbox calls it
// from a worker and must not wait on Telegram.
func (d *Dispatcher) TransportSecretAlert(ctx context.Context, transport string, err error) {
	text := fmt.Sprintf("yuuka: the password of transport %s does not open as it was sealed (%v): the database "+
		"was altered or corrupted. The transport is locked; set its password again with `yuuka transport set`.", transport, err)
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := d.Security(ctx, Alert{Text: text}); err != nil {
			d.log.Error("security alert", "transport", transport, "err", err)
		}
	}()
}

// alertOthers raises a security alert on the bindings of every channel
// but channel, which cannot be trusted to carry it. Failure is logged.
func (d *Dispatcher) alertOthers(ctx context.Context, channel, text string) {
	bs, err := d.cfg.DB.SecurityBindings(ctx, "")
	if err != nil {
		d.log.Error("security alert", "err", err)
		return
	}
	keep := bs[:0]
	for _, b := range bs {
		if b.ChannelID != channel {
			keep = append(keep, b)
		}
	}
	// A channel that fails in turn alerts on its own; send skips it.
	if err := d.send(ctx, keep, nil, func(tg *Telegram, _ store.Binding) error { return tg.Notify(ctx, text) }); err != nil {
		d.log.Error("security alert", "err", err)
	}
}

// Test sends a test message to the live bindings of channel name, or to
// binding only when it is not "". It returns how many were reached.
func (d *Dispatcher) Test(ctx context.Context, name, binding string) (int, error) {
	bs, err := d.cfg.DB.Bindings(ctx, false)
	if err != nil {
		return 0, err
	}
	var to []store.Binding
	for _, b := range bs {
		if b.ChannelName == name && (binding == "" || b.ID == binding) {
			to = append(to, b)
		}
	}
	n := 0
	err = d.send(ctx, to, nil, func(tg *Telegram, b store.Binding) error {
		err := tg.Notify(ctx, "yuuka · test\nThis chat is bound to "+b.PrincipalKind+":"+b.PrincipalName+
			" on channel "+b.ChannelName+" (binding "+b.ID+").")
		if err == nil {
			n++
		}
		return err
	})
	return n, err
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
