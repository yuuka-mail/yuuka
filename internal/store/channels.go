package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Notification channels, their bindings to people, bind codes and
// subscriptions (§17, §8.4, §13.7, migration 0018). Like transports they
// are configuration: what a binding may receive is decided at push time
// by the read paths of §13.8 (DeliveryVisible and friends), never here.
//
// The store persists the bot token as a secret.Record and never opens
// it. What it enforces is the shape §17 relies on: a channel moves to
// another bot only with a token sealed for that bot, a binding belongs to
// an enabled human, a binding is made only from a code its person
// confirmed (or by the import from the host's environment, §17.7), and no
// action row carries a token, a code or a hash of either.

// SecretTelegramBotToken is the secrets.kind of a bot token.
const SecretTelegramBotToken = "telegram_bot_token"

// ChannelTelegram is the only channel kind so far.
const ChannelTelegram = "telegram"

// The events a binding can subscribe to (notification_subscriptions).
const (
	EventMailNew        = "mail.new"
	EventSendHeld       = "send.held"
	EventSendEscalated  = "send.escalated"
	EventGrantSuspended = "grant.suspended"
)

// Events is every subscribable event, in the order of §8.4. security is
// not one: it goes to administrators and the person it is about, always.
var Events = []string{EventMailNew, EventSendHeld, EventSendEscalated, EventGrantSuspended}

// BindCodeTTL is how long a bind code is good for: the /start and the
// confirmation both within it (§13.7).
const BindCodeTTL = 10 * time.Minute

var (
	// ErrBindCode: no bind code on this channel matches, or it expired,
	// or it was used. One error for all, so /start tells a stranger
	// nothing about which.
	ErrBindCode = errors.New("bind code is not valid, expired or already used")
	// ErrNotHuman: bindings and bind codes are for enabled humans only;
	// pushes go to people (§13.8).
	ErrNotHuman = errors.New("only an enabled human can have a binding")
	// ErrNotRedeemed: nobody has sent /start with the code yet.
	ErrNotRedeemed = errors.New("the code has not been sent to the bot yet")
	// ErrChannelBot: an enabled channel cannot move to another bot; its
	// bindings are chats with the old one. Remove it and add it again.
	ErrChannelBot = errors.New("an enabled channel cannot move to another bot")
)

// Channel is a notification_channels row.
type Channel struct {
	ID         string
	Name       string
	Kind       string
	ExternalID string
	// Settings are the kind's non-secret settings (Telegram: username).
	Settings map[string]string
	// SecretID is "" when disabled.
	SecretID   string
	DisabledAt *time.Time
	Revision   int64
	CreatedAt  time.Time
}

// BotID is a Telegram channel's bot id, 0 if it has none.
func (c Channel) BotID() int64 {
	if c.Kind != ChannelTelegram {
		return 0
	}
	n, _ := strconv.ParseInt(c.ExternalID, 10, 64)
	return n
}

// Audience is the audience the channel's secret must have been sealed
// for, recomputed from the row (§17.2).
func (c Channel) Audience() (string, error) {
	if c.Kind != ChannelTelegram {
		return "", fmt.Errorf("channel %s: unknown kind %q", c.Name, c.Kind)
	}
	return secret.TelegramBot(c.BotID())
}

// ChannelConfig is what configuring a channel sets.
type ChannelConfig struct {
	Name string
	Kind string
	// ExternalID: Telegram, the bot's numeric id as getMe reported it.
	ExternalID string
	Settings   map[string]string
}

const channelCols = `c.id, c.name, c.kind, c.external_id, c.settings, coalesce(c.secret_id, ''), c.disabled_at, c.revision, c.created_at`

