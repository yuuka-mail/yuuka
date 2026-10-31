package store_test

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/classify"
	"github.com/yuuka-mail/yuuka/internal/contenthash"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

var (
	operator = &auth.Viewer{PrincipalKind: "human", PrincipalName: "operator"}
	// shipped turns the OTP mail into a different message.
	shipped = strings.NewReplacer("Subject: Your verification code is 482913", "Subject: Your order has shipped")
)

// deliver commits and folds one more copy of a corpus file to prefix at
// example.com, as a second recipient would. The replacer may change the
// content, making it a message of its own.
func deliver(t *testing.T, db *store.DB, file string, edit *strings.Replacer, prefix string, at time.Time) string {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/eml/" + file)
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		raw = []byte(edit.Replace(string(raw)))
	}
	id := ulid.New(at)
	key := fmt.Sprintf("raw/2026/10/%s.eml", id)
	in := store.Ingest{
		RawID: id, R2Key: key, LocalPath: key, Size: int64(len(raw)), SHA256: sha256.Sum256(append([]byte(prefix), raw...)),
		ContentHash: contenthash.V1(raw), ReceivedAt: at, DeliveryID: id, Account: "cf",
		Zone: "example.com", Prefix: prefix, EnvelopeTo: prefix + "@example.com",
	}
	if _, err := db.CommitIngest(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	f, err := db.Fold(t.Context(), store.RawRef{
		DeliveryID: id, RawID: id, R2Key: key, LocalPath: key, ContentHash: in.ContentHash[:],
		Zone: "example.com", Prefix: prefix, ReceivedAt: at,
	}, mimeparse.Parse(raw), authres.Extract(raw, "mx.cloudflare.net"))
	if err != nil {
		t.Fatal(err)
	}
	return f.MessageID
}

func subjects(ms []store.MessageSummary) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Subject
	}
	return out
}

