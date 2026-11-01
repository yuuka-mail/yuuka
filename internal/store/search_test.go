package store_test

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/classify"
	"github.com/yuuka-mail/yuuka/internal/contenthash"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/search"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// load commits and folds every corpus message to a claimed prefix, one
// second apart in file name order.
func load(t *testing.T) (*store.DB, *pgxpool.Pool) {
	t.Helper()
	db, pool := storetest.New(t)
	// Claimed when the corpus arrives, not at the wall clock: tests deliver
	// "later" mail at fixed times, which now() overtakes.
	if _, err := pool.Exec(t.Context(), `INSERT INTO prefixes (zone, prefix, first_seen, last_seen, state)
		VALUES ('example.com', 'a', '2026-10-31T00:00:00Z', '2026-10-31T00:00:00Z', 1)`); err != nil {
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
			ContentHash: contenthash.V1(raw), ReceivedAt: at, DeliveryID: id, AccountID: storetest.Account(t, db),
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

// hitSubjects runs q and returns the subjects of the hits, newest first,
// checking that the total agrees.
func hitSubjects(t *testing.T, db *store.DB, v *auth.Viewer, q string, downgraded bool) []string {
	t.Helper()
	res, err := db.Search(t.Context(), v, store.SearchQuery{Filter: storetest.Query(t, q), IncludeDowngraded: downgraded, Limit: 500})
	if err != nil {
		t.Fatalf("%q: %v", q, err)
	}
	got := []string{}
	for _, h := range res.Hits {
		got = append(got, h.Subject)
	}
	if res.Total != len(got) {
		t.Errorf("%q: total %d, %d hits", q, res.Total, len(got))
	}
	return got
}

func TestSearch(t *testing.T) {
	t.Parallel()
	db, _ := load(t)
	v := &auth.Viewer{PrincipalKind: "human", PrincipalName: "operator"}
	lunch, weekly, otp := "Re: Lunch on Friday?", "This week at Example", "【示例】验证码通知"
	cases := []struct {
		q         string
		downgrade bool
		want      []string // subjects, newest first
	}{
		{"验证码", false, []string{otp}},               // GB2312 subject and body
		{"堃", false, []string{"关于会议时间调整（堃、喆）"}},     // single GBK-only character, raw 8-bit header
		{"会议 三点", false, []string{"关于会议时间调整（堃、喆）"}}, // all terms must match, one in the body
		{"FRIDAY", false, []string{lunch}},
		{"bob@example", false, []string{lunch}},     // sender address
		{"Example Weekly", false, []string{weekly}}, // sender name
		{"missed", false, []string{weekly}},         // HTML-only body
		{"100%", false, []string{}},                 // % is literal
		{"prize", false, []string{}},                // dmarc=fail: downgraded
		{"prize", true, []string{"You have won"}},
		{"verify", true, []string{"Verify your account"}},
		// A phrase must occur as written; its words alone are two terms.
		{`"lunch on"`, false, []string{lunch}},
		{`"on lunch"`, false, []string{}},
		{`on lunch`, false, []string{lunch}},
		// OR, negation, grouping.
		{"friday OR missed", false, []string{weekly, lunch}},
		{"(friday OR missed) -lunch", false, []string{weekly}},
		{"-(friday OR missed) 堃", false, []string{"关于会议时间调整（堃、喆）"}},
		// Not operators: searched as written.
		{"Re:Lunch", false, []string{}},
		{"Re: Lunch", false, []string{lunch}},
		// Fields.
		{"from:bob", false, []string{lunch}},
		{`from:"example weekly"`, false, []string{weekly}},
		{"subject:friday", false, []string{lunch}},
		{"subject:missed", false, []string{}},
		{"body:missed", false, []string{weekly}},
		{"to:news@", false, []string{weekly}},
		{"to:svc-shop@example.com", false, []string{otp}},
		{"to:Alice friday", false, []string{lunch}},
		{"list:weekly.news", false, []string{weekly}},
		{"list:WEEKLY", false, []string{weekly}},
		{"class:human friday", false, []string{lunch}},
		{"class:machine friday", false, []string{}},
		{"deliveredto:a@example.net", false, []string{}},
		{"deliveredto:b", false, []string{}},
		{"is:archived", false, []string{}},
		{"is:sent", false, []string{}},
		// is:downgraded lifts the default hiding, wherever it is.
		{"is:downgraded", false, []string{"Urgent: confirm your account", "You have won", "Verify your account"}},
		{"prize is:downgraded", false, []string{"You have won"}},
		{"prize -is:downgraded", false, []string{}},
		// Untrusted text is a value, never SQL.
		{`"'; DROP TABLE messages; --"`, false, []string{}},
		{`a_b \ $1`, false, []string{}},
	}
	for _, c := range cases {
		if got := hitSubjects(t, db, v, c.q, c.downgrade); !slices.Equal(got, c.want) {
			t.Errorf("%q: %q, want %q", c.q, got, c.want)
		}
	}
	// Everything not downgraded was delivered to a@example.com.
	all := hitSubjects(t, db, v, "", false)
	for _, q := range []string{"deliveredto:a", "deliveredto:a@example.com", "-is:archived", "-is:sent"} {
		if got := hitSubjects(t, db, v, q, false); !slices.Equal(got, all) {
			t.Errorf("%q: %q, want all", q, got)
		}
	}
}

// Each condition and its negation split the visible mail: NOT is "none
// of", and a fact that is missing (no message_state row, recipients or
// List-Id not read yet) reads as false, not as unknown.
func TestSearchNegation(t *testing.T) {
	t.Parallel()
	db, pool := load(t)
	v := &auth.Viewer{PrincipalID: store.SystemPrincipalID, PrincipalKind: "human", PrincipalName: "operator", Scopes: []auth.Scope{auth.DeliveryRead}}
	ctx := t.Context()
	var ids []string
	if err := pool.QueryRow(ctx, `SELECT array_agg(id ORDER BY received_at) FROM messages`).Scan(&ids); err != nil {
		t.Fatal(err)
	}
	// One message not read by the 0007, 0010 and 0016 backlog yet, one
	// copied to someone, one labelled, archived and reclassified.
	if _, err := pool.Exec(ctx, `UPDATE messages SET to_addrs = NULL, cc_addrs = NULL, reply_to_addrs = NULL, list_id = NULL,
		to_names = NULL, cc_names = NULL WHERE id = $1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE messages SET cc_addrs = '{copied@example.net}' WHERE id = $1`, ids[1]); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, _, err := db.Annotate(ctx, v, ids[2], []store.Annotation{store.SetLabel("带 空格", true), store.SetArchived(true), store.SetClass(classify.Human)},
		store.Action{ID: ulid.New(now), Actor: store.SystemPrincipalID, Tool: "test", At: now}); err != nil {
		t.Fatal(err)
	}
	all := hitSubjects(t, db, v, "", true)
	for _, q := range []string{
		"verification", `"lunch on"`, "from:example.org", "to:alice", "cc:copied", "bcc:anyone", "subject:re", "body:the",
		"list:example", "after:2026-10-31T00:00:05Z", "before:2026-10-31T00:00:05Z", "has:attachment",
		"deliveredto:a", "deliveredto:a@example.com", `label:"带 空格"`, "label:other", "is:archived", "is:downgraded",
		"is:sent", "class:human", "class:machine", "class:unknown", "friday OR missed", "from:example.org verification",
	} {
		in := hitSubjects(t, db, v, q, true)
		out := hitSubjects(t, db, v, "-("+q+")", true)
		if len(in)+len(out) != len(all) {
			t.Errorf("%q: %d, negated %d, of %d", q, len(in), len(out), len(all))
		}
		for _, s := range in {
			if slices.Contains(out, s) {
				t.Errorf("%q: %q both matches and does not", q, s)
			}
		}
	}
	for q, want := range map[string]int{"cc:copied@": 1, `label:"带 空格"`: 1, "label:带": 0, "is:archived": 1} {
		if got := len(hitSubjects(t, db, v, q, true)); got != want {
			t.Errorf("%q: %d hits, want %d", q, got, want)
		}
	}
}

// to:, cc: and text match the display names of To and Cc (0016), which
// are derived: written from the raw, NULL until the backlog pass reads a
// row from before the migration, and rewritten the same by a rebuild.
func TestSearchRecipientNames(t *testing.T) {
	t.Parallel()
	db, pool := storetest.New(t)
	ctx := t.Context()
	acct := storetest.Account(t, db)
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	namedRaw := letter("a@example.net", "=?utf-8?b?546L5bCP5piO?= <wang@example.com>, wang@example.com", "plan",
		"Cc: \"Dev Team\" <dev@example.org>, Ops <ops@example.org>, Ops <ops2@example.org>\r\n", "x")
	named := inbound(t, db, acct, "a", namedRaw, at)
	plain := inbound(t, db, acct, "a", letter("b@example.net", "lee@example.com", "plan", "", "y"), at.Add(time.Minute))
	names := func(id string) (to, cc []string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT to_names, cc_names FROM messages WHERE id = $1`, id).Scan(&to, &cc); err != nil {
			t.Fatal(err)
		}
		return to, cc
	}
	if to, cc := names(named); !slices.Equal(to, []string{"王小明"}) || !slices.Equal(cc, []string{"Dev Team", "Ops"}) {
		t.Errorf("names %q %q", to, cc)
	}
	if to, cc := names(plain); to == nil || cc == nil || len(to)+len(cc) != 0 {
		t.Errorf("no names: %#v %#v, want empty, not NULL", to, cc)
	}

	v := &auth.Viewer{PrincipalKind: "human", PrincipalName: "operator"}
	ids := func(q string) []string {
		t.Helper()
		// The prefix is not claimed: the mail is downgraded.
		res, err := db.Search(ctx, v, store.SearchQuery{Filter: storetest.Query(t, q), IncludeDowngraded: true})
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		out := []string{}
		for _, h := range res.Hits {
			out = append(out, h.ID)
		}
		if res.Total != len(out) {
			t.Errorf("%q: total %d, %d hits", q, res.Total, len(out))
		}
		return out
	}
	check := func(when string, cases map[string][]string) {
		t.Helper()
		for q, want := range cases {
			if got := ids(q); !slices.Equal(got, want) {
				t.Errorf("%s: %q: %q, want %q", when, q, got, want)
			}
		}
	}
	check("read", map[string][]string{
		"to:王小明":           {named},
		"to:wang@":         {named},
		`to:"dev team"`:    {},
		`cc:"dev team"`:    {named},
		"cc:OPS":           {named},
		"王小明":              {named},
		`"Dev Team"`:       {named},
		"lee@example.com":  {plain},
		"-to:王小明":          {plain},
		"to:王小明 cc:ops":    {named},
		"to:王小明 OR to:lee": {plain, named},
	})

	// A row from before 0016: found by its addresses only, until the
	// backlog pass reads it.
	if _, err := pool.Exec(ctx, `UPDATE messages SET to_names = NULL WHERE id = $1`, named); err == nil {
		t.Error("to_names NULL without cc_names: the pending marker would be ambiguous")
	}
	if _, err := pool.Exec(ctx, `UPDATE messages SET to_names = NULL, cc_names = NULL WHERE id = $1`, named); err != nil {
		t.Fatal(err)
	}
	check("unread", map[string][]string{
		"to:王小明":   {},
		"-to:王小明":  {plain, named},
		"王小明":      {},
		"to:wang@": {named},
		"cc:ops@":  {named},
	})
	pending, err := db.UnreadNames(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].MessageID != named {
		t.Fatalf("unread names %+v, %v", pending, err)
	}
	rebuild := func() bool {
		t.Helper()
		d := pending[0].Deliveries[0]
		changed, err := db.Rebuild(ctx, pending[0], mimeparse.Parse(namedRaw),
			map[string]authres.Auth{d.DeliveryID: authres.Extract(namedRaw, "mx.cloudflare.net")})
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}
	if !rebuild() {
		t.Error("filling the names changed nothing")
	}
	if pending, err := db.UnreadNames(ctx, 10); err != nil || len(pending) != 0 {
		t.Errorf("after rebuild: %+v, %v", pending, err)
	}
	if to, cc := names(named); !slices.Equal(to, []string{"王小明"}) || !slices.Equal(cc, []string{"Dev Team", "Ops"}) {
		t.Errorf("names after rebuild %q %q", to, cc)
	}
	// Reparse is deterministic: the same raw writes the same names.
	if rebuild() {
		t.Error("rebuilding from the same raw changed something")
	}
	check("rebuilt", map[string][]string{"to:王小明": {named}, "王小明": {named}})
}

// Dates start their day in the query's zone; after is inclusive and
// before exclusive (JMAP), so after:d before:d+1 is exactly day d.
func TestSearchTime(t *testing.T) {
	t.Parallel()
	db, pool := load(t)
	v := &auth.Viewer{PrincipalKind: "human", PrincipalName: "operator"}
	ctx := t.Context()
	// The corpus arrived on 2026-10-31 UTC; move one to midnight after.
	var first string
	if err := pool.QueryRow(ctx, `SELECT min(id) FROM messages`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE messages SET received_at = '2026-11-01T00:00:00Z' WHERE id = $1`, first); err != nil {
		t.Fatal(err)
	}
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	count := func(q string, loc *time.Location) int {
		t.Helper()
		p, err := search.Parse(q, search.Env{Loc: loc, Now: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)})
		if err != nil {
			t.Fatal(err)
		}
		res, err := db.Search(ctx, v, store.SearchQuery{Filter: p.Filter, IncludeDowngraded: true})
		if err != nil {
			t.Fatal(err)
		}
		return res.Total
	}
	n := count("", nil)
	for _, c := range []struct {
		q    string
		loc  *time.Location
		want int
	}{
		{"after:2026-10-31 before:2026-11-01", nil, n - 1},
		{"after:2026-11-01", nil, 1},
		{"before:2026-11-01T00:00:00Z", nil, n - 1},
		{"after:2026-11-01T08:00:00+08:00", nil, 1},
		// 2026-11-01T00:00Z is still 10-31 in Los Angeles, the rest 10-30.
		{"after:2026-11-01", la, 0},
		{"after:2026-10-31 before:2026-11-01", la, 1},
		{"before:2026-10-31", la, n - 1},
		{"before:-1h", nil, n},
		{"after:-1h", nil, 0},
		{"after:-25h", nil, 1},
		{"after:-2d", nil, n},
		{"after:-1w", nil, n},
	} {
		if got := count(c.q, c.loc); got != c.want {
			t.Errorf("%q in %v: %d, want %d", c.q, c.loc, got, c.want)
		}
	}
}

// The extensions keep the semantics the list gives them: a visible
// delivery to the prefix, the effective class, the triage leaning of the
// active profile.
func TestSearchExtensions(t *testing.T) {
	t.Parallel()
	db, pool := load(t)
	v := &auth.Viewer{PrincipalKind: "agent", PrincipalName: "arisu"}
	ctx := t.Context()
	all, err := db.Search(ctx, v, store.SearchQuery{IncludeDowngraded: true})
	if err != nil {
		t.Fatal(err)
	}
	// Move the newest message to another prefix, and score it.
	newest := all.Hits[0]
	for _, q := range []string{
		`INSERT INTO prefixes (zone, prefix, first_seen, last_seen, state) VALUES ('example.com', 'b', now(), now(), 1)`,
		`UPDATE deliveries SET prefix = 'b' WHERE message_id = '` + newest.ID + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.InsertScore(ctx, newScore(newest.ID, laya, time.Now(), 0.9)); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		q    string
		want func(store.SearchHit) bool
	}{
		{"deliveredto:b", func(h store.SearchHit) bool { return h.ID == newest.ID }},
		{"deliveredto:B@Example.com", func(h store.SearchHit) bool { return h.ID == newest.ID }},
		{"deliveredto:b@example.net", func(store.SearchHit) bool { return false }},
		{"-deliveredto:b", func(h store.SearchHit) bool { return h.ID != newest.ID && !h.Downgraded }},
		{"class:human", func(h store.SearchHit) bool { return h.Class == classify.Human && !h.Downgraded }},
		{"after:" + newest.ReceivedAt.Format(time.RFC3339Nano), func(h store.SearchHit) bool {
			return !h.ReceivedAt.Before(newest.ReceivedAt) && !h.Downgraded
		}},
		{"triage:phishing:yes", func(h store.SearchHit) bool { return h.ID == newest.ID }},
		{"triage:phishing:no", func(store.SearchHit) bool { return false }},
		{"-triage:phishing:yes is:downgraded", func(h store.SearchHit) bool { return h.Downgraded }},
	}
	for _, c := range cases {
		res, err := db.Search(ctx, v, store.SearchQuery{Filter: storetest.Query(t, c.q), Triage: &laya})
		if err != nil {
			t.Fatalf("%s: %v", c.q, err)
		}
		want := 0
		for _, h := range all.Hits {
			if c.want(h) {
				want++
			}
		}
		if res.Total != want || len(res.Hits) != want {
			t.Errorf("%s: total %d hits %d, want %d", c.q, res.Total, len(res.Hits), want)
		}
		for _, h := range res.Hits {
			if !c.want(h) {
				t.Errorf("%s: unexpected hit %s %q", c.q, h.ID, h.Subject)
			}
		}
	}
	// What the filter cannot do for this caller is refused, never
	// answered with nothing: no scorer, Bcc without delivery:read.
	for q, want := range map[string]search.Error{
		"triage:phishing:yes": {At: 1},
		"bcc:x":               {At: 1, Scope: auth.DeliveryRead},
		"a -bcc:x":            {At: 4, Scope: auth.DeliveryRead},
	} {
		_, err := db.Search(ctx, v, store.SearchQuery{Filter: storetest.Query(t, q)})
		if e, ok := errors.AsType[*search.Error](err); !ok || e.At != want.At || e.Scope != want.Scope {
			t.Errorf("%s: %v, want a search error at %d", q, err, want.At)
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
	after := &store.MessageCursor{ReceivedAt: all.Hits[2].ReceivedAt, ID: all.Hits[2].ID}
	page, err := db.Search(t.Context(), &auth.Viewer{}, store.SearchQuery{IncludeDowngraded: true, Limit: 3, Before: after})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != all.Total || len(page.Hits) != 3 || page.Hits[0].ID != all.Hits[3].ID {
		t.Fatalf("page %+v", page)
	}
	last := all.Hits[len(all.Hits)-1]
	past, err := db.Search(t.Context(), &auth.Viewer{}, store.SearchQuery{IncludeDowngraded: true, Before: &store.MessageCursor{ReceivedAt: last.ReceivedAt, ID: last.ID}})
	if err != nil || past.Total != all.Total || len(past.Hits) != 0 {
		t.Fatalf("past the end: %+v %v", past, err)
	}
	for i := 1; i < len(all.Hits); i++ {
		if all.Hits[i].ReceivedAt.After(all.Hits[i-1].ReceivedAt) {
			t.Fatal("not newest first")
		}
	}
}

// The text predicate must be the indexed expression, and a field
// condition must carry it too, or every search is a sequential scan of
// all bodies.
func TestSearchUsesTrigramIndex(t *testing.T) {
	t.Parallel()
	db, pool := load(t)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err := tx.Exec(t.Context(), `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	// With delivery:read, text also looks for a Bcc of our own mail: an
	// initplan whose content hashes are ORed with the trigram index, so
	// the index still serves the rest of it.
	bcc := &auth.Viewer{Scopes: []auth.Scope{auth.DeliveryRead}}
	for _, c := range []struct {
		q string
		v *auth.Viewer
	}{
		{"verification", &auth.Viewer{}},
		{"subject:verification", &auth.Viewer{}},
		{"from:example", &auth.Viewer{}},
		{"body:code", &auth.Viewer{}},
		{"to:alice", &auth.Viewer{}},
		{"cc:example", &auth.Viewer{}},
		{"verification", bcc},
	} {
		q, v := c.q, c.v
		where, args, err := db.CompileSearch(v, storetest.Query(t, q))
		if err != nil {
			t.Fatal(err)
		}
		rows, err := tx.Query(t.Context(), `EXPLAIN SELECT m.id FROM messages m WHERE `+where, args...)
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
		if !strings.Contains(plan.String(), "messages_search_text") {
			t.Errorf("%q: plan does not use messages_search_text:\n%s", q, plan.String())
		}
		if v == bcc && (!strings.Contains(plan.String(), "BitmapOr") || !strings.Contains(plan.String(), "messages_content_hash_key")) {
			t.Errorf("%q with delivery:read: the Bcc branch is not ORed with the index:\n%s", q, plan.String())
		}
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