func scanChannel(row pgx.Row) (Channel, error) {
	var c Channel
	var settings []byte
	err := row.Scan(&c.ID, &c.Name, &c.Kind, &c.ExternalID, &settings, &c.SecretID, &c.DisabledAt, &c.Revision, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(settings, &c.Settings); err != nil {
		return c, fmt.Errorf("channel %s: settings: %w", c.Name, err)
	}
	return c, nil
}

var (
	telegramBotID    = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
	telegramUsername = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
	// A chat id, or @channelusername (only the environment's chat can be
	// one; /start always reports a number).
	telegramChat = regexp.MustCompile(`^(-?[1-9][0-9]{0,18}|@[A-Za-z0-9_]{1,64})$`)
	telegramUser = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
)

func checkChannelConfig(c ChannelConfig) (ChannelConfig, error) {
	if !ValidName(c.Name) {
		return c, fmt.Errorf("channel name %q: want [A-Za-z0-9][A-Za-z0-9._-]*, at most 64", c.Name)
	}
	if c.Kind != ChannelTelegram {
		return c, fmt.Errorf("channel %s: kind %q: want telegram", c.Name, c.Kind)
	}
	if !telegramBotID.MatchString(c.ExternalID) {
		return c, fmt.Errorf("channel %s: bot id %q is not a positive number", c.Name, c.ExternalID)
	}
	if c.Settings == nil {
		c.Settings = map[string]string{}
	}
	for k, v := range c.Settings {
		if k != "username" || !telegramUsername.MatchString(v) {
			return c, fmt.Errorf("channel %s: setting %s=%q: a telegram channel has only username, [A-Za-z0-9_]", c.Name, k, v)
		}
	}
	return c, nil
}

// checkChannelSecret checks that s was sealed for c's bot. Like
// checkTransportSecret it is not the security boundary, which is the
// audience recomputed on open; it reports a wrong seal now.
func checkChannelSecret(c ChannelConfig, s secret.Record) error {
	if s.Kind != SecretTelegramBotToken {
		return fmt.Errorf("secret kind %q, want %s", s.Kind, SecretTelegramBotToken)
	}
	want, err := Channel{Kind: c.Kind, ExternalID: c.ExternalID, Name: c.Name}.Audience()
	if err != nil {
		return err
	}
	if s.Audience != want {
		return fmt.Errorf("secret sealed for %q, not for this channel's %q", s.Audience, want)
	}
	return nil
}

// Channels lists channels by name, disabled ones too when all.
func (db *DB) Channels(ctx context.Context, all bool) ([]Channel, error) {
	rows, err := db.pool.Query(ctx, `SELECT `+channelCols+` FROM notification_channels c
		WHERE $1 OR c.disabled_at IS NULL ORDER BY c.name`, all)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Channel, error) { return scanChannel(r) })
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	return out, nil
}

// ChannelByName returns the channel called name, disabled or not.
func (db *DB) ChannelByName(ctx context.Context, name string) (Channel, error) {
	c, err := scanChannel(db.pool.QueryRow(ctx, `SELECT `+channelCols+` FROM notification_channels c WHERE c.name = $1`, name))
	if err != nil {
		return c, fmt.Errorf("channel %s: %w", name, err)
	}
	return c, nil
}

// ChannelWithSecret is an enabled channel and its secret, for whoever
// opens the token: the channel manager and the dispatcher (§17.5).
type ChannelWithSecret struct {
	Channel
	Secret StoredSecret
}

// EnabledChannels returns every enabled channel with its secret, by
// name.
func (db *DB) EnabledChannels(ctx context.Context) ([]ChannelWithSecret, error) {
	return db.channelsWithSecret(ctx, "")
}

// EnabledChannel returns enabled channel id with its secret.
// ErrNotFound if it is not enabled.
func (db *DB) EnabledChannel(ctx context.Context, id string) (ChannelWithSecret, error) {
	out, err := db.channelsWithSecret(ctx, id)
	if err != nil {
		return ChannelWithSecret{}, err
	}
	if len(out) == 0 {
		return ChannelWithSecret{}, fmt.Errorf("channel %s: %w", id, ErrNotFound)
	}
	return out[0], nil
}

