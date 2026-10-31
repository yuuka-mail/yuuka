package r2fake_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/yuuka-mail/yuuka/internal/r2"
	"github.com/yuuka-mail/yuuka/internal/r2/r2fake"
)

const key = "sent/2026/10/01M67SX66B38NKRKAYDXR834N3.eml"

func TestPutSent(t *testing.T) {
	b := r2fake.New()
	ctx := t.Context()
	if err := b.PutSent(ctx, key, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := b.PutSent(ctx, key, []byte("two")); !errors.Is(err, r2.ErrExists) {
		t.Fatalf("second put: %v", err)
	}
	o, err := b.Get(ctx, key)
	if err != nil || string(o.Body) != "one" {
		t.Fatalf("Get = %q, %v", o.Body, err)
	}

	b.Put("raw/2026/10/01M67SX66B38NKRKAYDXR834N3.eml", []byte("inbound"), nil)
	for _, k := range []string{"raw/2026/10/01M67SX66B38NKRKAYDXR834N3.eml", "pending/01M67SX66B38NKRKAYDXR834N3", "sent/x"} {
		if err := b.PutSent(ctx, k, []byte("evil")); err == nil || errors.Is(err, r2.ErrExists) {
			t.Errorf("PutSent(%q): %v", k, err)
		}
	}
	if o, _ := b.Get(ctx, "raw/2026/10/01M67SX66B38NKRKAYDXR834N3.eml"); string(o.Body) != "inbound" {
		t.Fatalf("raw object changed: %q", o.Body)
	}
}

func TestPutSentConcurrent(t *testing.T) {
	b := r2fake.New()
	var wins, exists atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			switch err := b.PutSent(t.Context(), key, []byte("x")); {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, r2.ErrExists):
				exists.Add(1)
			default:
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 || exists.Load() != 7 {
		t.Fatalf("wins %d, exists %d", wins.Load(), exists.Load())
	}
}

func TestPutSentErr(t *testing.T) {
	b := r2fake.New()
	b.Err = errors.New("down")
	if err := b.PutSent(t.Context(), key, nil); !errors.Is(err, b.Err) {
		t.Fatalf("PutSent = %v", err)
	}
}
