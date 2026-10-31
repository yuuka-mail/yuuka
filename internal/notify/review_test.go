package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBot is the Bot API as the review tier uses it: it records what is
// sent and hands out queued updates once.
type fakeBot struct {
	mu       sync.Mutex
	calls    map[string][]map[string]any
	updates  []Update
	conflict bool
}

func (f *fakeBot) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
	if !strings.HasPrefix(r.URL.Path, "/botTOKEN/") {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Not Found"}`))
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string][]map[string]any{}
	}
	f.calls[method] = append(f.calls[method], body)
	var result any = true
	switch method {
	case "sendMessage":
		result = map[string]any{"message_id": 77}
	case "getUpdates":
		if f.conflict {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"ok":false,"description":"Conflict: can't use getUpdates method while webhook is active"}`))
			return
		}
		result, f.updates = f.updates, nil
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func (f *fakeBot) got(method string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

func press(update int64, from, chat int64, data string) Update {
	c := &CallbackQuery{ID: "cb", Data: data}
	c.From.ID = from
	c.Message = &struct {
		ID   int64 `json:"message_id"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	}{ID: 77}
	c.Message.Chat.ID = chat
	return Update{ID: update, Callback: c}
}

func TestHeldCard(t *testing.T) {
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	text := HeldText(HeldCard{
		OutboxID: "01JB0000000000000000000000", From: "arisu@example.com", To: []string{"bob@example.net"},
		Bcc: []string{"x@example.org"}, Submitter: "agent:arisu", ThreadSubject: "Plans\r\nInjected: header",
		Subject: "Re: Plans", Attachments: []Attachment{{Name: "a.txt", Type: "text/plain", Size: 2048}},
		Excerpt: "line one\n\n\x07line two\nline three\nline four", Passed: []string{"first_contact", "readable"},
		Flags: []string{"links to 1 domain(s) never seen\r\nInjected: line"}, SendAt: now.Add(10 * time.Minute), Now: now,
	}, time.UTC)
	for _, want := range []string{
		"yuuka · held for review", "arisu@example.com → bob@example.net; bcc x@example.org", "submitted by agent:arisu",
		"in reply to: Plans Injected: header", "attachment: a.txt (text/plain, 2 KiB)", "line one\nline two\nline three\n",
		"passed: first_contact, readable", "note: links to 1 domain(s) never seen Injected: line",
		"goes out at 09:10 (in 10 min) unless recalled",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("card lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "line four") || strings.ContainsRune(text, '\x07') {
		t.Errorf("card:\n%s", text)
	}
	long := strings.Repeat("x", 150) + "\n" + strings.Repeat("y", 150) + "\n" + strings.Repeat("z", 150)
	if ex := excerpt(long); strings.Contains(ex, "z") || len([]rune(ex)) > excerptChars+excerptLines {
		t.Errorf("excerpt %q", ex)
	}
}

func TestCards(t *testing.T) {
	bot := &fakeBot{}
	srv := httptest.NewServer(bot)
	defer srv.Close()
	tg := &Telegram{Token: "TOKEN", ChatID: "42", BaseURL: srv.URL}
	ctx := t.Context()

	c := &Cards{TG: tg, Origin: "https://mail.example.com/", Recall: true}
	if err := c.Held(ctx, HeldCard{OutboxID: "01JB0000000000000000000000", Subject: "s"}); err != nil {
		t.Fatal(err)
	}
	sent := bot.got("sendMessage")[0]
	kb, _ := json.Marshal(sent["reply_markup"])
	if sent["parse_mode"] != nil || !strings.Contains(string(kb), `"callback_data":"w:01JB0000000000000000000000"`) ||
		!strings.Contains(string(kb), `https://mail.example.com/?area=outbox\u0026o=01JB0000000000000000000000`) {
		t.Errorf("held card %v", sent)
	}
	// Without the poller there is no recall button.
	c.Recall = false
	if err := c.Held(ctx, HeldCard{OutboxID: "01JB0000000000000000000000"}); err != nil {
		t.Fatal(err)
	}
	if kb, _ := json.Marshal(bot.got("sendMessage")[1]["reply_markup"]); strings.Contains(string(kb), "callback_data") {
		t.Errorf("recall button without a poller: %s", kb)
	}
	if err := c.Escalated(ctx, EscalatedCard{DraftID: "D", Reasons: []string{"first contact with carol@example.org"}, Flags: []string{"cites another thread"}}); err != nil {
		t.Fatal(err)
	}
	if text := bot.got("sendMessage")[2]["text"].(string); !strings.Contains(text, "· first contact with carol@example.org") || !strings.Contains(text, "note: cites another thread") ||
		!strings.Contains(text, "Nothing was sent") {
		t.Errorf("escalated card:\n%s", text)
	}
}

// The poller acts only on the operator's presses in the operator's chat
// on its own buttons; the answer and the keyboard follow the outcome.
func TestPoller(t *testing.T) {
	const operator, chat = 1001, 42
	id := "01JB0000000000000000000000"
	bot := &fakeBot{updates: []Update{
		press(1, 666, chat, "w:"+id),          // someone else
		press(2, operator, 43, "w:"+id),       // another chat
		press(3, operator, chat, "s:"+id),     // not a recall
		press(4, operator, chat, "w:nope"),    // not an id
		press(5, operator, chat, "w:"+id),     // recalled, breaker trips
		press(6, operator, chat, "w:"+id),     // replayed
		{ID: 7},                               // no callback
		press(8, operator, chat, "w:"+id+"x"), // trailing garbage
	}}
	srv := httptest.NewServer(bot)
	defer srv.Close()
	tg := &Telegram{Token: "TOKEN", ChatID: "42", BaseURL: srv.URL}

	var recalls []string
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p := &Poller{
		TG: tg, OperatorID: operator, ChatID: chat, Cards: &Cards{TG: tg}, Timeout: 1,
		Recall: func(_ context.Context, got string, from int64) (string, bool, *SuspendedCard, error) {
			recalls = append(recalls, got)
			if from != operator {
				t.Errorf("recall from %d", from)
			}
			if len(recalls) == 1 {
				return "Recalled.", true, &SuspendedCard{Identity: "arisu@example.com", Principal: "agent:arisu", Recalls: 2}, nil
			}
			return "Already recalled.", true, nil, nil
		},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(bot.got("getUpdates")) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if len(recalls) != 2 || recalls[0] != id {
		t.Errorf("recalls %q", recalls)
	}
	answers := bot.got("answerCallbackQuery")
	if len(answers) != 5 || answers[0]["text"] != "Unknown action." || answers[2]["text"] != "Recalled." ||
		answers[3]["text"] != "Already recalled." || answers[4]["text"] != "Unknown action." {
		t.Errorf("answers %v", answers)
	}
	if edits := bot.got("editMessageReplyMarkup"); len(edits) != 2 || edits[0]["chat_id"] != float64(chat) || edits[0]["message_id"] != float64(77) {
		t.Errorf("keyboard edits %v", edits)
	}
	if sent := bot.got("sendMessage"); len(sent) != 1 || !strings.Contains(sent[0]["text"].(string), "grant suspended") {
		t.Errorf("suspended card %v", sent)
	}
	if polls := bot.got("getUpdates"); polls[1]["offset"] != float64(9) {
		t.Errorf("offset after the batch: %v", polls[1]["offset"])
	}
}

func TestPollerConflict(t *testing.T) {
	bot := &fakeBot{conflict: true}
	srv := httptest.NewServer(bot)
	defer srv.Close()
	tg := &Telegram{Token: "TOKEN", ChatID: "42", BaseURL: srv.URL}
	if _, err := tg.Updates(t.Context(), 0, 1); !errors.Is(err, ErrConflict) {
		t.Errorf("409: %v", err)
	}
}
