package store_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/extract"
	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// reviewFacts decides a submission by arisu (review + any) to bob with
// content c at now and returns the facts the review read.
func (e *sendEnv) reviewFacts(v *auth.Viewer, c store.ReviewInput, replyTo string, now time.Time) (*sendpolicy.ReviewFacts, sendpolicy.Decision) {
	e.t.Helper()
	var f *sendpolicy.ReviewFacts
	var d sendpolicy.Decision
	err := e.db.WithDecision(e.t.Context(), v, store.Submission{
		FromAddr: "arisu@example.com", Recipients: []string{"bob@example.net"}, InReplyToID: replyTo, Now: now, Content: &c,
	}, func(ctx context.Context, p *store.PolicyTx) error {
		f, d = p.Facts().Input.Review, p.Decision()
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return f, d
}

func tokens(where string, text string) []store.ReviewItem {
	var out []store.ReviewItem
	for _, t := range extract.Tokens(text) {
		out = append(out, store.ReviewItem{Value: t, Where: where})
	}
	return out
}

// The facts of the review tier, as the submitter sees the mailbox.
func TestReviewFacts(t *testing.T) {
	t.Parallel()
	e := newSendEnv(t)
	ctx := t.Context()
	e.knowBob()
	e.reviewGrant(e.arisuID, 600)

	// A code that came in, and a sign-in link from an authenticated
	// sender on a claimed prefix.
	otp := e.deliver(`
Authentication-Results: mx.cloudflare.net; dmarc=pass header.from=example.net
From: Service <no-reply@login.example.net>
To: github@example.com
Subject: Your verification code
Message-ID: <otp1@example.net>

Your verification code is 740219.
Sign in: https://login.example.net/verify?token=abc123def456
`, "github")
	if _, err := e.pool.Exec(ctx, `UPDATE prefixes SET state = 1 WHERE zone = 'example.com' AND prefix = 'github'`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM extractions WHERE message_id = $1 AND kind IN ('otp', 'link')`, otp).Scan(&n); err != nil || n != 2 {
		t.Fatalf("%d extractions of the fixture: %v", n, err)
	}
	now := e.at()

	// Nothing to find: held, the facts empty, every check passed.
	f, d := e.reviewFacts(e.arisu, store.ReviewInput{Tokens: tokens("body", "Thanks, see you on Friday.")}, "", now)
	if f == nil || len(f.Extracted)+len(f.Refs)+len(f.Links)+len(f.Unreadable) != 0 || !d.Held {
		t.Fatalf("clean mail: %+v %+v", f, d)
	}

	// The code, spaced out, in the body; the link in an attachment.
	f, d = e.reviewFacts(e.arisu, store.ReviewInput{Tokens: append(tokens("body", "it is 740 219"),
		tokens("attachment 2 (a.txt)", "https://login.example.net/verify?token=abc123def456")...)}, "", now)
	if len(f.Extracted) != 2 || d.Held || d.HoldReason != sendpolicy.HoldReview {
		t.Fatalf("leak: %+v %+v", f.Extracted, d)
	}
	for _, x := range f.Extracted {
		if x.MessageID != otp || (x.Kind == "otp") != (x.Where == "body") {
			t.Errorf("finding %+v", x)
		}
	}
	// The verdict says where, never the value.
	if b := d.Review; b == nil || slices.ContainsFunc(b.Checks, func(c sendpolicy.Check) bool {
		return c.Name == sendpolicy.CheckExtracted && c.Pass
	}) {
		t.Errorf("verdict %+v", b)
	}

	// Beyond the lookback the code no longer counts.
	if f, _ := e.reviewFacts(e.arisu, store.ReviewInput{Tokens: tokens("body", "740219")}, "", now.Add(31*24*time.Hour)); len(f.Extracted) != 0 {
		t.Errorf("lookback: %+v", f.Extracted)
	}

	// Our own sent copies' values do not count: they have no delivery.
	sent := []byte("From: arisu@example.com\r\nTo: bob@example.net\r\nSubject: Your verification code\r\n" +
		"Message-ID: <s1@example.com>\r\n\r\nYour verification code is 135792.\r\n")
	rawID, _ := enqueue(t, e.pool, e.arisuID.ID, sent, e.at())
	foldSent(t, e.db, rawID, sent)
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM extractions WHERE value = '135792'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("sent copy extractions: %d %v", n, err)
	}
	if f, _ := e.reviewFacts(e.arisu, store.ReviewInput{Tokens: tokens("body", "135792")}, "", now); len(f.Extracted) != 0 {
		t.Errorf("own sent copy: %+v", f.Extracted)
	}

	// Link domains: ours, an authenticated sender's (and its
	// subdomains), a recipient's; unknown, an IP, an IDN in either form.
	f, _ = e.reviewFacts(e.arisu, store.ReviewInput{Hosts: []store.ReviewItem{
		{Value: "www.example.com", Where: "body"},
		{Value: "docs.example.net", Where: "body"},
		{Value: "EXAMPLE.NET.", Where: "body"},
		{Value: "evil.example", Where: "body"},
		{Value: "192.0.2.1", Where: "body"},
		{Value: "bücher.example", Where: "body"},
	}}, "", now)
	seen := map[string]string{}
	for _, l := range f.Links {
		seen[l.Host] = l.Seen
	}
	want := map[string]string{
		"www.example.com": "identity", "docs.example.net": "inbound", "example.net": "inbound",
		"evil.example": "", "192.0.2.1": "", "xn--bcher-kva.example": "",
	}
	if len(seen) != len(want) {
		t.Errorf("links %+v", f.Links)
	}
	for h, w := range want {
		if got, ok := seen[h]; !ok || got != w {
			t.Errorf("link %s: seen %q (%v), want %q", h, got, ok, w)
		}
	}

	// References: one visible message of another thread, one that does
	// not exist, one by Message-ID.
	f, _ = e.reviewFacts(e.arisu, store.ReviewInput{Refs: []store.ReviewRef{
		{As: "yuuka://message/" + otp, Where: "cites", MessageID: otp},
		{As: "yuuka://message/01JZZZZZZZZZZZZZZZZZZZZZZZ", Where: "body", MessageID: "01JZZZZZZZZZZZZZZZZZZZZZZZ"},
		{As: "<otp1@example.net>", Where: "attachment 2", HeaderID: "otp1@example.net"},
	}}, "", now)
	if len(f.Refs) != 3 || f.Refs[0].MessageID != otp || f.Refs[0].ThreadID == "" || f.Refs[1].MessageID != "" ||
		f.Refs[2].MessageID != otp || !slices.Contains(f.Refs[0].Correspondents, "no-reply@login.example.net") {
		t.Errorf("refs %+v", f.Refs)
	}

	// Too many candidates: unreadable, not looked up.
	many := make([]store.ReviewItem, extract.MaxTokens+1)
	for i := range many {
		many[i] = store.ReviewItem{Value: "x", Where: "body"}
	}
	if f, _ := e.reviewFacts(e.arisu, store.ReviewInput{Tokens: many}, "", now); len(f.Unreadable) != 1 {
		t.Errorf("too many: %+v", f.Unreadable)
	}

	// No facts are gathered where the review does not run: auto, or a
	// person confirming.
	e.op.Scopes = auth.AllScopes
	var none *sendpolicy.ReviewFacts
	err := e.db.WithDecision(ctx, e.op, store.Submission{
		FromAddr: "github@example.com", Recipients: []string{"bob@example.net"}, Now: now, Content: &store.ReviewInput{},
	}, func(ctx context.Context, p *store.PolicyTx) error {
		none = p.Facts().Input.Review
		return nil
	})
	if err != nil || none != nil {
		t.Errorf("auto tier gathered review facts: %+v %v", none, err)
	}
}