func (db *DB) channelsWithSecret(ctx context.Context, id string) ([]ChannelWithSecret, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT `+channelCols+`, s.id, s.kind, s.audience, s.wrapper, s.key_ref, s.wrapped_dek, s.payload,
			s.revision, s.created_at, s.sealed_at
		FROM notification_channels c JOIN secrets s ON s.id = c.secret_id
		WHERE c.disabled_at IS NULL AND ($1 = '' OR c.id = $1)
		ORDER BY c.name`, id)
	if err != nil {
		return nil, fmt.Errorf("enabled channels: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ChannelWithSecret, error) {
		var c ChannelWithSecret
		var settings []byte
		s := &c.Secret
		err := r.Scan(&c.ID, &c.Name, &c.Kind, &c.ExternalID, &settings, &c.SecretID, &c.DisabledAt, &c.Revision, &c.CreatedAt,
			&s.ID, &s.Kind, &s.Audience, &s.Wrapper, &s.KeyRef, &s.WrappedDEK, &s.Payload, &s.Revision, &s.CreatedAt, &s.SealedAt)
		if err != nil {
			return c, err
		}
		return c, json.Unmarshal(settings, &c.Settings)
	})
	if err != nil {
		return nil, fmt.Errorf("enabled channels: %w", err)
	}
	return out, nil
}

// ConfigureChannel creates (revision 0) or rewrites (revision = the one
// read) the channel c.Name, and records a. Rewriting a disabled channel
// enables it again, with no bindings: removing it revoked them.
//
// sealed is a new token for it, or nil to keep the one it has. It is
// required for a new or disabled channel, and must be a
// telegram_bot_token sealed for secret.TelegramBot(c.ExternalID) under a
// fresh id; it replaces the old secret, which is deleted. An enabled
// channel keeps its bot (ErrChannelBot): its bindings are chats with
// that bot.
//
// ErrDuplicate if revision is 0 and the name exists, or another enabled
// channel has this bot; ErrNotFound if revision is not 0 and the name
// does not exist; ErrStale if its revision moved on; ErrNeedsSecret.
func (db *DB) ConfigureChannel(ctx context.Context, c ChannelConfig, revision int64, sealed *secret.Record, a Action) (Channel, error) {
	c, err := checkChannelConfig(c)
	if err != nil {
		return Channel{}, err
	}
	if sealed != nil {
		if err := checkChannelSecret(c, *sealed); err != nil {
			return Channel{}, fmt.Errorf("channel %s: %w", c.Name, err)
		}
	}
	settings, err := json.Marshal(c.Settings)
	if err != nil {
		return Channel{}, err
	}
	var out Channel
	err = pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if revision == 0 {
			if sealed == nil {
				return ErrNeedsSecret
			}
			if err := insertSecret(ctx, tx, *sealed, a.At); err != nil {
				return err
			}
			id := ulid.New(a.At)
			_, err := tx.Exec(ctx, `
				INSERT INTO notification_channels (id, name, kind, external_id, settings, secret_id, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`, id, c.Name, c.Kind, c.ExternalID, settings, sealed.ID, a.At)
			if err != nil {
				return uniqueAsDuplicate(err)
			}
			if err := insertAction(ctx, tx, a); err != nil {
				return err
			}
			out, err = scanChannel(tx.QueryRow(ctx, `SELECT `+channelCols+` FROM notification_channels c WHERE c.id = $1`, id))
			return err
		}
		cur, err := scanChannel(tx.QueryRow(ctx, `SELECT `+channelCols+` FROM notification_channels c WHERE c.name = $1 FOR UPDATE`, c.Name))
		if err != nil {
			return err
		}
		if cur.Revision != revision {
			return fmt.Errorf("revision %d, read %d: %w", cur.Revision, revision, ErrStale)
		}
		if cur.Kind != c.Kind {
			return fmt.Errorf("channel %s is a %s channel", c.Name, cur.Kind)
		}
		switch {
		case cur.DisabledAt == nil && cur.ExternalID != c.ExternalID:
			return ErrChannelBot
		case sealed == nil && cur.DisabledAt != nil:
			return ErrNeedsSecret
		}
		secretID := cur.SecretID
		if sealed != nil {
			if err := insertSecret(ctx, tx, *sealed, a.At); err != nil {
				return err
			}
			secretID = sealed.ID
		}
		_, err = tx.Exec(ctx, `
			UPDATE notification_channels SET external_id = $2, settings = $3, secret_id = $4, disabled_at = NULL,
				revision = revision + 1
			WHERE id = $1`, cur.ID, c.ExternalID, settings, secretID)
		if err != nil {
			return uniqueAsDuplicate(err)
		}
		if cur.SecretID != "" && cur.SecretID != secretID {
			if _, err := tx.Exec(ctx, `DELETE FROM secrets WHERE id = $1`, cur.SecretID); err != nil {
				return err
			}
		}
		if err := insertAction(ctx, tx, a); err != nil {
			return err
		}
		out, err = scanChannel(tx.QueryRow(ctx, `SELECT `+channelCols+` FROM notification_channels c WHERE c.id = $1`, cur.ID))
		return err
	})
	if err != nil {
		return Channel{}, fmt.Errorf("configure channel %s: %w", c.Name, err)
	}
	return out, nil
}

func uniqueAsDuplicate(err error) error {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		return fmt.Errorf("%s: %w", pgErr.ConstraintName, ErrDuplicate)
	}
	return err
}

// DisableChannel is `channel rm`: it disables channel name at revision,
// erases its secret, revokes its bindings, drops its open bind codes and
// records a. ErrNotFound if no enabled channel has that name; ErrStale if
// its revision moved on.
func (db *DB) DisableChannel(ctx context.Context, name string, revision int64, a Action) error {
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		cur, err := scanChannel(tx.QueryRow(ctx, `SELECT `+channelCols+` FROM notification_channels c
			WHERE c.name = $1 AND c.disabled_at IS NULL FOR UPDATE`, name))
		if err != nil {
			return err
		}
		if cur.Revision != revision {
			return fmt.Errorf("revision %d, read %d: %w", cur.Revision, revision, ErrStale)
		}
		if _, err := tx.Exec(ctx, `UPDATE notification_channels SET disabled_at = $2, secret_id = NULL, revision = revision + 1
			WHERE id = $1`, cur.ID, a.At); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM secrets WHERE id = $1`, cur.SecretID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE channel_bindings SET revoked_at = $2 WHERE channel_id = $1 AND revoked_at IS NULL`,
			cur.ID, a.At); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM channel_bind_codes WHERE channel_id = $1 AND binding_id IS NULL`, cur.ID); err != nil {
			return err
		}
		return insertAction(ctx, tx, a)
	})
	if err != nil {
		return fmt.Errorf("disable channel %s: %w", name, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Bindings

// Binding is a channel_bindings row, with its channel's and principal's
// names and its subscriptions.
type Binding struct {
	ID            string
	ChannelID     string
	ChannelName   string
	PrincipalID   string
	PrincipalKind string
	PrincipalName string
	// Address is the chat id.
	Address string
	// ActorID is the Telegram user whose presses count; "" for a binding
	// without buttons.
	ActorID    string
	Label      string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
	// Events are its subscriptions, in the order of Events.
	Events []string
}

// Viewer is who a push to b, or a press on it, acts and reads as: its
// person. The reads that filter pushes (§13.8) use nothing else of it.
func (b Binding) Viewer() *auth.Viewer {
	return &auth.Viewer{PrincipalID: b.PrincipalID, PrincipalKind: b.PrincipalKind, PrincipalName: b.PrincipalName}
}

const bindingCols = `b.id, b.channel_id, c.name, b.principal_id, p.kind, p.name, b.address, coalesce(b.actor_external_id, ''),
	b.label, b.created_at, b.last_used_at, b.revoked_at,
	ARRAY(SELECT ns.event FROM notification_subscriptions ns WHERE ns.binding_id = b.id ORDER BY ns.event)`

const bindingFrom = ` FROM channel_bindings b JOIN notification_channels c ON c.id = b.channel_id
	JOIN principals p ON p.id = b.principal_id`

// liveBinding is the condition under which a binding receives pushes and
// its presses count: not revoked, on an enabled channel, of an enabled
// human. Each is read when the event happens, never cached (§17.5).
const liveBinding = `b.revoked_at IS NULL AND c.disabled_at IS NULL AND p.kind = 'human' AND p.disabled_at IS NULL`

func scanBindings(rows pgx.Rows) ([]Binding, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Binding, error) {
		var b Binding
		err := r.Scan(&b.ID, &b.ChannelID, &b.ChannelName, &b.PrincipalID, &b.PrincipalKind, &b.PrincipalName, &b.Address,
			&b.ActorID, &b.Label, &b.CreatedAt, &b.LastUsedAt, &b.RevokedAt, &b.Events)
		slices.SortFunc(b.Events, func(x, y string) int { return slices.Index(Events, x) - slices.Index(Events, y) })
		return b, err
	})
}

