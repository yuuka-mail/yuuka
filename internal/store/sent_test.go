package store_test

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/contenthash"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// The outbox side is written with plain SQL: these tests only need its
// rows as the executor leaves them.

// identity creates an identity me@<zone> in account and returns its id.
func identity(t *testing.T, pool *pgxpool.Pool, account, zone string) string {
	t.Helper()
	id := ulid.New(time.Now())
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO identities (id, account_id, zone, prefix, transport, owner_id)
		VALUES ($1, $2, $3, 'me', 'cf', $4)`, id, account, zone, systemID); err != nil {
		t.Fatal(err)
	}
	return id
}

// enqueue stores raw as a sent copy, points a queued outbox row of
// identity at it as the enqueuer does, and returns both ids.
func enqueue(t *testing.T, pool *pgxpool.Pool, identity string, raw []byte, at time.Time) (rawID, outboxID string) {
	t.Helper()
	rawID = ulid.New(at)
	key := fmt.Sprintf("sent/%04d/%02d/%s.eml", at.Year(), int(at.Month()), rawID)
	sum, hash := sha256.Sum256(raw), contenthash.V1(raw)
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO raw_objects (id, r2_key, local_path, size, sha256, received_at, content_hash)
		VALUES ($1, $2, $2, $3, $4, $5, $6)`, rawID, key, len(raw), sum[:], at, hash[:]); err != nil {
		t.Fatal(err)
	}
	outboxID = ulid.New(at)
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO outbox (id, state, next_retry_at, identity_id, from_addr, submitted_by, approved_by, raw_id, created_at)
		VALUES ($1, 'queued', $2, $3, 'me@example.com', $4, $4, $5, $2)`, outboxID, at, identity, systemID, rawID); err != nil {
		t.Fatal(err)
	}
	return rawID, outboxID
}

// foldSent indexes a sent copy as the enqueuer's hook does.
func foldSent(t *testing.T, db *store.DB, rawID string, raw []byte) store.Folded {
	t.Helper()
	ref, err := db.SentCopy(t.Context(), rawID)
	if err != nil {
		t.Fatal(err)
	}
	f, err := db.FoldSent(t.Context(), ref, mimeparse.Parse(raw))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// attempt records a finished attempt of outbox with the ids the exit
// reported; "" is NULL.
func attempt(t *testing.T, pool *pgxpool.Pool, outbox, messageID, providerID string, at time.Time) {
	t.Helper()
	null := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO send_attempts (id, outbox_id, recipients, transport, owner, lease_until, started_at,
			committed_at, finished_at, outcome, message_id, provider_id)
		VALUES ($1, $2, '{bob@example.net}', 'cf', 'test', $3, $3, $3, $3, 'accepted', $4, $5)`,
		ulid.New(at), outbox, at, null(messageID), null(providerID)); err != nil {
		t.Fatal(err)
	}
}

