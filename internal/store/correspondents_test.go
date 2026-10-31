package store_test

import (
	"crypto/sha256"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// threadMail is a message in the thread of <p1@example.net>; auth is the
// trusted receiver's DMARC result, "" for none.
func threadMail(from, cc, replyTo, id, auth string) string {
	ar := ""
	if auth != "" {
		ar = "Authentication-Results: mx.cloudflare.net; dmarc=" + auth + " header.from=example.net\n"
	}
	ref := ""
	if id != "p1" {
		ref = "In-Reply-To: <p1@example.net>\n"
	}
	return "\n" + ar + "From: " + from + "\nTo: github@example.com\nCc: " + cc + "\nReply-To: " + replyTo +
		"\nSubject: Plans\nMessage-ID: <" + id + "@example.net>\n" + ref +
		"Date: Mon, 2 Nov 2026 10:00:00 +0000\n\nWhat do you think?\n"
}

// Who corresponds in a thread (§8.9): its senders, and whom its mail was
// addressed to as far as the headers can be trusted. recipient_scope
// thread lets a reply go to them and to nobody else.
func TestThreadCorrespondents(t *testing.T) {
	t.Parallel()
	e := newSendEnv(t)
	ctx := t.Context()
	m1 := e.deliver(threadMail("Bob <bob@example.net>",
		"Carol <Carol@Example.org>, undisclosed-recipients:;, not an address", "List <list@example.net>", "p1", "pass"), "github")
	if _, err := e.pool.Exec(ctx, `UPDATE prefixes SET state = 1 WHERE zone = 'example.com' AND prefix = 'github'`); err != nil {
		t.Fatal(err)
	}
	// Downgraded (§4.3): an unclaimed prefix, DMARC passing or not.
	e.deliver(threadMail("mallory@example.net", "mallory-cc@example.net", "mallory-reply@example.net", "p2", "pass"), "random1")
	// Claimed but unverified: no DMARC result at all.
	e.deliver(threadMail("dora@example.net", "frank@example.net", "dora-reply@example.net", "p3", ""), "github")
	// Claimed, but DMARC failed: downgraded.
	e.deliver(threadMail("eve@example.net", "eve-cc@example.net", "eve-reply@example.net", "p4", "fail"), "github")

	var to, cc, replyTo []string
	if err := e.pool.QueryRow(ctx, `SELECT to_addrs, cc_addrs, reply_to_addrs FROM messages WHERE id = $1`, m1).
		Scan(&to, &cc, &replyTo); err != nil {
		t.Fatal(err)
	}
	// Folded; the empty group and the broken element are skipped.
	if !slices.Equal(to, []string{"github@example.com"}) || !slices.Equal(cc, []string{"carol@example.org"}) ||
		!slices.Equal(replyTo, []string{"list@example.net"}) {
		t.Fatalf("to %q cc %q reply-to %q", to, cc, replyTo)
	}

	correspondents := func() []string {
		t.Helper()
		rf, err := e.db.ReplyFacts(ctx, e.arisu, m1)
		if err != nil {
			t.Fatal(err)
		}
		return rf.Correspondents
	}
	// Every sender counts. Cc counts unless downgraded (frank, not
	// mallory-cc or eve-cc); Reply-To only with DMARC passing on top (list,
	// not dora-reply).
	want := []string{
		"bob@example.net", "carol@example.org", "dora@example.net", "eve@example.net", "frank@example.net",
		"github@example.com", "list@example.net", "mallory@example.net",
	}
	if got := correspondents(); !slices.Equal(got, want) {
		t.Fatalf("correspondents %q, want %q", got, want)
	}

	// Our own reply in the thread names dan, but its only outbox row was
	// withdrawn: it reached nobody, so neither dan nor its sender
	// correspond. Once a row of it is sent, both do.
	reply := letter("arisu@example.com", "Dan <dan@example.net>", "Re: Plans",
		"Message-ID: <r1@example.com>\r\nIn-Reply-To: <p1@example.net>\r\n", "Let me ask Dan.")
	rawID, outboxID := enqueue(t, e.pool, e.arisuID.ID, reply, e.at())
	if f := foldSent(t, e.db, rawID, reply); !f.NewMessage {
		t.Fatalf("sent copy folded into an existing message: %+v", f)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE outbox SET state = 'withdrawn', next_retry_at = NULL, approved_by = NULL
		WHERE id = $1`, outboxID); err != nil {
		t.Fatal(err)
	}
	if got := correspondents(); !slices.Equal(got, want) {
		t.Errorf("withdrawn reply: correspondents %q, want %q", got, want)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE outbox SET state = 'sent', approved_by = $2 WHERE id = $1`,
		outboxID, store.SystemPrincipalID); err != nil {
		t.Fatal(err)
	}
	want = append([]string{"arisu@example.com"}, want...)
	want = slices.Insert(want, slices.Index(want, "dora@example.net"), "dan@example.net")
	if got := correspondents(); !slices.Equal(got, want) {
		t.Errorf("sent reply: correspondents %q, want %q", got, want)
	}

	// What the policy makes of it: arisu may reply in the thread on its
	// own, to the copied and the Reply-To address alike, not beyond.
	at := e.at()
	if err := e.db.SetGrant(ctx, store.Grant{
		IdentityID: e.arisuID.ID, PrincipalID: e.arisu.PrincipalID, Policy: sendpolicy.Auto,
		RecipientScope: sendpolicy.ScopeThread,
	}, store.Action{ID: ulid.New(at), Tool: "grant.set", At: at}); err != nil {
		t.Fatal(err)
	}
	decide := func(rcpts ...string) sendpolicy.Decision {
		t.Helper()
		f, err := e.db.PolicyInput(ctx, e.arisu, store.Submission{
			FromAddr: "arisu@example.com", Recipients: rcpts, InReplyToID: m1, Now: e.at(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return sendpolicy.Decide(f.Input)
	}
	if d := decide("CAROL@example.org", "list@example.net", "frank@example.net"); !d.Queue {
		t.Errorf("reply to the copied and Reply-To: %+v", d)
	}
	for _, outsider := range []string{"mallory-cc@example.net", "eve-reply@example.net", "dora-reply@example.net"} {
		if d := decide("carol@example.org", outsider); d.Queue || d.HoldReason != sendpolicy.HoldRecipients {
			t.Errorf("reply to %s: %+v", outsider, d)
		}
	}

	// A row written before 0007 and not yet rebuilt adds its sender only,
	// the rule before, and is what the backlog pass picks up.
	if _, err := e.pool.Exec(ctx, `UPDATE messages SET to_addrs = NULL, cc_addrs = NULL, reply_to_addrs = NULL
		WHERE id = $1`, m1); err != nil {
		t.Fatal(err)
	}
	want = slices.DeleteFunc(want, func(a string) bool { return a == "carol@example.org" || a == "list@example.net" })
	if got := correspondents(); !slices.Equal(got, want) {
		t.Errorf("unread recipients: correspondents %q, want %q", got, want)
	}
	pending, err := e.db.UnreadRecipients(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].MessageID != m1 || len(pending[0].Deliveries) != 1 {
		t.Errorf("unread recipients %+v, %v", pending, err)
	}
	// The three columns are written together.
	if _, err := e.pool.Exec(ctx, `UPDATE messages SET to_addrs = '{}' WHERE id = $1`, m1); err == nil {
		t.Error("to_addrs written without cc_addrs and reply_to_addrs")
	}
}

// Upgrading to 0007: messages already stored keep NULL recipients, which
// the backlog pass (internal/ingest) lists and rebuilds; the migration
// cannot fill them, since that takes parsing the raw.
func TestMigrateLeavesRecipientsUnread(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	ownerURL, _ := storetest.URLs(t)
	pool, err := pgxpool.New(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	db := store.New(pool)
	if _, to, err := db.MigrateTo(ctx, 6); err != nil || to != 6 {
		t.Fatalf("migrate to 6: %d, %v", to, err)
	}
	// A delivery and its message as the M2 schema has them.
	acct, err := db.EnsureCloudflareAccount(ctx, "cloudflare")
	if err != nil {
		t.Fatal(err)
	}
	in := record(acct, 1, sha256.Sum256([]byte("m2 content")), "a@example.com")
	if _, err := db.CommitIngest(ctx, in); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO threads (id, subject_norm, participants, first_at, last_at, n, class)
		 SELECT $1, 'hi', '{bob@example.net}', received_at, received_at, 1, 2 FROM raw_objects WHERE id = $2`,
		`INSERT INTO messages (id, canonical_raw_id, content_hash, thread_id, received_at, subject, snippet,
			from_addr, from_name, class, has_attach, body_text, parser_version)
		 SELECT $1, r.id, r.content_hash, $1, r.received_at, 'Hi', 'hi', 'bob@example.net', 'Bob', 2, false, 'hi', 1
		 FROM raw_objects r WHERE r.id = $2`,
		`UPDATE deliveries SET message_id = $1 WHERE raw_id = $2`,
	} {
		if _, err := pool.Exec(ctx, q, in.DeliveryID, in.RawID); err != nil {
			t.Fatal(err)
		}
	}

	if from, to, err := db.Migrate(ctx); err != nil || from != 6 || to < 7 {
		t.Fatalf("migrate: %d -> %d, %v", from, to, err)
	}
	if n := count(t, pool, `SELECT count(*) FROM messages
		WHERE to_addrs IS NULL AND cc_addrs IS NULL AND reply_to_addrs IS NULL`); n != 1 {
		t.Fatalf("%d messages with unread recipients, want 1", n)
	}
	pending, err := db.UnreadRecipients(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].MessageID != in.DeliveryID ||
		len(pending[0].Deliveries) != 1 || pending[0].Deliveries[0].RawID != in.RawID {
		t.Fatalf("unread recipients %+v, %v", pending, err)
	}
	// Until the backlog pass gets to it, it adds its sender only.
	v := &auth.Viewer{PrincipalID: store.SystemPrincipalID, PrincipalKind: "human", PrincipalName: "operator"}
	rf, err := db.ReplyFacts(ctx, v, in.DeliveryID)
	if err != nil || !slices.Equal(rf.Correspondents, []string{"bob@example.net"}) {
		t.Fatalf("reply facts %+v, %v", rf, err)
	}
}