// BindingByID returns binding id, revoked or not.
func (db *DB) BindingByID(ctx context.Context, id string) (Binding, error) {
	rows, err := db.pool.Query(ctx, `SELECT `+bindingCols+bindingFrom+` WHERE b.id = $1`, id)
	if err != nil {
		return Binding{}, fmt.Errorf("binding %s: %w", id, err)
	}
	out, err := scanBindings(rows)
	if err != nil {
		return Binding{}, fmt.Errorf("binding %s: %w", id, err)
	}
	if len(out) == 0 {
		return Binding{}, fmt.Errorf("binding %s: %w", id, ErrNotFound)
	}
	return out[0], nil
}

// Bindings lists bindings oldest first: the active ones, or with all
// the revoked ones and those on disabled channels too.
func (db *DB) Bindings(ctx context.Context, all bool) ([]Binding, error) {
	rows, err := db.pool.Query(ctx, `SELECT `+bindingCols+bindingFrom+`
		WHERE $1 OR (b.revoked_at IS NULL AND c.disabled_at IS NULL) ORDER BY b.id`, all)
	if err != nil {
		return nil, fmt.Errorf("list bindings: %w", err)
	}
	out, err := scanBindings(rows)
	if err != nil {
		return nil, fmt.Errorf("list bindings: %w", err)
	}
	return out, nil
}