// inbound commits and folds raw as delivered to prefix@example.com in
// account.
func inbound(t *testing.T, db *store.DB, account, prefix string, raw []byte, at time.Time) string {
	t.Helper()
	id := ulid.New(at)
	key := fmt.Sprintf("raw/2026/10/%s.eml", id)
	in := store.Ingest{
		RawID: id, R2Key: key, LocalPath: key, Size: int64(len(raw)), SHA256: sha256.Sum256(raw),
		ContentHash: contenthash.V1(raw), ReceivedAt: at, DeliveryID: id, AccountID: account,
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

// letter is a synthetic message. headers are extra fields, CRLF-ended.
func letter(from, to, subject, headers, body string) []byte {
	return fmt.Appendf(nil, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: Sat, 31 Oct 2026 12:00:00 +0000\r\n"+
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n%s\r\n%s\r\n", from, to, subject, headers, body)
}

func count(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A sent copy is a message like inbound mail: listed, searchable, in a
// thread, and readable from its raw, but never downgraded: it has no
// delivery that the §4.3 downgrade could judge.
func TestSentCopyIsAMessage(t *testing.T) {
	t.Parallel()
	db, pool := storetest.New(t)
	me := identity(t, pool, storetest.Account(t, db), "example.com")
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	// As composed for Cloudflare: no Message-ID, the platform assigns it.
	raw := letter("me@example.com", "bob@example.net", "Lunch on Friday", "", "Shall we meet at noon?")
	rawID, outbox := enqueue(t, pool, me, raw, at)

	f := foldSent(t, db, rawID, raw)
	if !f.NewMessage || f.Notify {
		t.Fatalf("fold %+v", f)
	}
	if n := count(t, pool, `SELECT count(*) FROM deliveries`); n != 0 {
		t.Fatalf("%d deliveries for a sent copy", n)
	}
	var canonical string
	var received time.Time
	if err := pool.QueryRow(t.Context(), `SELECT canonical_raw_id, received_at FROM messages WHERE id = $1`, f.MessageID).
		Scan(&canonical, &received); err != nil || canonical != rawID || !received.Equal(at) {
		t.Fatalf("canonical %s at %v (%v), want the sent copy at submission", canonical, received, err)
	}

	want := &store.Outgoing{OutboxID: outbox, State: "queued", From: "me@example.com"}
	list, err := db.ListMessages(t.Context(), operator, store.MessageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != f.MessageID || list[0].Downgraded || len(list[0].Recipients) != 0 ||
		list[0].Outgoing == nil || *list[0].Outgoing != *want {
		t.Fatalf("default list %+v", list)
	}
	res, err := db.Search(t.Context(), operator, store.SearchQuery{Text: "noon"})
	if err != nil || res.Total != 1 || res.Hits[0].Downgraded || res.Hits[0].Outgoing == nil || *res.Hits[0].Outgoing != *want {
		t.Fatalf("search %+v %v", res, err)
	}
	d, err := db.MessageByID(t.Context(), operator, f.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Deliveries) != 0 || d.Raw.RawID != rawID || d.Raw.DeliveryID != "" || d.Raw.R2Key == "" || d.Subject != "Lunch on Friday" {
		t.Fatalf("detail %+v", d)
	}
	th, err := db.ThreadByID(t.Context(), operator, d.ThreadID)
	if err != nil || len(th.Messages) != 1 || th.Messages[0].Outgoing == nil {
		t.Fatalf("thread %+v %v", th, err)
	}

	// The state is read live from the outbox, never stored on the message.
	if _, err := pool.Exec(t.Context(), `UPDATE outbox SET state = 'sent', next_retry_at = NULL WHERE id = $1`, outbox); err != nil {
		t.Fatal(err)
	}
	if d, _ := db.MessageByID(t.Context(), operator, f.MessageID); d.Outgoing == nil || d.Outgoing.State != "sent" {
		t.Errorf("after sending: %+v", d.Outgoing)
	}

	// Indexing again is a no-op.
	before := count(t, pool, `SELECT count(*) FROM changes`)
	if again := foldSent(t, db, rawID, raw); again.MessageID != f.MessageID || again.NewMessage {
		t.Errorf("second fold %+v", again)
	}
	if n := count(t, pool, `SELECT count(*) FROM changes`); n != before {
		t.Errorf("second fold logged %d changes", n-before)
	}
	if refs, err := db.UnindexedSent(t.Context(), 10); err != nil || len(refs) != 0 {
		t.Errorf("unindexed %+v %v", refs, err)
	}
}

// Only what an outbox row points at is a sent copy. A raw a draft holds
// (an SMTP submission stopped before the outbox) is not indexed yet.
func TestSentCopyNeedsOutbox(t *testing.T) {
	t.Parallel()
	db, pool := storetest.New(t)
	me := identity(t, pool, storetest.Account(t, db), "example.com")
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	raw := letter("me@example.com", "bob@example.net", "held", "", "held as a draft")
	rawID, _ := enqueue(t, pool, me, raw, at)
	if _, err := pool.Exec(t.Context(), `DELETE FROM outbox`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SentCopy(t.Context(), rawID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SentCopy of a raw without outbox: %v", err)
	}
	ref := store.RawRef{RawID: rawID, ContentHash: func() []byte { h := contenthash.V1(raw); return h[:] }(), ReceivedAt: at}
	if _, err := db.FoldSent(t.Context(), ref, mimeparse.Parse(raw)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("FoldSent of a raw without outbox: %v", err)
	}
	if refs, err := db.UnindexedSent(t.Context(), 10); err != nil || len(refs) != 0 {
		t.Errorf("unindexed %+v %v", refs, err)
	}
}

// Replies to our mail find its thread through what the exit told us:
// Cloudflare's provider id or a recorded Message-ID, matched in
// In-Reply-To or References, whichever arrives first, and rethreading
// from scratch agrees.
func TestRepliesThreadWithSentCopy(t *testing.T) {
	t.Parallel()
	db, pool := storetest.New(t)
	acct := storetest.Account(t, db)
	me := identity(t, pool, acct, "example.com")
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

	// 1. A Cloudflare send: no Message-ID in the sent copy; the reply
	// names the provider id from "250 Ok <...>".
	first := letter("me@example.com", "bob@example.net", "Lunch on Friday", "", "noon?")
	rawID, outbox := enqueue(t, pool, me, first, at)
	sent := foldSent(t, db, rawID, first).MessageID
	attempt(t, pool, outbox, "", "cf-1@example.com", at)
	reply := inbound(t, db, acct, "me", letter("bob@example.net", "me@example.com", "Re: Lunch on Friday",
		"Message-ID: <r1@example.net>\r\nIn-Reply-To: <cf-1@example.com>\r\n", "yes"), at.Add(time.Hour))

	// 2. A retry after partial gets a new platform id, recorded as the
	// attempt's message_id; a reply names it in References only.
	second := letter("me@example.com", "carol@example.net", "Plans", "", "Saturday?")
	rawID2, outbox2 := enqueue(t, pool, me, second, at.Add(time.Minute))
	sent2 := foldSent(t, db, rawID2, second).MessageID
	attempt(t, pool, outbox2, "", "cf-2@example.com", at.Add(time.Minute))
	attempt(t, pool, outbox2, "cf-3@example.com", "", at.Add(2*time.Minute))
	reply2 := inbound(t, db, acct, "me", letter("carol@example.net", "me@example.com", "Re: Plans",
		"Message-ID: <r2@example.net>\r\nReferences: <unrelated@example.org> <cf-3@example.com>\r\n", "sure"), at.Add(time.Hour))

	// 3. The reply arrives before the sent copy is indexed (the hook
	// failed; the backlog pass indexes it later): the sent copy finds it.
	third := letter("me@example.com", "dave@example.net", "Report", "", "attached soon")
	rawID3, outbox3 := enqueue(t, pool, me, third, at.Add(2*time.Minute))
	attempt(t, pool, outbox3, "", "cf-4@example.com", at.Add(2*time.Minute))
	reply3 := inbound(t, db, acct, "me", letter("dave@example.net", "me@example.com", "Re: Report",
		"Message-ID: <r3@example.net>\r\nIn-Reply-To: <cf-4@example.com>\r\n", "thanks"), at.Add(time.Hour))
	if refs, err := db.UnindexedSent(t.Context(), 10); err != nil || len(refs) != 1 || refs[0].RawID != rawID3 {
		t.Fatalf("unindexed %+v %v", refs, err)
	}
	sent3 := foldSent(t, db, rawID3, third).MessageID

	// 4. An exit that keeps our Message-ID: the sent copy carries it and
	// replies to an inbound message; both directions thread by headers.
	orig := inbound(t, db, acct, "me", letter("erin@example.net", "me@example.com", "Question",
		"Message-ID: <q@example.net>\r\n", "how?"), at)
	answer := letter("me@example.com", "erin@example.net", "Re: Question",
		"Message-ID: <a1@example.com>\r\nIn-Reply-To: <q@example.net>\r\nReferences: <q@example.net>\r\n", "like this")
	rawID4, _ := enqueue(t, pool, me, answer, at.Add(3*time.Minute))
	sent4 := foldSent(t, db, rawID4, answer).MessageID
	followUp := inbound(t, db, acct, "me", letter("erin@example.net", "me@example.com", "Re: Question",
		"Message-ID: <q2@example.net>\r\nIn-Reply-To: <a1@example.com>\r\n", "thanks"), at.Add(time.Hour))

	thread := func(id string) string {
		var th string
		if err := pool.QueryRow(t.Context(), `SELECT thread_id FROM messages WHERE id = $1`, id).Scan(&th); err != nil {
			t.Fatal(err)
		}
		return th
	}
	groups := [][]string{{sent, reply}, {sent2, reply2}, {sent3, reply3}, {orig, sent4, followUp}}
	check := func(when string) {
		t.Helper()
		seen := map[string]bool{}
		for _, g := range groups {
			th := thread(g[0])
			for _, m := range g[1:] {
				if thread(m) != th {
					t.Errorf("%s: %s in thread %s, want %s with %s", when, m, thread(m), th, g[0])
				}
			}
			if seen[th] {
				t.Errorf("%s: two conversations share thread %s", when, th)
			}
			seen[th] = true
		}
	}
	check("ingest")
	st, err := db.Rethread(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.Moved != 0 || st.Threads != len(groups) {
		t.Errorf("rethread %+v, want %d threads and nothing moved", st, len(groups))
	}
	check("rethread")

	// Rethread derives the links from send_attempts alone: with the
	// threads scrambled it puts every message back.
	if _, err := pool.Exec(t.Context(), `UPDATE messages SET thread_id = (SELECT min(id) FROM threads)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Rethread(t.Context()); err != nil {
		t.Fatal(err)
	}
	check("rethread from scratch")
}

// Visibility (§13.8) with account_access enforced: a message is visible
// through a delivery to a readable account, or as the sent copy of an
// outbox row whose identity's account is readable. Every read path runs
// over the same fixture; a restricted viewer may learn that hidden mail
// exists (a thread it is in), never its headers or body. New read paths
// join this table.
func TestVisibility(t *testing.T) {
	t.Parallel()
	db, pool := storetest.New(t)
	db.EnforceAccountAccess()
	ctx := t.Context()
	open, closed := storetest.Account(t, db), func() string {
		id, err := db.EnsureCloudflareAccount(ctx, "closed")
		if err != nil {
			t.Fatal(err)
		}
		return id
	}()
	mine, theirs := identity(t, pool, open, "example.com"), identity(t, pool, closed, "example.org")
	agent, err := db.EnsurePrincipal(ctx, store.Principal{ID: ulid.New(time.Now()), Kind: "agent", Name: "arisu", DisplayName: "arisu", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO account_access (account_id, principal_id, can_read) VALUES ($1, $3, true), ($2, $3, false)`,
		open, closed, agent.ID); err != nil {
		t.Fatal(err)
	}
	restricted := &auth.Viewer{PrincipalID: agent.ID, PrincipalKind: "agent", PrincipalName: "arisu"}
	nobody := &auth.Viewer{PrincipalID: ulid.New(time.Now()), PrincipalKind: "agent", PrincipalName: "nobody"}
	everyone := &auth.Viewer{PrincipalID: systemID, PrincipalKind: "human", PrincipalName: "operator"}
	if _, err := pool.Exec(ctx, `INSERT INTO account_access (account_id, principal_id, can_read) VALUES ($1, $3, true), ($2, $3, true)`,
		open, closed, systemID); err != nil {
		t.Fatal(err)
	}

	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	tick := func() time.Time { at = at.Add(time.Minute); return at }
	msgs := map[string]string{}
	msgs["in-open"] = inbound(t, db, open, "a", letter("x@example.net", "a@example.com", "in-open",
		"Message-ID: <in-open@example.net>\r\n", "visible inbound"), tick())
	msgs["in-closed"] = inbound(t, db, closed, "b", letter("x@example.net", "b@example.com", "in-closed", "Message-ID: <in-closed@example.net>\r\n", "hidden inbound"), tick())
	sendAs := func(name, identity, headers string) {
		raw := letter("me@example.com", "x@example.net", name, headers, name+" body")
		rawID, _ := enqueue(t, pool, identity, raw, tick())
		msgs[name] = foldSent(t, db, rawID, raw).MessageID
	}
	sendAs("sent-open", mine, "Message-ID: <sent-open@example.com>\r\n")
	// Hidden, in the same thread as in-open.
	sendAs("sent-closed", theirs, "In-Reply-To: <in-open@example.net>\r\nMessage-ID: <sent-closed@example.com>\r\n")
	// One content, delivered to the closed account and sent as an open
	// identity: visible through the sent copy only, so its delivery, raw
	// and recipients stay hidden.
	both := letter("me@example.com", "b@example.com", "both", "Message-ID: <both@example.com>\r\n", "both body")
	msgs["both"] = inbound(t, db, closed, "b", both, tick())
	bothRaw, _ := enqueue(t, pool, mine, both, tick())
	if f := foldSent(t, db, bothRaw, both); f.MessageID != msgs["both"] || f.NewMessage {
		t.Fatalf("sent copy did not fold into the delivered message: %+v", f)
	}

	// Someone only the hidden reply went to.
	if _, err := pool.Exec(ctx, `INSERT INTO outbox_recipients (outbox_id, addr, addr_fold, kind)
		SELECT so.id, 'hidden@example.org', 'hidden@example.org', 'to'
		FROM outbox so JOIN raw_objects r ON r.id = so.raw_id JOIN messages m ON m.content_hash = r.content_hash
		WHERE m.id = $1`, msgs["sent-closed"]); err != nil {
		t.Fatal(err)
	}

	visibleTo := map[*auth.Viewer][]string{
		everyone:   {"both", "in-closed", "in-open", "sent-closed", "sent-open"},
		restricted: {"both", "in-open", "sent-open"},
		nobody:     {},
	}
	names := func(ids []string) []string {
		out := []string{}
		for _, id := range ids {
			for n, m := range msgs {
				if m == id {
					out = append(out, n)
				}
			}
		}
		slices.Sort(out)
		return out
	}
	for v, want := range visibleTo {
		t.Run(v.PrincipalName, func(t *testing.T) {
			list, err := db.ListMessages(ctx, v, store.MessageQuery{MessageFilter: store.MessageFilter{Downgraded: store.IncludeDowngraded}})
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, m := range list {
				ids = append(ids, m.ID)
			}
			if got := names(ids); !slices.Equal(got, want) {
				t.Errorf("ListMessages %q, want %q", got, want)
			}

			res, err := db.Search(ctx, v, store.SearchQuery{Text: "body", IncludeDowngraded: true})
			if err != nil {
				t.Fatal(err)
			}
			ids = ids[:0]
			for _, h := range res.Hits {
				ids = append(ids, h.ID)
			}
			wantBodies := slices.DeleteFunc(slices.Clone(want), func(n string) bool { return n == "in-open" || n == "in-closed" })
			if got := names(ids); !slices.Equal(got, wantBodies) || res.Total != len(wantBodies) {
				t.Errorf("Search %q (total %d), want %q", got, res.Total, wantBodies)
			}

			for n, id := range msgs {
				d, err := db.MessageByID(ctx, v, id)
				if slices.Contains(want, n) != (err == nil) {
					t.Errorf("MessageByID %s: %v", n, err)
				}
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					t.Errorf("MessageByID %s: %v, want ErrNotFound", n, err)
				}
				if err == nil && d.Raw.RawID == "" {
					t.Errorf("MessageByID %s: no raw", n)
				}
			}
			if v == restricted {
				d, err := db.MessageByID(ctx, v, msgs["both"])
				if err != nil {
					t.Fatal(err)
				}
				if len(d.Deliveries) != 0 || len(d.Recipients) != 0 || d.Raw.RawID != bothRaw || d.Outgoing == nil {
					t.Errorf("both through its sent copy: %+v", d)
				}
			}

			// The thread of in-open holds sent-closed too; a viewer
			// that cannot read it sees the thread without it.
			var thread string
			if err := pool.QueryRow(ctx, `SELECT thread_id FROM messages WHERE id = $1`, msgs["sent-closed"]).Scan(&thread); err != nil {
				t.Fatal(err)
			}
			th, err := db.ThreadByID(ctx, v, thread)
			wantThread := slices.DeleteFunc([]string{"in-open", "sent-closed"}, func(n string) bool { return !slices.Contains(want, n) })
			if len(wantThread) == 0 {
				if !errors.Is(err, store.ErrNotFound) {
					t.Errorf("ThreadByID: %v, want ErrNotFound", err)
				}
			} else {
				ids = ids[:0]
				for _, m := range th.Messages {
					ids = append(ids, m.ID)
				}
				if got := names(ids); err != nil || !slices.Equal(got, wantThread) {
					t.Errorf("ThreadByID %q %v, want %q", got, err, wantThread)
				}
			}

			// The facts a reply is decided on (sendpolicy): the message
			// and the identities it came through only if visible, and
			// the thread's participants from visible mail only.
			for n, id := range msgs {
				_, err := db.ReplyFacts(ctx, v, id)
				if slices.Contains(want, n) != (err == nil) || err != nil && !errors.Is(err, store.ErrNotFound) {
					t.Errorf("ReplyFacts %s: %v", n, err)
				}
				_, err = db.ReplyIdentities(ctx, v, id)
				if slices.Contains(want, n) != (err == nil) || err != nil && !errors.Is(err, store.ErrNotFound) {
					t.Errorf("ReplyIdentities %s: %v", n, err)
				}
				// What an SMTP submission's In-Reply-To finds.
				var hid string
				if err := pool.QueryRow(ctx, `SELECT coalesce(message_id, '') FROM messages WHERE id = $1`, id).Scan(&hid); err != nil || hid == "" {
					t.Fatalf("Message-ID of %s: %q, %v", n, hid, err)
				}
				got, err := db.MessageByHeaderID(ctx, v, hid)
				if slices.Contains(want, n) != (err == nil && got == id) || err != nil && !errors.Is(err, store.ErrNotFound) {
					t.Errorf("MessageByHeaderID %s: %s, %v", n, got, err)
				}
			}
			if slices.Contains(want, "in-open") {
				rf, err := db.ReplyFacts(ctx, v, msgs["in-open"])
				if err != nil {
					t.Fatal(err)
				}
				if slices.Contains(rf.Participants, "hidden@example.org") != slices.Contains(want, "sent-closed") {
					t.Errorf("participants of in-open's thread: %q", rf.Participants)
				}
			}

			// Sent copies are no deliveries: the prefix view counts
			// the visible deliveries only.
			sum, err := db.PrefixStats(ctx, v, store.PrefixQuery{})
			if err != nil {
				t.Fatal(err)
			}
			wantDelivered := 0
			for _, n := range []string{"in-open", "in-closed", "both"} {
				if v == everyone || (v == restricted && n == "in-open") {
					wantDelivered++
				}
			}
			if sum.Messages != wantDelivered {
				t.Errorf("PrefixStats counts %d messages, want %d", sum.Messages, wantDelivered)
			}
		})
	}
}
