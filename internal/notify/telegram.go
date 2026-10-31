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

// Telegram sends messages through a bot with sendMessage.
type Telegram struct {
	Token  string
	ChatID string
	// BaseURL defaults to https://api.telegram.org; tests override it.
	BaseURL string
	Client  *http.Client
}

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
	base := t.BaseURL
	if base == "" {
		base = "https://api.telegram.org"
	}
	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}
	body, err := json.Marshal(map[string]any{
		"chat_id":                  t.ChatID,
		"text":                     text,
		"disable_web_page_preview": true,
		"disable_notification":     silent,
	})
	if err != nil {
		return fmt.Errorf("telegram: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+t.Token+"/sendMessage", bytes.NewReader(body))
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
		return fmt.Errorf("telegram: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var r struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err := json.Unmarshal(data, &r); err != nil || !r.OK {
		return fmt.Errorf("telegram: status %d: %s", resp.StatusCode, r.Description)
	}
	return nil
}
