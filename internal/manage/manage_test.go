package manage

import (
	"errors"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/store"
)

func TestChainWidens(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		was, names []string
		widens     bool
	}{
		{nil, []string{"a"}, true},
		{[]string{"a"}, []string{"a"}, false},
		{[]string{"a", "b"}, []string{"a"}, false},                // a fallback taken away
		{[]string{"a", "b", "c"}, []string{"a", "c", "b"}, false}, // fallbacks reordered
		{[]string{"a"}, []string{"a", "b"}, true},                 // a fallback added
		{[]string{"a", "b"}, []string{"b", "a"}, true},            // another primary
		{[]string{"a", "b"}, []string{"b"}, true},                 // the fallback promoted
	} {
		if got := ChainWidens(c.was, c.names); got != c.widens {
			t.Errorf("%v -> %v: widens %v, want %v", c.was, c.names, got, c.widens)
		}
	}
}

func TestCheckPassword(t *testing.T) {
	t.Parallel()
	at := time.Now()
	configured := &store.Transport{
		Name: "work", Host: "smtp.example.com", Port: 465, Auth: store.TransportAuthPlain, Username: "u",
		SecretID: "s1", ConfiguredAt: &at,
	}
	plain := store.TransportConfig{Name: "work", Host: "smtp.example.com", Port: 465, Auth: store.TransportAuthPlain, Username: "u"}
	moved := plain
	moved.Host = "elsewhere.example.com"
	none := store.TransportConfig{Name: "work", Host: "smtp.example.com", Port: 465, Auth: store.TransportAuthNone}

	if err := CheckPassword(configured, plain, false); err != nil {
		t.Errorf("same destination, password kept: %v", err)
	}
	var pn *PasswordNeededError
	if err := CheckPassword(configured, moved, false); !errors.As(err, &pn) || !pn.Moved || pn.Destination != "u@elsewhere.example.com:465" {
		t.Errorf("moved without a password: %v", err)
	}
	if err := CheckPassword(nil, plain, false); !errors.As(err, &pn) || pn.Moved {
		t.Errorf("new without a password: %v", err)
	}
	if err := CheckPassword(nil, plain, true); err != nil {
		t.Errorf("new with a password: %v", err)
	}
	if err := CheckPassword(configured, none, true); !errors.Is(err, ErrPasswordUnused) {
		t.Errorf("a password without auth: %v", err)
	}
	if err := CheckPassword(configured, none, false); err != nil {
		t.Errorf("auth none: %v", err)
	}
}

func TestTokenBotID(t *testing.T) {
	t.Parallel()
	if got := TokenBotID("123456:AAAAAAAAAAAAAAAAAAAAAAAA"); got != 123456 {
		t.Errorf("got %d", got)
	}
	for _, bad := range []string{"", "123456", "abc:AAAAAAAAAAAAAAAAAAAAAAAA", "123:short"} {
		if got := TokenBotID(bad); got != 0 {
			t.Errorf("%q: %d", bad, got)
		}
	}
}

func TestBindLinks(t *testing.T) {
	t.Parallel()
	p, g := BindLinks("example_bot", "a-b_c")
	if p != "https://t.me/example_bot?start=a-b_c" || g != "https://t.me/example_bot?startgroup=a-b_c" {
		t.Errorf("%s %s", p, g)
	}
	if p, g := BindLinks("", "x"); p != "" || g != "" {
		t.Errorf("no bot name: %s %s", p, g)
	}
}
