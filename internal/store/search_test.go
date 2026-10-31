package store_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/classify"
	"github.com/yuuka-mail/yuuka/internal/contenthash"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// load commits and folds every corpus message to a claimed prefix, one
// second apart in file name order.
func load(t *testing.T) (*store.DB, *pgxpool.Pool) {
	t.Helper()
	db, pool := storetest.New(t)
	if _, err := pool.Exec(t.Context(), `INSERT INTO prefixes (zone, prefix, first_seen, last_seen, state) VALUES ('example.com', 'a', now(), now(), 1)`); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob("../../testdata/eml/*.eml")
	for i, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		at := time.Date(2026, 10, 31, 0, 0, i, 0, time.UTC)
		id := ulid.New(at)
		key := fmt.Sprintf("raw/2026/10/%s.eml", id)
		in := store.Ingest{
			RawID: id, R2Key: key, LocalPath: key, Size: int64(len(raw)), SHA256: sha256.Sum256(raw),
			ContentHash: contenthash.V1(raw), ReceivedAt: at, DeliveryID: id, Account: "cf",
			Zone: "example.com", Prefix: "a", EnvelopeTo: "a@example.com",
		}
		if _, err := db.CommitIngest(t.Context(), in); err != nil {
			t.Fatal(err)
		}
		ref := store.RawRef{
			DeliveryID: id, RawID: id, R2Key: key, LocalPath: key, ContentHash: in.ContentHash[:],
			Zone: "example.com", Prefix: "a", ReceivedAt: at,
		}
		if _, err := db.Fold(t.Context(), ref, mimeparse.Parse(raw), authres.Extract(raw, "mx.cloudflare.net")); err != nil {
			t.Fatal(err)
		}
	}
	return db, pool
}

func TestSearch(t *testing.T) {
	t.Parallel()
	db, _ := load(t)
	v := &auth.Viewer{PrincipalKind: "human", PrincipalName: "operator"}
	cases := []struct {
		text      string
		downgrade bool
		want      []string // subjects, newest first
	}{
		{"验证码", false, []string{"【示例】验证码通知"}},       // GB2312 subject and body
		{"堃", false, []string{"关于会议时间调整（堃、喆）"}},     // single GBK-only character, raw 8-bit header
		{"会议 三点", false, []string{"关于会议时间调整（堃、喆）"}}, // all terms must match, one in the body
		{"FRIDAY", false, []string{"Re: Lunch on Friday?"}},
		{"bob@example", false, []string{"Re: Lunch on Friday?"}},    // sender address
		{"Example Weekly", false, []string{"This week at Example"}}, // sender name
		{"missed", false, []string{"This week at Example"}},         // HTML-only body
		{"100%", false, nil},  // % is literal
		{"prize", false, nil}, // dmarc=fail: downgraded
		{"prize", true, []string{"You have won"}},
		{"verify", true, []string{"Verify your account"}},
	}
	for _, c := range cases {
		res, err := db.Search(t.Context(), v, store.SearchQuery{Text: c.text, IncludeDowngraded: c.downgrade})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, h := range res.Hits {
			got = append(got, h.Subject)
		}
		if strings.Join(got, "|") != strings.Join(c.want, "|") || res.Total != len(c.want) {
			t.Errorf("%q: total %d %q, want %q", c.text, res.Total, got, c.want)
		}
	}
}

func TestSearchPaging(t *testing.T) {
	t.Parallel()
	db, _ := load(t)
	all, err := db.Search(t.Context(), &auth.Viewer{}, store.SearchQuery{IncludeDowngraded: true})
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob("../../testdata/eml/*.eml")
	if all.Total != len(files) || len(all.Hits) != len(files) {
		t.Fatalf("total %d hits %d, want %d", all.Total, len(all.Hits), len(files))
	}
	page, err := db.Search(t.Context(), &auth.Viewer{}, store.SearchQuery{IncludeDowngraded: true, Limit: 3, Offset: 3})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != all.Total || len(page.Hits) != 3 || page.Hits[0].ID != all.Hits[3].ID {
		t.Fatalf("page %+v", page)
	}
	past, err := db.Search(t.Context(), &auth.Viewer{}, store.SearchQuery{IncludeDowngraded: true, Offset: 1000})
	if err != nil || past.Total != all.Total || len(past.Hits) != 0 {
		t.Fatalf("past the end: %+v %v", past, err)
	}
	for i := 1; i < len(all.Hits); i++ {
		if all.Hits[i].ReceivedAt.After(all.Hits[i-1].ReceivedAt) {
			t.Fatal("not newest first")
		}
	}
}

// The search predicate must be the indexed expression, or every search
// is a sequential scan of all bodies.
func TestSearchUsesTrigramIndex(t *testing.T) {
	t.Parallel()
	_, pool := load(t)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err := tx.Exec(t.Context(), `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(t.Context(), `EXPLAIN SELECT m.id FROM messages m WHERE `+store.SearchDoc+` ILIKE '%verification%'`)
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	if !strings.Contains(plan.String(), "messages_search") {
		t.Fatalf("plan does not use messages_search:\n%s", plan.String())
	}
}

// A read without a viewer must fail rather than see everything (§13.8).
func TestSearchRefusesNilViewer(t *testing.T) {
	t.Parallel()
	db, _ := storetest.New(t)
	if _, err := db.Search(t.Context(), nil, store.SearchQuery{}); err == nil {
		t.Fatal("search with a nil viewer succeeded")
	}
}

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
