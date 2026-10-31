package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Telegram talks to a bot through the Bot API: sendMessage for
// notifications and cards, and, for the recall button of held mail and
// /start <code>, getUpdates (long polling, Poller) with the calls that
// answer it.
type Telegram struct {
	Token  string
	ChatID string
	// BaseURL defaults to https://api.telegram.org; tests override it.
	BaseURL string
	Client  *http.Client
}

// To returns a copy of t that sends to chat.
func (t *Telegram) To(chat string) *Telegram {
	c := *t
	c.ChatID = chat
	return &c
}

// Button is one inline keyboard button: a callback (Data) or a link
// (URL).
type Button struct {
	Text string `json:"text"`
	Data string `json:"callback_data,omitempty"`
	URL  string `json:"url,omitempty"`
}

// Mail is new mail to tell a person about (mail.new, §8.4): the summary,
// and what a dispatcher needs to decide who may be told.
type Mail struct {
	MessageID string
	// DeliveryID is the delivery that arrived. Its visibility decides
	// who is told (§13.8): the summary names its prefix.
	DeliveryID string
	Text       string
	// Silent: a rule asked for this notification to make no sound.
	Silent bool
}

// NewMail sends m's summary to the chat.
func (t *Telegram) NewMail(ctx context.Context, m Mail) error { return t.send(ctx, m.Text, m.Silent) }

// Notify sends text as a plain message (no parse_mode, so header content
// can never be interpreted as markup).
func (t *Telegram) Notify(ctx context.Context, text string) error {
	return t.send(ctx, text, false)
}

// NotifySilently is Notify without sound on the recipient's device
// (disable_notification), for mail a rule asked to notify silently.
func (t *Telegram) NotifySilently(ctx context.Context, text string) error {
	return t.send(ctx, text, true)
}

func (t *Telegram) send(ctx context.Context, text string, silent bool) error {
	return t.call(ctx, "sendMessage", map[string]any{
		"chat_id":                  t.ChatID,
		"text":                     text,
		"disable_web_page_preview": true,
		"disable_notification":     silent,
	}, nil)
}

// SendCard sends text, plain like Notify, with an inline keyboard of
// rows (none when empty), and returns the message id.
func (t *Telegram) SendCard(ctx context.Context, text string, rows [][]Button) (int64, error) {
	body := map[string]any{"chat_id": t.ChatID, "text": text, "disable_web_page_preview": true}
	if len(rows) > 0 {
		body["reply_markup"] = map[string]any{"inline_keyboard": rows}
	}
	var msg struct {
		MessageID int64 `json:"message_id"`
	}
	err := t.call(ctx, "sendMessage", body, &msg)
	return msg.MessageID, err
}

// RemoveKeyboard takes the buttons off message id in chat.
func (t *Telegram) RemoveKeyboard(ctx context.Context, chat, id int64) error {
	return t.call(ctx, "editMessageReplyMarkup", map[string]any{
		"chat_id": chat, "message_id": id, "reply_markup": map[string]any{"inline_keyboard": [][]Button{}},
	}, nil)
}

// Answer answers callback query id with a short notice.
func (t *Telegram) Answer(ctx context.Context, id, text string) error {
	return t.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
}

// Reply sends text to chat, outside of t's own chat: the answer to a
// /start.
func (t *Telegram) Reply(ctx context.Context, chat int64, text string) error {
	return t.call(ctx, "sendMessage", map[string]any{"chat_id": chat, "text": text}, nil)
}

// ErrConflict: getUpdates refused because the bot has a webhook set, or
// another process is polling it. Telegram delivers each update to one
// of them only.
var ErrConflict = errors.New("telegram: conflict: the bot has a webhook, or another process polls it")

// Update is the part of a Bot API update the poller reads.
type Update struct {
	ID       int64          `json:"update_id"`
	Callback *CallbackQuery `json:"callback_query"`
	Message  *Message       `json:"message"`
}

// CallbackQuery is a press of an inline button.
type CallbackQuery struct {
	ID   string `json:"id"`
	From struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Message *struct {
		ID   int64 `json:"message_id"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
	Data string `json:"data"`
}

// User is a Telegram user as an update reports them.
type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

// Chat is the chat of a message.
type Chat struct {
	ID int64 `json:"id"`
	// Type is private, group, supergroup or channel.
	Type  string `json:"type"`
	Title string `json:"title"`
}

// Message is the part of a message the poller reads: /start <code>.
type Message struct {
	ID   int64  `json:"message_id"`
	From *User  `json:"from"`
	Chat Chat   `json:"chat"`
	Text string `json:"text"`
}

// Bot is who a token belongs to, as getMe reports it.
type Bot struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// GetMe asks Telegram who the token belongs to: how a channel learns its
// bot, whose id is the token's audience (§17.2).
func (t *Telegram) GetMe(ctx context.Context) (Bot, error) {
	var b Bot
	err := t.call(ctx, "getMe", map[string]any{}, &b)
	return b, err
}

// Updates long-polls for the updates of the kinds in allowed
// ("callback_query", "message") after offset, waiting up to timeout
// seconds.
func (t *Telegram) Updates(ctx context.Context, offset int64, timeout int, allowed []string) ([]Update, error) {
	var out []Update
	err := t.call(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": timeout, "allowed_updates": allowed,
	}, &out)
	return out, err
}

// call posts body to method and decodes the result into out (nil to
// ignore it). Errors never carry the URL, which holds the token.
func (t *Telegram) call(ctx context.Context, method string, body, out any) error {
	base := t.BaseURL
	if base == "" {
		base = "https://api.telegram.org"
	}
	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("telegram: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+t.Token+"/"+method, bytes.NewReader(data))
	if err != nil {
		return errors.New("telegram: cannot build request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		// *url.Error embeds the URL, and the URL embeds the bot token.
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}
		return fmt.Errorf("telegram: %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var r struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusConflict {
		return ErrConflict
	}
	if err := json.Unmarshal(raw, &r); err != nil || !r.OK {
		return fmt.Errorf("telegram: %s: status %d: %s", method, resp.StatusCode, r.Description)
	}
	if out != nil && len(r.Result) > 0 {
		if err := json.Unmarshal(r.Result, out); err != nil {
			return fmt.Errorf("telegram: %s: result: %w", method, err)
		}
	}
	return nil
}
