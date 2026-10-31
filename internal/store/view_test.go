package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/classify"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

func TestSearchFilters(t *testing.T) {
	t.Parallel()
	db, pool := load(t)
	v := &auth.Viewer{PrincipalKind: "agent", PrincipalName: "arisu"}
	all, err := db.Search(t.Context(), v, store.SearchQuery{IncludeDowngraded: true})
	if err != nil {
		t.Fatal(err)
	}
	// Move the newest message to another prefix.
	newest := all.Hits[0]
	for _, q := range []string{
		`INSERT INTO prefixes (zone, prefix, first_seen, last_seen, state) VALUES ('example.com', 'b', now(), now(), 1)`,
		`UPDATE deliveries SET prefix = 'b' WHERE message_id = '` + newest.ID + `'`,
	} {
		if _, err := pool.Exec(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	human := classify.Human
	cases := []struct {
		name string
		q    store.SearchQuery
		want func(store.SearchHit) bool
	}{
		{"prefix", store.SearchQuery{Prefix: "b"}, func(h store.SearchHit) bool { return h.ID == newest.ID }},
		{"prefix and zone", store.SearchQuery{Prefix: "b", Zone: "example.com"}, func(h store.SearchHit) bool { return h.ID == newest.ID }},
		{"other zone", store.SearchQuery{Prefix: "b", Zone: "example.net"}, func(store.SearchHit) bool { return false }},
		{"class", store.SearchQuery{Class: &human}, func(h store.SearchHit) bool { return h.Class == classify.Human && !h.Downgraded }},
		{"since", store.SearchQuery{Since: newest.ReceivedAt, IncludeDowngraded: true}, func(h store.SearchHit) bool { return !h.ReceivedAt.Before(newest.ReceivedAt) }},
	}
	for _, c := range cases {
		res, err := db.Search(t.Context(), v, c.q)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		want := 0
		for _, h := range all.Hits {
			if c.q.IncludeDowngraded || !h.Downgraded {
				if c.want(h) {
					want++
				}
			}
		}
		if res.Total != want || len(res.Hits) != want {
			t.Errorf("%s: total %d hits %d, want %d", c.name, res.Total, len(res.Hits), want)
		}
		for _, h := range res.Hits {
			if !c.want(h) {
				t.Errorf("%s: unexpected hit %s %q", c.name, h.ID, h.Subject)
			}
		}
	}
}

func TestMessage(t *testing.T) {
	t.Parallel()
	db, pool := load(t)
	v := &auth.Viewer{PrincipalKind: "agent", PrincipalName: "arisu"}
	res, err := db.Search(t.Context(), v, store.SearchQuery{Text: "verification code is 482913"})
	if err != nil || len(res.Hits) != 1 {
		t.Fatalf("search: %+v %v", res, err)
	}
	id := res.Hits[0].ID
	m, err := db.Message(t.Context(), v, id)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != id || m.Class != classify.Machine || m.Downgraded || len(m.Deliveries) != 1 {
		t.Fatalf("message = %+v", m)
	}
	d := m.Deliveries[0]
	if d.EnvelopeTo != "a@example.com" || !d.Auth.Trusted || d.Auth.DMARC != "pass" || d.Raw.R2Key == "" {
		t.Errorf("delivery = %+v", d)
	}

	// An alias left by a merge (§4.2) still resolves.
	alias := ulid.New(time.Now())
	if _, err := pool.Exec(t.Context(), `INSERT INTO message_aliases (old_id, new_id) VALUES ($1, $2)`, alias, id); err != nil {
		t.Fatal(err)
	}
	if m, err := db.Message(t.Context(), v, alias); err != nil || m.ID != id {
		t.Errorf("alias: %+v %v", m, err)
	}
	if _, err := db.Message(t.Context(), v, ulid.New(time.Now())); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing message: %v", err)
	}
}

func TestPrefixes(t *testing.T) {
	t.Parallel()
	db, pool := load(t)
	v := &auth.Viewer{PrincipalKind: "agent", PrincipalName: "arisu"}
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(t.Context(), `INSERT INTO prefixes (zone, prefix, first_seen, last_seen, leak_suspect)
		VALUES ('example.com', 'leaky', $1, $1, true)`, at); err != nil {
		t.Fatal(err)
	}
	all, err := db.Prefixes(t.Context(), v, store.PrefixQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if all.Total != 2 || len(all.Prefixes) != 2 || all.Prefixes[0].Prefix != "leaky" {
		t.Fatalf("all = %+v", all)
	}
	a := all.Prefixes[1]
	if a.Prefix != "a" || !a.Claimed || a.Messages == 0 || a.LeakSuspect {
		t.Errorf("a = %+v", a)
	}
	yes := true
	leaks, err := db.Prefixes(t.Context(), v, store.PrefixQuery{LeakSuspect: &yes})
	if err != nil || leaks.Total != 1 || leaks.Prefixes[0].Prefix != "leaky" || leaks.Prefixes[0].Claimed || leaks.Prefixes[0].Messages != 0 {
		t.Errorf("leaks = %+v %v", leaks, err)
	}
}

// Every read refuses a missing viewer rather than seeing everything
// (§13.8). New read paths belong in this table.
func TestReadsRefuseNilViewer(t *testing.T) {
	t.Parallel()
	db, _ := storetest.New(t)
	reads := map[string]func() error{
		"Search":   func() error { _, err := db.Search(t.Context(), nil, store.SearchQuery{}); return err },
		"Message":  func() error { _, err := db.Message(t.Context(), nil, ulid.New(time.Now())); return err },
		"Prefixes": func() error { _, err := db.Prefixes(t.Context(), nil, store.PrefixQuery{}); return err },
	}
	for name, read := range reads {
		if err := read(); err == nil || errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s with a nil viewer: %v", name, err)
		}
	}
}
