package pull

import (
	"context"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/notify"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// The §10.1.2 benchmark, at a size the test database takes in seconds
// rather than tens of minutes: a first sync (the engine's cost per
// message, the content step left out, since it is ingest's and measured
// there) and an empty poll of a folder of 10^5 messages. Run with
//
//	go test -run '^$' -bench . -benchtime 1x ./internal/pull/
//
// Memory is bounded by construction rather than measured here: one
// body at a time, at most max_message_bytes, and one window of metadata.

// noContent skips the content step.
type noContent struct{}

func (noContent) FoldPulled(context.Context, store.RawRef, []byte, bool) (store.Folded, error) {
	return store.Folded{}, nil
}
func (noContent) Notify(context.Context, notify.Mail) {}

func BenchmarkFirstSync(b *testing.B) {
	const n = 5000
	for b.Loop() {
		b.StopTimer()
		srv, all := gmailServer(b)
		e := newEnv(b, srv)
		e.cfg.Content = noContent{}
		e.add("gmail", store.PresetGmail, nil)
		for i := range n {
			srv.add(all, fakeMsg{raw: letter(i, "bulk"), date: e.clock().Add(-time.Hour), gmID: uint64(1 + i)})
		}
		w := e.worker("gmail")
		b.StartTimer()
		start := time.Now()
		e.round(w, false)
		b.ReportMetric(float64(time.Since(start).Microseconds())/n, "µs/message")
		if c := e.count(`SELECT count(*) FROM deliveries`); c != n {
			b.Fatalf("%d deliveries", c)
		}
	}
}

func BenchmarkEmptyPoll(b *testing.B) {
	const n = 100_000
	srv, all := gmailServer(b)
	e := newEnv(b, srv)
	src := e.add("gmail", store.PresetGmail, nil)
	srv.set(func() {
		for i := range n {
			all.msgs = append(all.msgs, &fakeMsg{uid: uint32(i + 1), gmID: uint64(i + 1)}) //nolint:gosec // n fits
		}
		all.uidnext = n + 1
	})
	ctx := b.Context()
	if _, err := e.db.StartMailbox(ctx, src.AccountID, store.MailboxStart{Name: "[Gmail]/All Mail", KeyKind: store.KeyGmail, UIDValidity: 11, High: n}); err != nil {
		b.Fatal(err)
	}
	if _, err := e.db.AdvanceLow(ctx, src.AccountID, "[Gmail]/All Mail", 11, 1, 0); err != nil {
		b.Fatal(err)
	}
	if _, err := e.db.RecordMailboxStatus(ctx, src.AccountID, "[Gmail]/All Mail", 11, n+1, n, e.clock()); err != nil {
		b.Fatal(err)
	}
	w := e.worker("gmail")
	w.reconciled["[Gmail]/All Mail"] = e.clock()
	for b.Loop() {
		e.round(w, false)
	}
	if srv.count(func() int { return srv.examines }) != 0 {
		b.Fatal("an unchanged folder was opened")
	}
}
