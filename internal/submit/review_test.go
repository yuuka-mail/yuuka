package submit

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yuuka-mail/yuuka/internal/notify"
	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/transport"
)

// fakeNotifier records the cards it is asked to send.
type fakeNotifier struct {
	mu        sync.Mutex
	held      []notify.HeldCard
	escalated []notify.EscalatedCard
}

func (f *fakeNotifier) Held(_ context.Context, c notify.HeldCard) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held = append(f.held, c)
	return nil
}

func (f *fakeNotifier) Escalated(_ context.Context, c notify.EscalatedCard) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.escalated = append(f.escalated, c)
	return errors.New("cards may fail; nothing waits on them")
}

// know makes addr an address the operator sent mail to, so it is no
// first contact.
func (e *env) know(addr string) {
	e.t.Helper()
	d, err := e.db.CreateDraft(e.t.Context(), e.op, store.Draft{FromAddr: "op@example.com", To: []store.Recipient{{Addr: addr}}, BodyMD: "hi"}, e.at())
	if err != nil {
		e.t.Fatal(err)
	}
	if r, err := e.svc.SubmitDraft(e.t.Context(), e.op, d.ID); err != nil || !r.Queued {
		e.t.Fatalf("know %s: %+v %v", addr, r, err)
	}
}

func (e *env) draftRow(id string) store.Draft {
	e.t.Helper()
	d, err := e.db.DraftByID(e.t.Context(), e.op, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

// An agent's draft under review: held with a card when every check
// passes, a draft with the verdict and a card when one escalates.
func TestReviewDraft(t *testing.T) {
	t.Parallel()
	e := newEnv(t, transport.CloudflareCaps())
	ctx := t.Context()
	cards := &fakeNotifier{}
	e.svc.cfg.Notifier = cards
	e.grant(e.arisuID, e.arisu, sendpolicy.Review, sendpolicy.ScopeAny, "")
	e.know("bob@example.net")
	e.deliver(`
From: Service <no-reply@example.net>
To: github@example.com
Subject: Your verification code
Message-ID: <otp1@example.net>

Your verification code is 740219.
`, "github")

	draft := func(to, body string) string {
		d, err := e.db.CreateDraft(ctx, e.arisu, store.Draft{FromAddr: "arisu@example.com", To: []store.Recipient{{Addr: to}}, Subject: "Lunch", BodyMD: body}, e.at())
		if err != nil {
			t.Fatal(err)
		}
		return d.ID
	}

	id := draft("bob@example.net", "See you at noon.\n\nBring the map.")
	res, err := e.svc.SubmitDraft(ctx, e.arisu, id)
	if err != nil || res.Queued || !res.Held || res.OutboxID == "" || res.DraftID != id || res.HoldSeconds != sendpolicy.DefaultHoldSeconds ||
		res.HoldReason != string(sendpolicy.HoldReview) {
		t.Fatalf("held: %+v %v", res, err)
	}
	if n := e.count(`SELECT count(*) FROM outbox WHERE id = $1 AND state = 'held' AND draft_id = $2 AND review IS NOT NULL`, res.OutboxID, id); n != 1 {
		t.Error("no held row")
	}
	if h := header(e.sent(res.OutboxID)); !strings.Contains(h, "Auto-Submitted: auto-generated") {
		t.Errorf("held agent mail without Auto-Submitted:\n%s", h)
	}
	if _, err := e.db.UpdateDraft(ctx, e.arisu, store.Draft{ID: id, FromAddr: "arisu@example.com", BodyMD: "x"}, e.at()); !errors.Is(err, store.ErrInFlight) {
		t.Errorf("edit while held: %v", err)
	}
	e.svc.Wait()
	if len(cards.held) != 1 || cards.held[0].OutboxID != res.OutboxID || !strings.Contains(cards.held[0].Excerpt, "See you at noon") ||
		cards.held[0].Submitter != "agent:arisu" || len(cards.held[0].Passed) != 5 {
		t.Errorf("held card %+v", cards.held)
	}

	// Withdrawn, it is a draft again; edited and submitted, held again.
	if _, err := e.db.WithdrawOutbox(ctx, e.arisu, res.OutboxID, nil, e.at()); err != nil {
		t.Fatal(err)
	}
	if d := e.draftRow(id); d.HoldReason != sendpolicy.HoldWithdrawn || d.OutboxID != "" {
		t.Errorf("withdrawn draft %+v", d)
	}
	if _, err := e.db.UpdateDraft(ctx, e.arisu, store.Draft{ID: id, FromAddr: "arisu@example.com", To: []store.Recipient{{Addr: "bob@example.net"}}, BodyMD: "Noon."}, e.at()); err != nil {
		t.Fatal(err)
	}
	if res, err := e.svc.SubmitDraft(ctx, e.arisu, id); err != nil || !res.Held {
		t.Errorf("resubmitted: %+v %v", res, err)
	}

	// The code that came in, written out in a reply: escalated.
	leak := draft("bob@example.net", "The code is 740-219.")
	res, err = e.svc.SubmitDraft(ctx, e.arisu, leak)
	if err != nil || res.Queued || res.Held || res.OutboxID != "" || res.HoldReason != string(sendpolicy.HoldReview) {
		t.Fatalf("leak: %+v %v", res, err)
	}
	d := e.draftRow(leak)
	if d.HoldReason != sendpolicy.HoldReview || !strings.Contains(string(d.Review), `"extracted_values"`) || strings.Contains(string(d.Review), "740") {
		t.Errorf("escalated draft: %s %s", d.HoldReason, d.Review)
	}
	if e.count(`SELECT count(*) FROM outbox WHERE draft_id = $1`, leak) != 0 {
		t.Error("an escalation made an outbox row")
	}

	// Or written as a recipient's name: names go out too.
	named, err := e.db.CreateDraft(ctx, e.arisu, store.Draft{
		FromAddr: "arisu@example.com", To: []store.Recipient{{Name: "Bob 740219", Addr: "bob@example.net"}}, Subject: "Lunch", BodyMD: "Noon.",
	}, e.at())
	if err != nil {
		t.Fatal(err)
	}
	if res, err := e.svc.SubmitDraft(ctx, e.arisu, named.ID); err != nil || res.Held || res.HoldReason != string(sendpolicy.HoldReview) ||
		!strings.Contains(string(e.draftRow(named.ID).Review), `"where": "to name"`) {
		t.Errorf("a code in a name: %+v %v %s", res, err, e.draftRow(named.ID).Review)
	}

	// A first contact: escalated, with the reason on the card.
	res, err = e.svc.SubmitDraft(ctx, e.arisu, draft("carol@example.org", "Hello."))
	if err != nil || res.Held || res.HoldReason != string(sendpolicy.HoldReview) || !strings.Contains(res.Detail, "first contact") {
		t.Errorf("first contact: %+v %v", res, err)
	}
	e.svc.Wait()
	if len(cards.escalated) != 3 || !slices.ContainsFunc(cards.escalated[2].Reasons, func(r string) bool { return strings.Contains(r, "carol@example.org") }) {
		t.Errorf("escalated cards %+v", cards.escalated)
	}

	// A person confirming the escalated draft is the review: queued.
	if res, err := e.svc.SubmitDraft(ctx, e.op, leak); err != nil || !res.Queued {
		t.Errorf("confirmed: %+v %v", res, err)
	}
}

// An SMTP submission under review: held with a draft backed by its raw,
// so a recall has somewhere to go; an attachment the review cannot read
// escalates.
func TestReviewRaw(t *testing.T) {
	t.Parallel()
	e := newEnv(t, transport.CloudflareCaps())
	ctx := t.Context()
	e.grant(e.alerts, e.monitor, sendpolicy.Review, sendpolicy.ScopeAny, "")
	e.know("ops@example.net")
	ops := []string{"ops@example.net"}

	res, err := e.svc.SubmitRaw(ctx, e.monitor, "alerts@example.com", ops,
		[]byte("From: alerts@example.com\nTo: ops@example.net\nSubject: Disk\n\nDisk full on host-1.\n"))
	if err != nil || !res.Held || res.Queued || res.OutboxID == "" || res.DraftID == "" {
		t.Fatalf("held: %+v %v", res, err)
	}
	if n := e.count(`SELECT count(*) FROM outbox o JOIN drafts d ON d.id = o.draft_id
		WHERE o.id = $1 AND o.state = 'held' AND d.id = $2 AND d.raw_id = o.raw_id AND d.hold_reason IS NULL`, res.OutboxID, res.DraftID); n != 1 {
		t.Error("held row without its raw-backed draft")
	}

	// Recalled, then submitted again by the service: held again, with
	// the same bytes.
	if _, err := e.db.WithdrawOutbox(ctx, e.op, res.OutboxID, nil, e.at()); err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.SubmitDraft(ctx, e.monitor, res.DraftID)
	if err != nil || !again.Held || e.sent(again.OutboxID) != e.sent(res.OutboxID) {
		t.Errorf("again: %+v %v", again, err)
	}

	pdf := "From: alerts@example.com\nTo: ops@example.net\nSubject: Report\nMIME-Version: 1.0\n" +
		"Content-Type: multipart/mixed; boundary=b\n\n--b\nContent-Type: text/plain\n\nThe report.\n" +
		"--b\nContent-Type: application/pdf; name=report.pdf\nContent-Disposition: attachment; filename=report.pdf\n" +
		"Content-Transfer-Encoding: base64\n\nJVBERi0xLjQK\n--b--\n"
	res, err = e.svc.SubmitRaw(ctx, e.monitor, "alerts@example.com", ops, []byte(pdf))
	if err != nil || res.Held || res.OutboxID != "" || res.HoldReason != string(sendpolicy.HoldReview) {
		t.Fatalf("pdf: %+v %v", res, err)
	}
	if d := e.draftRow(res.DraftID); !strings.Contains(string(d.Review), "application/pdf is not text") {
		t.Errorf("pdf verdict %s", d.Review)
	}
}

func TestReadOutgoing(t *testing.T) {
	t.Parallel()
	inner := "From: x@example.net\r\nMessage-ID: <inner@example.net>\r\nSubject: old\r\n\r\ncode 482913\r\n"
	raw := "From: a@example.com\r\nTo: b@example.net\r\nSubject: Fwd 7-4-0-2-1-9\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n" +
		"Content-Type: multipart/alternative; boundary=c\r\n\r\n--c\r\nContent-Type: text/plain\r\n\r\n" +
		"see yuuka://message/01JB0000000000000000000000 and https://evil.example/x\r\n--c\r\n" +
		"Content-Type: text/html\r\n\r\n<p>hidden <!-- 555666 --><a href=\"https://other.example/&amp;y\">l</a></p>\r\n--c--\r\n" +
		"--b\r\nContent-Type: text/plain; name=n.txt\r\nContent-Disposition: attachment; filename=n.txt\r\n\r\nnote 112233\r\n" +
		"--b\r\nContent-Type: message/rfc822\r\nContent-Disposition: attachment\r\n\r\n" + inner +
		"--b\r\nContent-Type: image/png; name=i.png\r\nContent-Disposition: attachment; filename=i.png\r\n\r\nxx\r\n--b--\r\n"
	o := readOutgoing([]byte(raw), []byte(`[{"uri":"yuuka://message/01JAGENT000000000000000000#part/1"},{"uri":"https://x"}]`))
	has := func(v string) bool {
		return slices.ContainsFunc(o.review.Tokens, func(t store.ReviewItem) bool { return t.Value == v })
	}
	for _, v := range []string{"740219", "555666", "112233", "482913", "https://evil.example/x"} {
		if !has(v) {
			t.Errorf("token %s missing", v)
		}
	}
	var hosts []string
	for _, h := range o.review.Hosts {
		hosts = append(hosts, h.Value)
	}
	if !slices.Contains(hosts, "evil.example") || !slices.Contains(hosts, "other.example") {
		t.Errorf("hosts %q", hosts)
	}
	var refs []string
	for _, r := range o.review.Refs {
		refs = append(refs, r.MessageID+r.HeaderID)
	}
	if !slices.Equal(refs, []string{"01JB0000000000000000000000", "inner@example.net", "01JAGENT000000000000000000", ""}) {
		t.Errorf("refs %+v", o.review.Refs)
	}
	if len(o.review.Unreadable) != 1 || !strings.Contains(o.review.Unreadable[0], "image/png") {
		t.Errorf("unreadable %q", o.review.Unreadable)
	}
	if len(o.attachments) != 3 {
		t.Errorf("attachments %+v", o.attachments)
	}
}
