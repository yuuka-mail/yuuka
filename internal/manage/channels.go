package manage

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/yuuka-mail/yuuka/internal/notify"
	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Channels and bindings (§17, §8.4, §13.7). Adding a channel, giving it
// a new token and binding a chat widen; removing a channel, revoking a
// binding and changing what a binding receives do not (§13.10).

// botToken is the shape of a Bot API token: the bot's id, a colon, the
// secret part.
var botToken = regexp.MustCompile(`^([1-9][0-9]{0,18}):[A-Za-z0-9_-]{20,}$`)

// TokenBotID is the bot id a token starts with, 0 if it is not a token.
func TokenBotID(token string) int64 {
	m := botToken.FindStringSubmatch(token)
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	return n
}

// ErrNotToken: what was given does not look like a Bot API token.
var ErrNotToken = errors.New("the token does not look like a Bot API token, <bot id>:<secret>")

// AskError: Telegram could not be asked who a token belongs to.
type AskError struct{ Err error }

func (e *AskError) Error() string {
	return "asking Telegram who the token belongs to: " + e.Err.Error()
}
func (e *AskError) Unwrap() error { return e.Err }

// BotMismatchError: Telegram says the token belongs to another bot than
// the one its text names.
type BotMismatchError struct{ Says, Named int64 }

func (e *BotMismatchError) Error() string {
	return fmt.Sprintf("telegram says the token belongs to bot %d, not %d", e.Says, e.Named)
}

// GetMe asks Telegram who token belongs to (notify.Telegram.GetMe).
type GetMe func(ctx context.Context, token string) (notify.Bot, error)

// ChannelStore is what configuring channels needs.
type ChannelStore interface {
	ConfigureChannel(ctx context.Context, c store.ChannelConfig, revision int64, sealed *secret.Record, a store.Action) (store.Channel, error)
}

// AddChannel gives channel name the bot token token at revision, the
// one the caller read (0: a new name): a new channel, a removed one
// enabled again, or a new token for the same bot. Telegram, not the
// token's text, says which bot it is; the token is sealed for that bot
// (§17.2) and the action, channel.add, records the bot and the sealed
// secret's metadata, never the token.
//
// Errors: ErrLocked (checked before anything is sent to Telegram),
// ErrNotToken, *AskError, *BotMismatchError, *Invalid, and the store's
// (ErrStale, ErrDuplicate, ErrChannelBot, ErrNotFound).
func AddChannel(ctx context.Context, st ChannelStore, sealer *secret.Sealer, getMe GetMe,
	name, token string, revision int64, act Act, now time.Time,
) (store.Channel, notify.Bot, error) {
	if sealer == nil {
		return store.Channel{}, notify.Bot{}, ErrLocked
	}
	if !store.ValidName(name) {
		return store.Channel{}, notify.Bot{}, &Invalid{Reason: fmt.Sprintf("channel name %q: want [A-Za-z0-9][A-Za-z0-9._-]*, at most 64", name)}
	}
	botID := TokenBotID(token)
	if botID == 0 {
		return store.Channel{}, notify.Bot{}, ErrNotToken
	}
	bot, err := getMe(ctx, token)
	if err != nil {
		return store.Channel{}, bot, &AskError{Err: err}
	}
	if bot.ID != botID {
		return store.Channel{}, bot, &BotMismatchError{Says: bot.ID, Named: botID}
	}
	aud, err := secret.TelegramBot(bot.ID)
	if err != nil {
		return store.Channel{}, bot, err
	}
	sealed, err := sealer.Seal(ctx, ulid.New(now), store.SecretTelegramBotToken, aud, []byte(token))
	if err != nil {
		return store.Channel{}, bot, err
	}
	external := strconv.FormatInt(bot.ID, 10)
	c, err := st.ConfigureChannel(ctx, store.ChannelConfig{
		Name: name, Kind: store.ChannelTelegram, ExternalID: external,
		Settings: map[string]string{"username": bot.Username},
	}, revision, &sealed, act("channel.add", map[string]any{
		"name": name, "kind": store.ChannelTelegram, "external_id": external, "username": bot.Username,
		"audience": aud, "secret_id": sealed.ID, "wrapper": sealed.Wrapper, "key_ref": sealed.KeyRef,
	}))
	return c, bot, err
}

// DisableChannel removes channel c, as read, at its revision: disabled,
// its token erased, its bindings revoked (store.DisableChannel). A
// tightening, recorded as channel.rm.
func DisableChannel(ctx context.Context, st interface {
	DisableChannel(ctx context.Context, name string, revision int64, a store.Action) error
}, c store.Channel, act Act,
) error {
	return st.DisableChannel(ctx, c.Name, c.Revision, act("channel.rm", map[string]any{"name": c.Name, "id": c.ID}))
}

// NewBindCode draws the text of a bind code: 16 random bytes, so its
// stored hash cannot be guessed backwards (§17.4).
func NewBindCode() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// BindLinks are the links that send code to the bot username: into a
// private chat, and into a group. Empty when the bot's name is unknown;
// then the code is sent as "/start <code>".
func BindLinks(username, code string) (private, group string) {
	if username == "" {
		return "", ""
	}
	q := url.QueryEscape(code)
	return "https://t.me/" + username + "?start=" + q, "https://t.me/" + username + "?startgroup=" + q
}

// BindBeginArgs are what starting a binding records (channel.bind.begin):
// never the code.
func BindBeginArgs(channel string, p store.Principal) map[string]any {
	return map[string]any{"channel": channel, "principal": p.Kind + ":" + p.Name}
}

// BindConfirmArgs are what confirming a binding records
// (channel.bind.confirm): the Telegram identity as it was reported,
// never the code.
func BindConfirmArgs(channel string, p store.Principal, bc store.BindCode, events []string) map[string]any {
	return map[string]any{
		"channel": channel, "principal": p.Kind + ":" + p.Name, "chat": bc.SeenAddress, "button_user": bc.SeenActorID,
		"chat_kind": bc.SeenChatKind, "events": events, "code_id": bc.ID,
	}
}
