package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSummary(t *testing.T) {
	cases := []struct {
		name, raw, want string
	}{
		{
			"plain",
			"From: GitHub <noreply@github.example>\r\nSubject: Your code\r\n\r\nbody",
			"[example.com] github@ ← GitHub <noreply@github.example>: Your code",
		},
		{
			"encoded words",
			"From: =?utf-8?B?5byg5LiJ?= <z@example.net>\nSubject: =?gb2312?B?suLK1A==?= ok\n\n",
			"[example.com] github@ ← 张三 <z@example.net>: 测试 ok",
		},
		{
			"bare address and folded subject",
			"From: a@example.net\nSubject: one\n\ttwo\n\n",
			"[example.com] github@ ← a@example.net: one two",
		},
		{
			"control characters",
			"From: x@example.net\nSubject: a\x07b\n\n",
			"[example.com] github@ ← x@example.net: a b",
		},
		{
			"unparsable",
			"not a message",
			"[example.com] github@ ← ?: ",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Summary("example.com", "github", []byte(tc.raw)); got != tc.want {
				t.Errorf("Summary =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

func TestSummaryTruncates(t *testing.T) {
	got := Summary("example.com", "a", []byte("From: x@example.net\nSubject: "+strings.Repeat("z", 1000)+"\n\n"))
	if n := len([]rune(got)); n > 300 {
		t.Fatalf("summary is %d runes", n)
	}
}

func TestTelegram(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botTOKEN/sendMessage" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"ok":false,"description":"Not Found"}`))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	tg := &Telegram{Token: "TOKEN", ChatID: "42", BaseURL: srv.URL}
	if err := tg.Notify(t.Context(), "hello"); err != nil {
		t.Fatal(err)
	}
	if got["chat_id"] != "42" || got["text"] != "hello" || got["parse_mode"] != nil || got["disable_notification"] != false {
		t.Fatalf("sent %v", got)
	}
	if err := tg.NotifySilently(t.Context(), "quiet"); err != nil || got["disable_notification"] != true {
		t.Fatalf("silently: sent %v, %v", got, err)
	}

	bad := &Telegram{Token: "WRONG", ChatID: "42", BaseURL: srv.URL}
	if err := bad.Notify(t.Context(), "x"); err == nil {
		t.Fatal("API error not reported")
	}
	srv.Close()
	err := tg.Notify(t.Context(), "x")
	if err == nil || strings.Contains(err.Error(), "TOKEN") {
		t.Fatalf("transport error leaks token or is nil: %v", err)
	}
}