// SubscribedBindings returns the live bindings subscribed to event. The
// dispatcher calls it for every event, so a revocation or unsubscription
// takes effect on the next one (§17.5). Whether a binding may see what
// the event is about is the caller's to check, with the read path of
// that object (§13.8).
func (db *DB) SubscribedBindings(ctx context.Context, event string) ([]Binding, error) {
	if !slices.Contains(Events, event) {
		return nil, fmt.Errorf("unknown event %q", event)
	}
	rows, err := db.pool.Query(ctx, `SELECT `+bindingCols+bindingFrom+`
		WHERE `+liveBinding+` AND EXISTS (SELECT 1 FROM notification_subscriptions s WHERE s.binding_id = b.id AND s.event = $1)
		ORDER BY b.id`, event)
	if err != nil {
		return nil, fmt.Errorf("bindings for %s: %w", event, err)
	}
	out, err := scanBindings(rows)
	if err != nil {
		return nil, fmt.Errorf("bindings for %s: %w", event, err)
	}
	return out, nil
}

// isAdmin is the condition "principal p is an administrator", the
// recipients of security alerts (§13.10): an enabled human whose passkey
// sessions carry admin. Sessions carry every scope today (auth.AllScopes),
// so that is every enabled human; when sessions get per-principal scopes,
// this is where it changes.
const isAdmin = `p.kind = 'human' AND p.disabled_at IS NULL`

// SecurityBindings returns where a security alert about subject goes
// (§8.4): every live binding of every administrator, and of subject
// ("" for none) whether an administrator or not. It ignores
// subscriptions: security cannot be subscribed to or away from.
func (db *DB) SecurityBindings(ctx context.Context, subject string) ([]Binding, error) {
	rows, err := db.pool.Query(ctx, `SELECT `+bindingCols+bindingFrom+`
		WHERE `+liveBinding+` AND ((`+isAdmin+`) OR b.principal_id = $1)
		ORDER BY b.id`, subject)
	if err != nil {
		return nil, fmt.Errorf("security bindings: %w", err)
	}
	out, err := scanBindings(rows)
	if err != nil {
		return nil, fmt.Errorf("security bindings: %w", err)
	}
	return out, nil
}

// BindingForPress authenticates a button press (§13.7): the live binding
// on channel whose chat is address and whose button user is actor. It
// is read on every press, and stamps last_used_at. ErrNotFound when
// nothing matches: the press does not count.
func (db *DB) BindingForPress(ctx context.Context, channel, address, actor string, at time.Time) (Binding, error) {
	var id string
	err := db.pool.QueryRow(ctx, `
		UPDATE channel_bindings b SET last_used_at = $4
		FROM notification_channels c, principals p
		WHERE c.id = b.channel_id AND p.id = b.principal_id AND `+liveBinding+`
			AND b.channel_id = $1 AND b.address = $2 AND b.actor_external_id = $3
		RETURNING b.id`, channel, address, actor, at).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Binding{}, fmt.Errorf("binding for a press: %w", ErrNotFound)
	}
	if err != nil {
		return Binding{}, fmt.Errorf("binding for a press: %w", err)
	}
	return db.BindingByID(ctx, id)
}