func TestListMessages(t *testing.T) {
	t.Parallel()
	db, _ := load(t)
	// A second copy of the OTP mail to an unclaimed prefix: same message,
	// two recipients; and a message only that prefix got.
	otp := deliver(t, db, "plain-otp.eml", nil, "shop", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	deliver(t, db, "plain-otp.eml", shipped, "shop", time.Date(2026, 11, 1, 0, 0, 1, 0, time.UTC))

	all, err := db.ListMessages(t.Context(), operator, store.MessageQuery{MessageFilter: store.MessageFilter{Downgraded: store.IncludeDowngraded}})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 20 {
		t.Fatalf("got %d messages, want 20 (the corpus, folded, and one more)", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].ReceivedAt.After(all[i-1].ReceivedAt) {
			t.Fatal("not newest first")
		}
	}
	i := slices.IndexFunc(all, func(m store.MessageSummary) bool { return m.ID == otp })
	if got := all[i].Recipients; !slices.Equal(got, []string{"a@example.com", "shop@example.com"}) {
		t.Errorf("recipients %q", got)
	}

	// Keyset paging walks the same list.
	var paged []store.MessageSummary
	var cur *store.MessageCursor
	for {
		page, err := db.ListMessages(t.Context(), operator, store.MessageQuery{
			MessageFilter: store.MessageFilter{Downgraded: store.IncludeDowngraded}, Before: cur, Limit: 5,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		paged = append(paged, page...)
		last := page[len(page)-1]
		cur = &store.MessageCursor{ReceivedAt: last.ReceivedAt, ID: last.ID}
	}
	if !slices.Equal(subjects(paged), subjects(all)) {
		t.Errorf("paging: %q", subjects(paged))
	}

	// shop is discovered, so what only it received is downgraded; the
	// OTP escapes through its claimed delivery.
	shop, err := db.ListMessages(t.Context(), operator, store.MessageQuery{MessageFilter: store.MessageFilter{
		Zone: "example.com", Prefix: "shop", Downgraded: store.IncludeDowngraded,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := subjects(shop); !slices.Equal(got, []string{"Your order has shipped", "Your verification code is 482913"}) {
		t.Errorf("shop: %q", got)
	}
	if !shop[0].Downgraded || shop[1].Downgraded {
		t.Errorf("downgrade: %v %v", shop[0].Downgraded, shop[1].Downgraded)
	}
	hidden, _ := db.ListMessages(t.Context(), operator, store.MessageQuery{MessageFilter: store.MessageFilter{Zone: "example.com", Prefix: "shop"}})
	if len(hidden) != 1 {
		t.Errorf("default hides downgraded: %q", subjects(hidden))
	}
	only, _ := db.ListMessages(t.Context(), operator, store.MessageQuery{MessageFilter: store.MessageFilter{Downgraded: store.OnlyDowngraded}})
	for _, m := range only {
		if !m.Downgraded {
			t.Errorf("only downgraded returned %q", m.Subject)
		}
	}

	human := classify.Human
	hs, _ := db.ListMessages(t.Context(), operator, store.MessageQuery{MessageFilter: store.MessageFilter{Class: &human, Downgraded: store.IncludeDowngraded}})
	for _, m := range hs {
		if m.Class != classify.Human {
			t.Errorf("class filter returned %q (%v)", m.Subject, m.Class)
		}
	}
	if len(hs) == 0 {
		t.Error("no human mail in the corpus?")
	}

	if _, err := db.ListMessages(t.Context(), operator, store.MessageQuery{MessageFilter: store.MessageFilter{Prefix: "a"}}); err == nil {
		t.Error("prefix without zone accepted")
	}
}

func TestMessageByID(t *testing.T) {
	t.Parallel()
	db, pool := load(t)
	all, err := db.ListMessages(t.Context(), operator, store.MessageQuery{MessageFilter: store.MessageFilter{Downgraded: store.IncludeDowngraded}})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(all, func(m store.MessageSummary) bool { return m.Subject == "Re: Lunch on Friday?" })
	d, err := db.MessageByID(t.Context(), operator, all[i].ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.InReplyTo != "orig-1@example.com" || d.BodyText == "" || d.Date == nil || len(d.Deliveries) != 1 {
		t.Fatalf("detail %+v", d)
	}
	if dl := d.Deliveries[0]; dl.Prefix != "a" || dl.PrefixState != store.PrefixClaimed || dl.Auth == nil ||
		dl.EnvelopeTo != "a@example.com" || dl.Raw.R2Key == "" || dl.Raw.DeliveryID != dl.ID {
		t.Errorf("delivery %+v", dl)
	}
	// The trusted receiver's verdict comes through as stored.
	i = slices.IndexFunc(all, func(m store.MessageSummary) bool { return m.Subject == "Your verification code is 482913" })
	otp, err := db.MessageByID(t.Context(), operator, all[i].ID)
	if err != nil {
		t.Fatal(err)
	}
	if a := otp.Deliveries[0].Auth; a == nil || !a.Trusted || a.DMARC != "pass" {
		t.Errorf("otp auth %+v", a)
	}

	// An alias resolves to its survivor.
	alias := ulid.New(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	if _, err := pool.Exec(t.Context(), `INSERT INTO message_aliases (old_id, new_id) VALUES ($1, $2)`, alias, d.ID); err != nil {
		t.Fatal(err)
	}
	if a, err := db.MessageByID(t.Context(), operator, alias); err != nil || a.ID != d.ID {
		t.Errorf("alias: %v %v", a.ID, err)
	}

	if _, err := db.MessageByID(t.Context(), operator, ulid.New(time.Now())); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing message: %v", err)
	}

	th, err := db.ThreadByID(t.Context(), operator, d.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(th.Messages) == 0 || th.Messages[0].ThreadID != d.ThreadID || len(th.Participants) == 0 {
		t.Errorf("thread %+v", th)
	}
	if _, err := db.ThreadByID(t.Context(), operator, ulid.New(time.Now())); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing thread: %v", err)
	}
}

func TestPrefixStats(t *testing.T) {
	t.Parallel()
	db, _ := load(t)
	deliver(t, db, "plain-otp.eml", nil, "shop", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	deliver(t, db, "plain-otp.eml", shipped, "shop", time.Date(2026, 11, 1, 0, 0, 1, 0, time.UTC))
	sum, err := db.PrefixStats(t.Context(), operator, store.PrefixQuery{})
	if err != nil {
		t.Fatal(err)
	}
	// The OTP went to two prefixes and counts once.
	if sum.Messages != 20 || sum.Downgraded != 4 {
		t.Errorf("totals %d/%d, want 20/4", sum.Messages, sum.Downgraded)
	}
	ps := sum.Prefixes
	got := map[string]store.PrefixStat{}
	for _, p := range ps {
		got[p.Prefix] = p
	}
	// a: the whole corpus; three of it fail DMARC.
	if a := got["a"]; a.State != store.PrefixClaimed || a.Messages != 19 || a.Downgraded != 3 {
		t.Errorf("a: %+v", a)
	}
	// shop: the OTP escapes via a, what only shop got is downgraded.
	if s := got["shop"]; s.State != store.PrefixDiscovered || s.Messages != 2 || s.Downgraded != 1 {
		t.Errorf("shop: %+v", s)
	}
	if ps[0].Prefix != "shop" || sum.Total != 2 || len(ps) != 2 {
		t.Errorf("most recent first: %q of %d", ps[0].Prefix, sum.Total)
	}
}

func TestPrefixStatsQuery(t *testing.T) {
	t.Parallel()
	db, pool := load(t)
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(t.Context(), `INSERT INTO prefixes (zone, prefix, first_seen, last_seen, leak_suspect, note, expected_domains)
		VALUES ('example.com', 'leaky', $1, $1, true, 'gave it to a shop', '{shop.example.net}')`, at); err != nil {
		t.Fatal(err)
	}
	yes := true
	leaks, err := db.PrefixStats(t.Context(), operator, store.PrefixQuery{LeakSuspect: &yes})
	if err != nil {
		t.Fatal(err)
	}
	if leaks.Total != 1 || len(leaks.Prefixes) != 1 {
		t.Fatalf("leaks %+v", leaks)
	}
	if l := leaks.Prefixes[0]; l.Prefix != "leaky" || !l.LeakSuspect || l.Messages != 0 || l.State != store.PrefixDiscovered ||
		l.Note != "gave it to a shop" || !slices.Equal(l.ExpectedDomains, []string{"shop.example.net"}) || !l.Active {
		t.Errorf("leaky %+v", l)
	}
	// The totals count all visible mail whatever the filter.
	if leaks.Messages != 19 {
		t.Errorf("totals %d, want 19", leaks.Messages)
	}
	one, err := db.PrefixStats(t.Context(), operator, store.PrefixQuery{Limit: 1})
	if err != nil || one.Total != 2 || len(one.Prefixes) != 1 || one.Prefixes[0].Prefix != "leaky" {
		t.Errorf("limit 1: %+v %v", one, err)
	}
}

// Every read must fail without a viewer rather than see everything
// (§13.8). New read functions join this list.
func TestReadsRefuseNilViewer(t *testing.T) {
	t.Parallel()
	db, _ := storetest.New(t)
	id := ulid.New(time.Now())
	reads := map[string]func() error{
		"Search":       func() error { _, err := db.Search(t.Context(), nil, store.SearchQuery{}); return err },
		"ListMessages": func() error { _, err := db.ListMessages(t.Context(), nil, store.MessageQuery{}); return err },
		"MessageByID":  func() error { _, err := db.MessageByID(t.Context(), nil, id); return err },
		"ThreadByID":   func() error { _, err := db.ThreadByID(t.Context(), nil, id); return err },
		"PrefixStats":  func() error { _, err := db.PrefixStats(t.Context(), nil, store.PrefixQuery{}); return err },
	}
	for name, read := range reads {
		if err := read(); err == nil || errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s with a nil viewer: %v", name, err)
		}
	}
}
