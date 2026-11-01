package r2fake_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/r2"
	"github.com/yuuka-mail/yuuka/internal/r2/r2fake"
)

func pullMeta() r2.PullMeta {
	return r2.PullMeta{
		Account: "personal", SourceKey: "loc:7:42:INBOX", Mailbox: "INBOX",
		UIDValidity: 7, UID: 42, InternalDate: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC),
	}
}

func TestPutPull(t *testing.T) {
	b := r2fake.New()
	ctx := t.Context()
	const pk = "pull/2026/10/01M67SX66B38NKRKAYDXR834N3.eml"
	meta := pullMeta()
	if err := b.PutPull(ctx, pk, []byte("one"), meta); err != nil {
		t.Fatal(err)
	}
	if err := b.PutPull(ctx, pk, []byte("one"), meta); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	other := meta
	other.Mailbox = "Archive"
	if err := b.PutPull(ctx, pk, []byte("two"), meta); err == nil {
		t.Fatal("different bytes accepted")
	}
	if err := b.PutPull(ctx, pk, []byte("one"), other); err == nil {
		t.Fatal("different metadata accepted")
	}
	o, err := b.Get(ctx, pk)
	if err != nil || string(o.Body) != "one" {
		t.Fatalf("Get = %q, %v", o.Body, err)
	}
	got, err := r2.DecodePullMeta(o.Metadata[r2.PullMetadataKey])
	if err != nil || got != meta {
		t.Fatalf("metadata = %+v, %v", got, err)
	}

	b.Put("raw/2026/10/01M67SX66B38NKRKAYDXR834N3.eml", []byte("inbound"), nil)
	for _, k := range []string{"raw/2026/10/01M67SX66B38NKRKAYDXR834N3.eml", "pending/01M67SX66B38NKRKAYDXR834N3", key, "pull/x"} {
		if err := b.PutPull(ctx, k, []byte("evil"), meta); err == nil {
			t.Errorf("PutPull(%q) accepted", k)
		}
	}
	if err := b.PutSent(ctx, pk, []byte("evil")); err == nil {
		t.Error("PutSent accepted a pull key")
	}
	if o, _ := b.Get(ctx, "raw/2026/10/01M67SX66B38NKRKAYDXR834N3.eml"); string(o.Body) != "inbound" {
		t.Fatalf("raw object changed: %q", o.Body)
	}
	if err := b.PutPull(ctx, "pull/2026/11/01M67SX66B38NKRKAYDXR834N3.eml", []byte("x"), r2.PullMeta{}); err == nil {
		t.Error("empty metadata accepted")
	}
}

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