// RevokeBinding is `channel unbind`: it revokes active binding id and
// records a. ErrNotFound if it is not active.
func (db *DB) RevokeBinding(ctx context.Context, id string, a Action) error {
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE channel_bindings SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`, id, a.At)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return insertAction(ctx, tx, a)
	})
	if err != nil {
		return fmt.Errorf("revoke binding %s: %w", id, err)
	}
	return nil
}

// Subscribe adds (on) or removes (!on) events to binding id's
// subscriptions and records a. Subscribing is not a widening (§13.10):
// the destination was confirmed when it was bound, and mail.new is
// filtered by the binding's person's visibility. ErrNotFound if the
// binding is not active.
func (db *DB) Subscribe(ctx context.Context, id string, events []string, on bool, a Action) error {
	if err := checkEvents(events); err != nil {
		return err
	}
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		var one int
		err := tx.QueryRow(ctx, `SELECT 1 FROM channel_bindings WHERE id = $1 AND revoked_at IS NULL FOR UPDATE`, id).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if on {
			err = subscribe(ctx, tx, id, events)
		} else {
			_, err = tx.Exec(ctx, `DELETE FROM notification_subscriptions WHERE binding_id = $1 AND event = ANY($2)`, id, events)
		}
		if err != nil {
			return err
		}
		return insertAction(ctx, tx, a)
	})
	if err != nil {
		return fmt.Errorf("subscriptions of binding %s: %w", id, err)
	}
	return nil
}

func checkEvents(events []string) error {
	for _, e := range events {
		if !slices.Contains(Events, e) {
			return fmt.Errorf("event %q: want one of %v (security cannot be subscribed to: it always goes to administrators and the person it is about)", e, Events)
		}
	}
	return nil
}

func subscribe(ctx context.Context, tx pgx.Tx, id string, events []string) error {
	_, err := tx.Exec(ctx, `INSERT INTO notification_subscriptions (binding_id, event)
		SELECT $1, e FROM unnest($2::text[]) e ON CONFLICT DO NOTHING`, id, events)
	return err
}

// checkHuman locks principal id and checks that it is an enabled human.
func checkHuman(ctx context.Context, tx pgx.Tx, id string) (Principal, error) {
	p, err := scanPrincipal(tx.QueryRow(ctx, `SELECT `+principalCols+` FROM principals WHERE id = $1 FOR SHARE`, id))
	if errors.Is(err, ErrNotFound) {
		return p, ErrNotHuman
	}
	if err != nil {
		return p, err
	}
	if p.Kind != "human" || p.DisabledAt != nil {
		return p, ErrNotHuman
	}
	return p, nil
}

// lockEnabledChannel locks channel id against removal and checks it is
// enabled.
func lockEnabledChannel(ctx context.Context, tx pgx.Tx, id string) (Channel, error) {
	c, err := scanChannel(tx.QueryRow(ctx, `SELECT `+channelCols+` FROM notification_channels c
		WHERE c.id = $1 AND c.disabled_at IS NULL FOR SHARE`, id))
	if err != nil {
		return c, fmt.Errorf("enabled channel %s: %w", id, err)
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// Bind codes

// BindCode is a channel_bind_codes row. The code itself is never stored.
type BindCode struct {
	ID          string
	ChannelID   string
	PrincipalID string
	// CreatedWith is the session that asked for it; "" for the CLI.
	CreatedWith string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	// What Telegram reported with /start <code>; RedeemedAt nil until then.
	RedeemedAt   *time.Time
	SeenAddress  string
	SeenActorID  string
	SeenLabel    string
	SeenChatKind string
	// BindingID is set once confirmed.
	BindingID string
}

const bindCodeCols = `id, channel_id, principal_id, coalesce(created_with, ''), created_at, expires_at, redeemed_at,
	coalesce(seen_address, ''), coalesce(seen_actor_id, ''), coalesce(seen_label, ''), coalesce(seen_chat_kind, ''),
	coalesce(binding_id, '')`

func scanBindCode(row pgx.Row) (BindCode, error) {
	var c BindCode
	err := row.Scan(&c.ID, &c.ChannelID, &c.PrincipalID, &c.CreatedWith, &c.CreatedAt, &c.ExpiresAt, &c.RedeemedAt,
		&c.SeenAddress, &c.SeenActorID, &c.SeenLabel, &c.SeenChatKind, &c.BindingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// HashBindCode is what is stored of a code. The code is 16 random bytes,
// so a plain hash cannot be guessed backwards.
func HashBindCode(code string) []byte {
	h := sha256.Sum256([]byte(code))
	return h[:]
}

// NewBindCode stores a bind code for principal on channel, whose text the
// caller drew and shows once (code; only its hash is kept), and records a
// (§13.7: starting a binding is a widening). createdWith is the
// re-verified session that asked, "" for the CLI. Codes that ran out
// unconfirmed are dropped on the way. ErrNotHuman unless principal is an
// enabled human; ErrNotFound unless the channel is enabled.
func (db *DB) NewBindCode(ctx context.Context, channel, principal, createdWith, code string, a Action) (BindCode, error) {
	if len(code) < 16 {
		return BindCode{}, errors.New("bind code too short")
	}
	var out BindCode
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if _, err := lockEnabledChannel(ctx, tx, channel); err != nil {
			return err
		}
		if _, err := checkHuman(ctx, tx, principal); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM channel_bind_codes WHERE binding_id IS NULL AND expires_at < $1`, a.At); err != nil {
			return err
		}
		id := ulid.New(a.At)
		_, err := tx.Exec(ctx, `
			INSERT INTO channel_bind_codes (id, channel_id, principal_id, code_hash, created_with, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			id, channel, principal, HashBindCode(code), nullable(createdWith), a.At, a.At.Add(BindCodeTTL))
		if err != nil {
			return err
		}
		if err := insertAction(ctx, tx, a); err != nil {
			return err
		}
		out, err = scanBindCode(tx.QueryRow(ctx, `SELECT `+bindCodeCols+` FROM channel_bind_codes WHERE id = $1`, id))
		return err
	})
	if err != nil {
		return BindCode{}, fmt.Errorf("new bind code: %w", err)
	}
	return out, nil
}

// BindCodeByID returns bind code id.
func (db *DB) BindCodeByID(ctx context.Context, id string) (BindCode, error) {
	c, err := scanBindCode(db.pool.QueryRow(ctx, `SELECT `+bindCodeCols+` FROM channel_bind_codes WHERE id = $1`, id))
	if err != nil {
		return c, fmt.Errorf("bind code %s: %w", id, err)
	}
	return c, nil
}

// Seen is who sent /start <code>, as Telegram reported it.
type Seen struct {
	// Address is the chat; ActorID the user who sent it.
	Address string
	ActorID string
	// Label names them for the person who confirms, e.g. "Alice
	// (@alice)" or a group's title.
	Label string
	// ChatKind is Telegram's chat type: private, group, supergroup.
	ChatKind string
}

// RedeemBindCode records who sent code to the bot of channel (§13.7
// step 2). It grants nothing: the person the code is for still has to
// confirm this identity. A code is redeemed once, before it expires;
// everything else is ErrBindCode.
func (db *DB) RedeemBindCode(ctx context.Context, channel, code string, s Seen, at time.Time) (BindCode, error) {
	if s.Address == "" || s.ActorID == "" {
		return BindCode{}, errors.New("redeem bind code: chat and user are required")
	}
	c, err := scanBindCode(db.pool.QueryRow(ctx, `
		UPDATE channel_bind_codes SET redeemed_at = $4, seen_address = $5, seen_actor_id = $6, seen_label = $7, seen_chat_kind = $8
		WHERE channel_id = $1 AND code_hash = $2 AND redeemed_at IS NULL AND expires_at > $3
		RETURNING `+bindCodeCols,
		channel, HashBindCode(code), at, at, s.Address, s.ActorID, truncate(s.Label, 200), truncate(s.ChatKind, 32)))
	if errors.Is(err, ErrNotFound) {
		return c, ErrBindCode
	}
	if err != nil {
		return c, fmt.Errorf("redeem bind code: %w", err)
	}
	return c, nil
}

// truncate cuts s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// ConfirmBindCode is §13.7 step 3: the person the code is for has seen
// who redeemed it and agrees. It makes the binding (chat and button
// user as Telegram reported them), subscribes it to events, links the
// code to it and records a, with the binding id as its result unless a
// has one. The code must be redeemed (ErrNotRedeemed),
// unexpired and unconfirmed (ErrBindCode); its person still an enabled
// human (ErrNotHuman); its channel enabled (ErrNotFound); the chat not
// bound already (ErrDuplicate).
func (db *DB) ConfirmBindCode(ctx context.Context, id, label string, events []string, a Action) (Binding, error) {
	if err := checkEvents(events); err != nil {
		return Binding{}, err
	}
	var bindingID string
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		c, err := scanBindCode(tx.QueryRow(ctx, `SELECT `+bindCodeCols+` FROM channel_bind_codes WHERE id = $1 FOR UPDATE`, id))
		if err != nil {
			return err
		}
		switch {
		case c.BindingID != "" || !a.At.Before(c.ExpiresAt):
			return ErrBindCode
		case c.RedeemedAt == nil:
			return ErrNotRedeemed
		}
		if _, err := lockEnabledChannel(ctx, tx, c.ChannelID); err != nil {
			return err
		}
		if _, err := checkHuman(ctx, tx, c.PrincipalID); err != nil {
			return err
		}
		if label == "" {
			label = c.SeenLabel
		}
		bindingID, err = insertBinding(ctx, tx, c.ChannelID, c.PrincipalID, c.SeenAddress, c.SeenActorID, label, events, a.At)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE channel_bind_codes SET binding_id = $2 WHERE id = $1`, id, bindingID); err != nil {
			return err
		}
		if a.Result == nil {
			a.Result = map[string]any{"binding_id": bindingID}
		}
		return insertAction(ctx, tx, a)
	})
	if err != nil {
		return Binding{}, fmt.Errorf("confirm bind code %s: %w", id, err)
	}
	return db.BindingByID(ctx, bindingID)
}

