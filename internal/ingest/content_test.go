package ingest

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/contenthash"
	"github.com/yuuka-mail/yuuka/internal/contract"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
)

func eml(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "eml", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// mail builds a message the trusted receiver passed. headers go below
// the receiver's block, in the sender's part of the header.
func mail(rcpt, dmarc, headers, body string) []byte {
	return fmt.Appendf(nil, "Received: from mail.example.net (192.0.2.10)\r\n"+
		"\tby cloudflare-email.net (cloudflare) id tx for <%s>; Fri, 31 Oct 2026 01:41:38 +0000\r\n"+
		"Authentication-Results: mx.cloudflare.net; dmarc=%s header.from=example.net\r\n"+
		"%sTo: %s\r\n\r\n%s\r\n", rcpt, dmarc, headers, rcpt, body)
}

// ingest delivers raw to rcpt at e.now+offset and commits it.
func (e *env) ingest(t *testing.T, offset time.Duration, rcpt string, raw []byte) contract.Marker {
	t.Helper()
	m := e.deliver(t, e.now.Add(offset), "s@example.net", rcpt, raw)
	if out, err := e.in.Ingest(t.Context(), m); err != nil || out != store.Delivered {
		t.Fatalf("ingest %s: %v %v", rcpt, out, err)
	}
	return m
}

func (e *env) messageOf(t *testing.T, rawID string) (id, thread string) {
	t.Helper()
	err := e.pool.QueryRow(t.Context(), `
		SELECT m.id, m.thread_id FROM deliveries d JOIN messages m ON m.id = d.message_id WHERE d.raw_id = $1`,
		rawID).Scan(&id, &thread)
	if err != nil {
		t.Fatalf("message of raw %s: %v", rawID, err)
	}
	return id, thread
}

func TestSpamDowngrade(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.claim(t, "github")
	hdr := "From: a@example.net\r\nSubject: s\r\n"
	// Discovered prefix: stored, not notified.
	e.ingest(t, 0, "random1@example.com", mail("random1@example.com", "pass", hdr+"Message-ID: <1@x>\r\n", "a"))
	// Claimed prefix, but the trusted receiver says dmarc=fail; the forged
	// pass the sender wrote below must not rescue it.
	e.ingest(t, time.Second, "github@example.com", eml(t, "forged-auth.eml"))
	// Claimed and not failing: notified.
	e.ingest(t, 2*time.Second, "github@example.com", mail("github@example.com", "pass", hdr+"Message-ID: <3@x>\r\n", "c"))
	e.in.Wait()

	if got := e.notes.got(); len(got) != 1 {
		t.Fatalf("notified %q, want only the claimed, passing one", got)
	}
	if n := e.count(t, `SELECT count(*) FROM messages`); n != 3 {
		t.Fatalf("%d messages; downgraded mail must still be stored", n)
	}
	var state int
	if err := e.pool.QueryRow(t.Context(), `SELECT state FROM prefixes WHERE zone = 'example.com' AND prefix = 'random1'`).Scan(&state); err != nil || state != store.PrefixDiscovered {
		t.Fatalf("random1 registered as %d (%v), want discovered", state, err)
	}
	// Excluded from default search, found when asked for.
	res, err := e.in.cfg.DB.Search(t.Context(), &auth.Viewer{}, store.SearchQuery{})
	if err != nil || res.Total != 1 {
		t.Fatalf("default search total %d (%v), want 1", res.Total, err)
	}
	res, err = e.in.cfg.DB.Search(t.Context(), &auth.Viewer{}, store.SearchQuery{IncludeDowngraded: true})
	if err != nil || res.Total != 3 {
		t.Fatalf("full search total %d (%v), want 3", res.Total, err)
	}
	var dmarc string
	if err := e.pool.QueryRow(t.Context(), `
		SELECT d.auth->>'dmarc' FROM deliveries d JOIN messages m ON m.id = d.message_id
		WHERE m.message_id = 'forged-15@spam.example'`).Scan(&dmarc); err != nil || dmarc != "fail" {
		t.Fatalf("auth dmarc %q %v", dmarc, err)
	}
}

// The same content to two prefixes is one message with two deliveries,
// each with its own auth; the second, claimed one is still notified once.
func TestFoldByContentHash(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.claim(t, "b")
	hdr := "From: Alice <alice@example.net>\r\nSubject: cc\r\nMessage-ID: <cc@example.net>\r\n"
	a := e.ingest(t, 0, "a@example.com", mail("x@example.com", "pass", hdr, "hi"))
	b := e.ingest(t, time.Second, "b@example.com", mail("x@example.com", "pass", hdr, "hi"))
	e.in.Wait()
	ma, _ := e.messageOf(t, a.ID)
	mb, _ := e.messageOf(t, b.ID)
	if ma != mb {
		t.Fatalf("two messages %s %s for one content", ma, mb)
	}
	if n := e.count(t, `SELECT count(*) FROM deliveries WHERE message_id = $1 AND auth->>'trusted' = 'true'`, ma); n != 2 {
		t.Fatalf("%d deliveries with auth", n)
	}
	if got := e.notes.got(); len(got) != 1 {
		t.Fatalf("notified %d times, want once (via b)", len(got))
	}
	var canonical string
	if err := e.pool.QueryRow(t.Context(), `SELECT canonical_raw_id FROM messages WHERE id = $1`, ma).Scan(&canonical); err != nil || canonical != a.ID {
		t.Fatalf("canonical raw %s, want the earliest %s", canonical, a.ID)
	}
}

func TestThreading(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	seq := 0
	msg := func(id, irt, from, subject string) []byte {
		seq++ // distinct bodies: identical content would be a redelivery
		h := "From: " + from + "\r\nSubject: " + subject + "\r\n"
		if id != "" {
			h += "Message-ID: <" + id + ">\r\n"
		}
		if irt != "" {
			h += "In-Reply-To: <" + irt + ">\r\n"
		}
		return mail("alice@example.com", "pass", h, fmt.Sprintf("message %d", seq))
	}
	at := time.Duration(0)
	next := func(raw []byte) string {
		at += time.Minute
		return e.ingest(t, at, "alice@example.com", raw).ID
	}
	// A child arriving before its parent, then the parent: one thread.
	child := next(msg("c@x", "p@x", "bob@example.org", "Re: lunch"))
	parent := next(msg("p@x", "", "alice@example.net", "lunch"))
	// Two separate threads that a third message bridges.
	x := next(msg("x@x", "", "carol@example.org", "one"))
	z := next(msg("z@x", "y@x", "carol@example.org", "Re: two"))
	y := next(msg("y@x", "x@x", "carol@example.org", "Re: one"))
	// Human mail without reply headers: subject+sender fallback.
	h1 := next(msg("", "", "dave@example.org", "Re: plans"))
	h2 := next(msg("", "", "dave@example.org", "RE: Plans"))
	// Machine mail with the same subject and sender stays apart.
	m1 := next(msg("", "", "noreply@example.net", "Your code is 123456"))
	m2 := next(msg("", "", "noreply@example.net", "Your code is 123456"))
	e.in.Wait()

	th := map[string]string{}
	for _, r := range []string{child, parent, x, y, z, h1, h2, m1, m2} {
		_, th[r] = e.messageOf(t, r)
	}
	same := func(a, b string, want bool) {
		t.Helper()
		if (th[a] == th[b]) != want {
			t.Errorf("thread(%s)=%s thread(%s)=%s, same=%v wanted", a, th[a], b, th[b], want)
		}
	}
	same(child, parent, true)
	same(x, y, true)
	same(y, z, true)
	same(x, parent, false)
	same(h1, h2, true)
	same(m1, m2, false)
	if n := e.count(t, `SELECT count(*) FROM threads`); n != 5 {
		t.Errorf("%d threads, want 5", n)
	}
	var n, parts int
	if err := e.pool.QueryRow(t.Context(), `SELECT n, cardinality(participants) FROM threads WHERE id = $1`, th[x]).Scan(&n, &parts); err != nil || n != 3 || parts != 1 {
		t.Errorf("bridged thread n=%d participants=%d %v", n, parts, err)
	}

	// Rebuilding from scratch agrees with the incremental result.
	st, err := e.in.cfg.DB.Rethread(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.Moved != 0 || st.Threads != 5 {
		t.Errorf("rethread %+v, want nothing moved", st)
	}
}

func TestQuarantineKeepsMail(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	m := e.ingest(t, 0, "random123@example.com", eml(t, "malformed-header.eml"))
	id, _ := e.messageOf(t, m.ID)
	var subject string
	var problem *string
	if err := e.pool.QueryRow(t.Context(), `SELECT subject, parse_error FROM messages WHERE id = $1`, id).Scan(&subject, &problem); err != nil {
		t.Fatal(err)
	}
	if problem == nil || subject != "You have won" {
		t.Fatalf("subject %q problem %v: want quarantined with what could be read", subject, problem)
	}
	res, err := e.in.cfg.DB.Search(t.Context(), &auth.Viewer{}, store.SearchQuery{Filter: storetest.Query(t, "prize"), IncludeDowngraded: true})
	if err != nil || res.Total != 1 || !res.Hits[0].Quarantined {
		t.Fatalf("search %+v %v", res, err)
	}
}

// M0 committed deliveries without messages; the backlog pass parses them
// after the upgrade and notifies nobody.
func TestBacklogAfterUpgrade(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.claim(t, "a")
	for i, name := range []string{"plain-otp.eml", "gb2312-otp.eml", "dsn-report.eml"} {
		body := eml(t, name)
		m := e.marker(e.now.Add(time.Duration(i)*time.Second), "s@example.net", "a@example.com", body)
		if err := e.local.Put(m.RawKey, body); err != nil {
			t.Fatal(err)
		}
		e.raw.Put(m.RawKey, body, nil)
		acct, err := e.in.account(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		_, err = e.in.cfg.DB.CommitIngest(t.Context(), store.Ingest{
			RawID: m.ID, R2Key: m.RawKey, LocalPath: m.RawKey, Size: m.RawSize,
			ContentHash: contenthash.V1(body), ReceivedAt: m.ReceivedAt, DeliveryID: m.ID,
			AccountID: acct, Zone: "example.com", Prefix: "a", EnvelopeTo: "a@example.com",
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// One local copy lost: the raw bucket serves it.
	var key string
	if err := e.pool.QueryRow(t.Context(), `SELECT local_path FROM raw_objects ORDER BY id LIMIT 1`).Scan(&key); err != nil {
		t.Fatal(err)
	}
	path, _ := e.local.Path(key)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	st, err := e.in.ProcessBacklog(t.Context())
	if err != nil || st != (BacklogStats{Processed: 3}) {
		t.Fatalf("backlog %+v %v", st, err)
	}
	e.in.Wait()
	if got := e.notes.got(); len(got) != 0 {
		t.Fatalf("backlog notified %q", got)
	}
	if n := e.count(t, `SELECT count(*) FROM deliveries WHERE message_id IS NULL`); n != 0 {
		t.Fatalf("%d deliveries left without a message", n)
	}
	res, err := e.in.cfg.DB.Search(t.Context(), &auth.Viewer{}, store.SearchQuery{Filter: storetest.Query(t, "验证码")})
	if err != nil || res.Total != 1 || res.Hits[0].Subject != "【示例】验证码通知" {
		t.Fatalf("search %+v %v", res, err)
	}
}

// reparse rewrites derived columns in place and touches no decision.
func TestReparse(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	old := e.ingest(t, -48*time.Hour, "alice@example.com", eml(t, "alternative-reply.eml"))
	recent := e.ingest(t, 0, "news@example.com", eml(t, "html-only-newsletter.eml"))
	oldID, oldThread := e.messageOf(t, old.ID)
	recentID, _ := e.messageOf(t, recent.ID)

	// Decisions a person made since.
	for _, q := range []string{
		`INSERT INTO message_state (message_id, labels, class_override, pinned) VALUES ($1, '{keep}', 1, true)`,
		`UPDATE prefixes SET state = 1, label = 'newsletters' WHERE prefix = 'news' AND $1 <> ''`,
		`INSERT INTO message_aliases (old_id, new_id) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5FAV', $1)`,
	} {
		if _, err := e.pool.Exec(t.Context(), q, oldID); err != nil {
			t.Fatal(err)
		}
	}
	// What an older parser left behind.
	if _, err := e.pool.Exec(t.Context(), `
		UPDATE messages SET subject = 'stale', body_text = '', parser_version = 0;
		UPDATE deliveries SET auth = NULL`); err != nil {
		t.Fatal(err)
	}
	before := e.count(t, `SELECT count(*) FROM changes`)

	// --since covers only the recent message.
	st, err := e.in.Reparse(t.Context(), e.now.Add(-time.Hour))
	if err != nil || st.Messages != 1 || st.Changed != 1 || st.Failed != 0 {
		t.Fatalf("reparse --since: %+v %v", st, err)
	}
	var subject string
	if err := e.pool.QueryRow(t.Context(), `SELECT subject FROM messages WHERE id = $1`, oldID).Scan(&subject); err != nil || subject != "stale" {
		t.Fatalf("old message rebuilt despite --since: %q %v", subject, err)
	}

	st, err = e.in.Reparse(t.Context(), time.Time{})
	if err != nil || st.Messages != 2 || st.Failed != 0 {
		t.Fatalf("reparse: %+v %v", st, err)
	}
	for id, raw := range map[string][]byte{oldID: eml(t, "alternative-reply.eml"), recentID: eml(t, "html-only-newsletter.eml")} {
		var subj string
		var version int
		if err := e.pool.QueryRow(t.Context(), `SELECT subject, parser_version FROM messages WHERE id = $1`, id).Scan(&subj, &version); err != nil {
			t.Fatal(err)
		}
		if want := mimeparse.Parse(raw).Subject; subj != want || version != mimeparse.Version {
			t.Errorf("%s: subject %q v%d, want %q v%d", id, subj, version, want, mimeparse.Version)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM deliveries WHERE auth IS NULL`); n != 0 {
		t.Errorf("%d deliveries without auth after reparse", n)
	}
	if _, th := e.messageOf(t, old.ID); th != oldThread {
		t.Errorf("thread id changed %s -> %s", oldThread, th)
	}
	if n := e.count(t, `SELECT count(*) FROM message_state WHERE message_id = $1 AND labels = '{keep}' AND class_override = 1 AND pinned`, oldID); n != 1 {
		t.Error("message_state touched")
	}
	if n := e.count(t, `SELECT count(*) FROM prefixes WHERE prefix = 'news' AND state = 1 AND label = 'newsletters'`); n != 1 {
		t.Error("prefix decision touched")
	}
	if n := e.count(t, `SELECT count(*) FROM message_aliases`); n != 1 {
		t.Error("message_aliases touched")
	}
	if e.count(t, `SELECT count(*) FROM changes`) <= before {
		t.Error("reparse logged no changes")
	}
	// A second reparse of unchanged content changes nothing.
	before = e.count(t, `SELECT count(*) FROM changes`)
	st, err = e.in.Reparse(t.Context(), time.Time{})
	if err != nil || st.Changed != 0 || st.Threads.Moved != 0 {
		t.Fatalf("idempotent reparse: %+v %v", st, err)
	}
	if after := e.count(t, `SELECT count(*) FROM changes`); after != before {
		t.Errorf("idempotent reparse logged %d changes", after-before)
	}
}

// Messages stored before migration 0007 have no recipients yet, and
// before 0016 none of their names. The backlog pass fills them from the
// raw, without a reparse, and what it writes is what a reparse writes.
func TestRecipientsAfterUpgrade(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	hdr := "From: Bob <bob@example.net>\r\nSubject: s\r\nCc: Carol <Carol@Example.org>, dan@example.net\r\n" +
		"Reply-To: list@example.net\r\n"
	a := e.ingest(t, 0, "a@example.com", mail("a@example.com", "pass", hdr+"Message-ID: <1@x>\r\n", "one"))
	b := e.ingest(t, time.Second, "b@example.com", mail("b@example.com", "pass", hdr+"Message-ID: <2@x>\r\n", "two"))
	// A malformed list costs only its broken elements, and never the mail.
	c := e.ingest(t, 2*time.Second, "c@example.com", mail("c@example.com", "pass",
		"From: s@example.net\r\nSubject: broken\r\nCc: @@, , =?bogus?= <ok@example.net>, \"unterminated <x@example.net>\r\n"+
			"Reply-To: <<<>>>\r\n", "three"))
	type row struct {
		to, cc, replyTo  []string
		toNames, ccNames []string
		version          int
	}
	read := func(raw string) row {
		t.Helper()
		id, _ := e.messageOf(t, raw)
		var r row
		if err := e.pool.QueryRow(t.Context(), `SELECT to_addrs, cc_addrs, reply_to_addrs, to_names, cc_names, parser_version
			FROM messages WHERE id = $1`, id).Scan(&r.to, &r.cc, &r.replyTo, &r.toNames, &r.ccNames, &r.version); err != nil {
			t.Fatal(err)
		}
		return r
	}
	want := map[string]row{
		a.ID: {[]string{"a@example.com"}, []string{"carol@example.org", "dan@example.net"}, []string{"list@example.net"}, []string{}, []string{"Carol"}, mimeparse.Version},
		b.ID: {[]string{"b@example.com"}, []string{"carol@example.org", "dan@example.net"}, []string{"list@example.net"}, []string{}, []string{"Carol"}, mimeparse.Version},
		c.ID: {[]string{"c@example.com"}, []string{"ok@example.net"}, []string{}, []string{}, []string{"=?bogus?="}, mimeparse.Version},
	}
	check := func(when string) {
		t.Helper()
		for raw, w := range want {
			if r := read(raw); !slices.Equal(r.to, w.to) || !slices.Equal(r.cc, w.cc) || !slices.Equal(r.replyTo, w.replyTo) ||
				r.replyTo == nil || !slices.Equal(r.toNames, w.toNames) || r.toNames == nil || !slices.Equal(r.ccNames, w.ccNames) ||
				r.version != w.version {
				t.Errorf("%s: %s = %+v, want %+v", when, raw, r, w)
			}
		}
	}
	check("ingest")

	// What the M2 binary left behind.
	if _, err := e.pool.Exec(t.Context(), `UPDATE messages SET to_addrs = NULL, cc_addrs = NULL, reply_to_addrs = NULL,
		to_names = NULL, cc_names = NULL, parser_version = 1`); err != nil {
		t.Fatal(err)
	}
	st, err := e.in.ProcessBacklog(t.Context())
	if err != nil || st != (BacklogStats{Processed: 3}) {
		t.Fatalf("backlog %+v %v", st, err)
	}
	check("backlog")
	if st, err := e.in.ProcessBacklog(t.Context()); err != nil || st != (BacklogStats{}) {
		t.Fatalf("second backlog %+v %v", st, err)
	}
	e.in.Wait()
	if got := e.notes.got(); len(got) != 0 {
		t.Fatalf("backlog notified %q", got)
	}
	rs, err := e.in.Reparse(t.Context(), time.Time{})
	if err != nil || rs.Messages != 3 || rs.Changed != 0 || rs.Failed != 0 {
		t.Fatalf("reparse after the backlog: %+v %v", rs, err)
	}
	check("reparse")
}

// Messages stored before migration 0008, or by an older extractor, are
// re-extracted by the backlog pass from their raw; what it writes is
// what ingest wrote, and a reparse afterwards finds nothing to change.
func TestExtractionsAfterUpgrade(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for i, name := range []string{"plain-otp.eml", "gb2312-otp.eml", "html-only-newsletter.eml"} {
		e.ingest(t, time.Duration(i)*time.Second, "a@example.com", eml(t, name))
	}
	snapshot := func() string {
		t.Helper()
		var s string
		if err := e.pool.QueryRow(t.Context(), `SELECT coalesce(string_agg(
			concat_ws('|', message_id, ord, kind, value, confidence, source, expires_at), ',' ORDER BY message_id, ord), '')
			FROM extractions`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	e.in.Wait()
	notified := len(e.notes.got())
	ingested := snapshot()
	if !strings.Contains(ingested, "|otp|482913|95|subject") || !strings.Contains(ingested, "|otp|739201|95|body") ||
		strings.Count(ingested, "|expiry|") != 2 {
		t.Fatalf("extractions after ingest: %s", ingested)
	}

	// What the M2 binary left behind.
	if _, err := e.pool.Exec(t.Context(), `DELETE FROM extractions`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(t.Context(), `UPDATE messages SET extract_version = 0`); err != nil {
		t.Fatal(err)
	}
	st, err := e.in.ProcessBacklog(t.Context())
	if err != nil || st != (BacklogStats{Processed: 3}) {
		t.Fatalf("backlog %+v %v", st, err)
	}
	if got := snapshot(); got != ingested {
		t.Errorf("backlog wrote\n%s\nwant\n%s", got, ingested)
	}
	if st, err := e.in.ProcessBacklog(t.Context()); err != nil || st != (BacklogStats{}) {
		t.Fatalf("second backlog %+v %v", st, err)
	}
	e.in.Wait()
	if got := e.notes.got(); len(got) != notified {
		t.Fatalf("backlog notified %q", got[notified:])
	}
	rs, err := e.in.Reparse(t.Context(), time.Time{})
	if err != nil || rs.Messages != 3 || rs.Changed != 0 || rs.Failed != 0 {
		t.Fatalf("reparse after the backlog: %+v %v", rs, err)
	}
}

// Too many addresses in one field: the list is cut short and the cut
// logged once, but the mail is not quarantined; its content is all there.
func TestAddressCap(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var logged bytes.Buffer
	e.in.cfg.Log = slog.New(slog.NewTextHandler(&logged, nil))
	cc := ""
	for i := range 1500 {
		cc += fmt.Sprintf("u%d@example.net, ", i)
	}
	m := e.ingest(t, 0, "a@example.com", mail("a@example.com", "pass",
		"From: s@example.net\r\nSubject: many\r\nCc: "+cc+"z@example.net\r\n", "body"))
	id, _ := e.messageOf(t, m.ID)
	var n int
	var problem *string
	if err := e.pool.QueryRow(t.Context(), `SELECT cardinality(cc_addrs), parse_error FROM messages WHERE id = $1`, id).
		Scan(&n, &problem); err != nil {
		t.Fatal(err)
	}
	if n != 1000 {
		t.Errorf("cc_addrs holds %d addresses, want the first 1000", n)
	}
	if problem != nil {
		t.Errorf("quarantined for a long Cc: %q", *problem)
	}
	if got := strings.Count(logged.String(), "address list cut short"); got != 1 {
		t.Errorf("logged the cut %d times:\n%s", got, logged.String())
	}
}

func TestChangesLogged(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.ingest(t, 0, "alice@example.com", eml(t, "plain-otp.eml"))
	rows, err := e.pool.Query(t.Context(), `SELECT kind FROM changes ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, k)
	}
	for _, k := range []string{"prefix", "thread", "delivery", "message"} {
		if !slices.Contains(kinds, k) {
			t.Errorf("no %s change in %v", k, kinds)
		}
	}
}