// DropBindCode deletes unconfirmed bind code id: the person declined who
// redeemed it, or gave up waiting. Nothing to record: nothing was granted.
func (db *DB) DropBindCode(ctx context.Context, id string) error {
	if _, err := db.pool.Exec(ctx, `DELETE FROM channel_bind_codes WHERE id = $1 AND binding_id IS NULL`, id); err != nil {
		return fmt.Errorf("drop bind code %s: %w", id, err)
	}
	return nil
}

func insertBinding(ctx context.Context, tx pgx.Tx, channel, principal, address, actor, label string, events []string, at time.Time) (string, error) {
	if !telegramChat.MatchString(address) {
		return "", fmt.Errorf("chat %q is not a Telegram chat id", address)
	}
	if actor != "" && !telegramUser.MatchString(actor) {
		return "", fmt.Errorf("user %q is not a Telegram user id", actor)
	}
	id := ulid.New(at)
	_, err := tx.Exec(ctx, `
		INSERT INTO channel_bindings (id, channel_id, principal_id, address, actor_external_id, label, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, id, channel, principal, address, nullable(actor), truncate(label, 256), at)
	if err != nil {
		return "", uniqueAsDuplicate(err)
	}
	return id, subscribe(ctx, tx, id, events)
}

// ---------------------------------------------------------------------------
// Import from the environment (§17.7)

// ImportedTelegram is the Telegram configuration of the environment
// (YUUKA_TELEGRAM_*) as ImportTelegram writes it.
type ImportedTelegram struct {
	Channel ChannelConfig
	Token   secret.Record
	// Principal is the human the chat belongs to.
	Principal string
	Address   string
	// ActorID: the button user, "" without YUUKA_TELEGRAM_OPERATOR_ID.
	ActorID string
	Label   string
	Events  []string
}

// ImportTelegram writes the environment's bot as channel t.Channel with
// one binding of t.Principal, subscribed to t.Events, and records a
// (config.import, by system). It is the one binding made without the
// two-sided confirmation: the environment is the host's own (§17.7).
//
// It writes once: false, and nothing written, when a channel of that
// name exists (any state) or the bot is already an enabled channel.
// Processes that start together race on those unique keys, and all but
// one find the row there.
func (db *DB) ImportTelegram(ctx context.Context, t ImportedTelegram, a Action) (bool, error) {
	c, err := checkChannelConfig(t.Channel)
	if err != nil {
		return false, err
	}
	if err := checkChannelSecret(c, t.Token); err != nil {
		return false, fmt.Errorf("import channel %s: %w", c.Name, err)
	}
	if err := checkEvents(t.Events); err != nil {
		return false, err
	}
	settings, err := json.Marshal(c.Settings)
	if err != nil {
		return false, err
	}
	errExists := errors.New("exists")
	err = pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if _, err := checkHuman(ctx, tx, t.Principal); err != nil {
			return err
		}
		if err := insertSecret(ctx, tx, t.Token, a.At); err != nil {
			return err
		}
		id := ulid.New(a.At)
		tag, err := tx.Exec(ctx, `
			INSERT INTO notification_channels (id, name, kind, external_id, settings, secret_id, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING`,
			id, c.Name, c.Kind, c.ExternalID, settings, t.Token.ID, a.At)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errExists // rolls the secret back
		}
		if _, err := insertBinding(ctx, tx, id, t.Principal, t.Address, t.ActorID, t.Label, t.Events, a.At); err != nil {
			return err
		}
		return insertAction(ctx, tx, a)
	})
	if errors.Is(err, errExists) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("import channel %s: %w", c.Name, err)
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// Read paths of the pushes (§13.8): what a binding's person may be told.

// DeliveryVisible reports whether v may see delivery id, and with it the
// summary of new mail that names its prefix and sender (mail.new, §8.4).
func (db *DB) DeliveryVisible(ctx context.Context, v *auth.Viewer, id string) (bool, error) {
	if err := needViewer(v, "delivery visible"); err != nil {
		return false, err
	}
	var ok bool
	err := db.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM deliveries d WHERE d.id = $1 AND `+db.visibleDelivery(v, "d")+`)`,
		id).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("delivery %s visible: %w", id, err)
	}
	return ok, nil
}

// IdentityMailVisible reports whether v may see the mail sent as
// identity id: whether its account is readable, which is what decides
// the visibility of its outbox rows. A card about a grant on it
// (grant.suspended) follows this.
func (db *DB) IdentityMailVisible(ctx context.Context, v *auth.Viewer, id string) (bool, error) {
	if err := needViewer(v, "identity visible"); err != nil {
		return false, err
	}
	var ok bool
	err := db.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM identities si WHERE si.id = $1 AND `+db.readableAccount(v, "si.account_id")+`)`,
		id).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("identity %s visible: %w", id, err)
	}
	return ok, nil
}
