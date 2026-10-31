package submit

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/mail"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/contenthash"
	"github.com/yuuka-mail/yuuka/internal/ingest"
	"github.com/yuuka-mail/yuuka/internal/localraw"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/outbox"
	"github.com/yuuka-mail/yuuka/internal/r2/r2fake"
	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/transport"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// fakeTransport only answers Caps: these tests stop at the outbox row.
type fakeTransport struct{ caps transport.Caps }

func (f fakeTransport) Caps() transport.Caps { return f.caps }

func (fakeTransport) Send(context.Context, transport.Envelope, []byte, func(context.Context) error) (*transport.SendResult, error) {
	return nil, errors.New("not in these tests")
}

// env has the principals and identities of §13.5's examples: the
// operator owns *@example.com, arisu owns arisu@, the monitoring service
// owns alerts@ and noreply@, and guest is a second human.
type env struct {
	t       *testing.T
	db      *store.DB
	pool    *pgxpool.Pool
	svc     *Service
	out     *outbox.Service
	account string
	seq     atomic.Int64

	op, guest, arisu, monitor *auth.Viewer
	star, arisuID, alerts     store.Identity
}

func newEnv(t *testing.T, caps transport.Caps) *env {
	t.Helper()
	db, pool := storetest.New(t)
	e := &env{t: t, db: db, pool: pool, account: storetest.Account(t, db)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	bucket, local := r2fake.New(), localraw.Dir{Root: t.TempDir()}
	in := ingest.New(ingest.Config{Raw: bucket, Local: local, DB: db, Log: log})
	e.out = outbox.New(outbox.Config{
		DB: db, Raw: bucket, Local: local, Transports: map[string]outbox.Transport{"cf": fakeTransport{caps}},
		IndexSent: in.IndexSent, Log: log,
	})
	e.svc = New(Config{DB: db, Outbox: e.out, Log: log})

	e.op = e.principal("human", "operator", auth.AllScopes...)
	e.guest = e.principal("human", "guest", auth.SubmissionSend)
	e.arisu = e.principal("agent", "arisu", auth.DraftCreate, auth.SubmissionSend)
	e.monitor = e.principal("service", "monitoring", auth.SubmissionSend)
	e.star = e.identity("*@example.com", "Operator", "Operator, example.com", e.op)
	e.arisuID = e.identity("arisu@example.com", "Arisu", "Arisu (an agent)", e.arisu)
	e.alerts = e.identity("alerts@example.com", "Alerts", "", e.monitor)
	e.identity("noreply@example.com", "No Reply", "", e.monitor)
	return e
}

func (e *env) at() time.Time { return time.Now().Add(time.Duration(e.seq.Add(1)) * time.Microsecond) }

func (e *env) principal(kind, name string, scopes ...auth.Scope) *auth.Viewer {
	e.t.Helper()
	at := e.at()
	p := store.Principal{ID: ulid.New(at), Kind: kind, Name: name, DisplayName: name, CreatedAt: at}
	if err := e.db.CreatePrincipal(e.t.Context(), p, store.Action{ID: ulid.New(at), Tool: "principal.create", At: at}); err != nil {
		e.t.Fatal(err)
	}
	return &auth.Viewer{PrincipalID: p.ID, PrincipalKind: kind, PrincipalName: name, CredentialID: "", Scopes: scopes}
}

func (e *env) identity(addr, name, sig string, owner *auth.Viewer) store.Identity {
	e.t.Helper()
	zone, prefix, err := store.SplitIdentityAddress(addr)
	if err != nil {
		e.t.Fatal(err)
	}
	at := e.at()
	i := store.Identity{
		ID: ulid.New(at), AccountID: e.account, Zone: zone, Prefix: prefix, DisplayName: name, Signature: sig,
		Transport: "cf", OwnerID: owner.PrincipalID, CreatedAt: at,
	}
	if err := e.db.AddIdentity(e.t.Context(), i, store.Action{ID: ulid.New(at), Tool: "identity.add", At: at}); err != nil {
		e.t.Fatal(err)
	}
	return i
}

// grant writes a grant row directly, so that a test can also write what
// `yuuka grant set` refuses (a hand-edited table).
func (e *env) grant(ident store.Identity, v *auth.Viewer, tier sendpolicy.Tier, scope sendpolicy.RecipientScope, set string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.t.Context(), `
		INSERT INTO identity_grants (identity_id, principal_id, send_policy, recipient_scope) VALUES ($1, $2, $3, $4)
		ON CONFLICT (identity_id, principal_id) DO UPDATE SET send_policy = $3, recipient_scope = $4,
			recipient_allowlist = '{}', max_per_hour = NULL, max_per_day = NULL, max_new_recipients_per_day = NULL,
			suspended_at = NULL`,
		ident.ID, v.PrincipalID, string(tier), string(scope)); err != nil {
		e.t.Fatal(err)
	}
	if set != "" {
		if _, err := e.pool.Exec(e.t.Context(), `UPDATE identity_grants SET `+set+` WHERE identity_id = $1 AND principal_id = $2`,
			append([]any{ident.ID, v.PrincipalID}, args...)...); err != nil {
			e.t.Fatal(err)
		}
	}
}

// deliver commits and folds raw (LF line endings) as delivered to
// prefix@example.com and returns the message id.
func (e *env) deliver(raw, prefix string) string {
	e.t.Helper()
	at := e.at()
	id := ulid.New(at)
	key := fmt.Sprintf("raw/2026/11/%s.eml", id)
	b := []byte(strings.ReplaceAll(strings.TrimLeft(raw, "\n"), "\n", "\r\n"))
	ch := contenthash.V1(b)
	if _, err := e.db.CommitIngest(e.t.Context(), store.Ingest{
		RawID: id, R2Key: key, LocalPath: key, Size: int64(len(b)), SHA256: sha256.Sum256(b),
		ContentHash: ch, ReceivedAt: at, DeliveryID: id, AccountID: e.account,
		Zone: "example.com", Prefix: prefix, EnvelopeTo: prefix + "@example.com",
	}); err != nil {
		e.t.Fatal(err)
	}
	f, err := e.db.Fold(e.t.Context(), store.RawRef{
		DeliveryID: id, RawID: id, R2Key: key, LocalPath: key, ContentHash: ch[:],
		Zone: "example.com", Prefix: prefix, ReceivedAt: at,
	}, mimeparse.Parse(b), authres.Extract(b, "mx.cloudflare.net"))
	if err != nil {
		e.t.Fatal(err)
	}
	return f.MessageID
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.t.Context(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// sent returns the sent copy of outbox row id.
func (e *env) sent(id string) string {
	e.t.Helper()
	var rawID string
	if err := e.pool.QueryRow(e.t.Context(), `SELECT raw_id FROM outbox WHERE id = $1`, id).Scan(&rawID); err != nil {
		e.t.Fatal(err)
	}
	return e.rawBytes(rawID)
}

func (e *env) rawBytes(rawID string) string {
	e.t.Helper()
	sr, err := e.db.SentRawByID(e.t.Context(), rawID)
	if err != nil {
		e.t.Fatal(err)
	}
	b, err := e.out.ReadSent(e.t.Context(), sr)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

// header returns the header section of raw.
func header(raw string) string {
	h, _, _ := strings.Cut(raw, "\r\n\r\n")
	return h + "\r\n"
}

// alert is an SMTP submission as monitoring software writes it: LF line
// endings, a display name of its own choosing, a Bcc.
func alert(from string, extra ...string) []byte {
	return []byte("From: \"The CEO\" <" + from + ">\n" + strings.Join(extra, "") +
		"To: \"Ops, on call\" <ops@example.net>\nBcc: Secret <secret@example.org>\nSubject: =?utf-8?q?Disk_f=C3=BCll?=\n\nDisk full on host-1.\n")
}

var opsAndSecret = []string{"ops@example.net", "secret@example.org"}

func TestSubmitRawQueued(t *testing.T) {
	t.Parallel()
	e := newEnv(t, transport.CloudflareCaps())
	ctx := t.Context()
	e.grant(e.alerts, e.monitor, sendpolicy.Auto, sendpolicy.ScopeAny, "")

	res, err := e.svc.SubmitRaw(ctx, e.monitor, "Alerts@example.com", opsAndSecret,
		alert("Alerts@example.com", "Message-ID: <client@example.com>\n", "Sender: noreply@example.com\n"))
	if err != nil || !res.Queued || res.OutboxID == "" || res.DraftID != "" {
		t.Fatalf("SubmitRaw = %+v, %v", res, err)
	}
	h := header(e.sent(res.OutboxID))
	for _, want := range []string{
		"From: \"Alerts\" <alerts@example.com>\r\n", // the identity's name, not the client's
		"Sender: \"No Reply\" <noreply@example.com>\r\n",
		"Auto-Submitted: auto-generated\r\n", // a service's mail goes out as automatic
		"Date: ",                             // added: the client wrote none
	} {
		if !strings.Contains(h, want) {
			t.Errorf("header lacks %q:\n%s", want, h)
		}
	}
	for _, not := range []string{"CEO", "Bcc", "secret@", "Message-ID", "\n\n", "\r\r"} {
		if strings.Contains(h, not) {
			t.Errorf("header has %q:\n%s", not, h)
		}
	}
	if strings.Contains(strings.ReplaceAll(e.sent(res.OutboxID), "\r\n", ""), "\n") {
		t.Error("bare LF in the sent copy")
	}

	var by, approved, from string
	if err := e.pool.QueryRow(ctx, `SELECT submitted_by, approved_by, from_addr FROM outbox WHERE id = $1`, res.OutboxID).
		Scan(&by, &approved, &from); err != nil {
		t.Fatal(err)
	}
	if by != e.monitor.PrincipalID || approved != store.SystemPrincipalID || from != "alerts@example.com" {
		t.Errorf("row by %s approved %s from %s", by, approved, from)
	}
	// Recipients from the envelope, labelled by the header.
	rows, err := e.pool.Query(ctx, `SELECT addr || ':' || kind FROM outbox_recipients WHERE outbox_id = $1 ORDER BY addr`, res.OutboxID)
	if err != nil {
		t.Fatal(err)
	}
	var rs []string
	for rows.Next() {
		var r string
		_ = rows.Scan(&r)
		rs = append(rs, r)
	}
	rows.Close()
	if !slices.Equal(rs, []string{"ops@example.net:to", "secret@example.org:bcc"}) {
		t.Errorf("recipients %q", rs)
	}
	// Indexed after the commit: the sent copy is a message.
	if n := e.count(`SELECT count(*) FROM messages m JOIN raw_objects r ON r.content_hash = m.content_hash
		JOIN outbox o ON o.raw_id = r.id WHERE o.id = $1`, res.OutboxID); n != 1 {
		t.Errorf("%d messages for the sent copy", n)
	}
	e.checkAudit(toolRaw, true, "Disk full")
}

// checkAudit checks the last entry of tool: it carries the decision and
// none of the body.
func (e *env) checkAudit(tool string, queue bool, body string) {
	e.t.Helper()
	var args, result map[string]any
	var raw string
	if err := e.pool.QueryRow(e.t.Context(), `SELECT args, result, args::text || result::text FROM actions WHERE tool = $1 ORDER BY id DESC LIMIT 1`, tool).
		Scan(&args, &result, &raw); err != nil {
		e.t.Fatal(err)
	}
	d, _ := result["decision"].(map[string]any)
	if d == nil || d["queue"] != queue {
		e.t.Errorf("%s result %v", tool, result)
	}
	if strings.Contains(raw, body) {
		e.t.Errorf("%s records the body: %s", tool, raw)
	}
}

func TestSubmitRawMessageID(t *testing.T) {
	t.Parallel()
	caps := transport.CloudflareCaps()
	caps.CallerMessageID = true
	e := newEnv(t, caps)
	e.grant(e.alerts, e.monitor, sendpolicy.Auto, sendpolicy.ScopeAny, "")
	res, err := e.svc.SubmitRaw(t.Context(), e.monitor, "alerts@example.com", opsAndSecret, alert("alerts@example.com"))
	if err != nil || !res.Queued {
		t.Fatalf("SubmitRaw = %+v, %v", res, err)
	}
	if h := header(e.sent(res.OutboxID)); !strings.Contains(h, "@example.com>\r\n") || !strings.Contains(h, "Message-ID: <") {
		t.Errorf("no Message-ID added for an exit that delivers ours:\n%s", h)
	}
	res, err = e.svc.SubmitRaw(t.Context(), e.monitor, "alerts@example.com", opsAndSecret,
		alert("alerts@example.com", "Message-ID: <mine@example.com>\n"))
	if err != nil || !strings.Contains(header(e.sent(res.OutboxID)), "Message-ID: <mine@example.com>\r\n") {
		t.Errorf("the client's Message-ID was not kept: %v", err)
	}
}

// The header rules of §13.6 refuse before anything is stored; each
// refusal is in the audit log.
func TestSubmitRawRules(t *testing.T) {
	t.Parallel()
	e := newEnv(t, transport.CloudflareCaps())
	ctx := t.Context()
	e.grant(e.alerts, e.monitor, sendpolicy.Auto, sendpolicy.ScopeAny, "")
	for name, c := range map[string]struct {
		mailFrom string
		rcpts    []string
		raw      []byte
		want     error
	}{
		"two From addresses": {"alerts@example.com", opsAndSecret, []byte("From: alerts@example.com, noreply@example.com\nTo: ops@example.net\n\nx\n"), ErrRejected},
		"two From fields":    {"alerts@example.com", opsAndSecret, alert("alerts@example.com", "From: alerts@example.com\n"), ErrRejected},
		"no From":            {"alerts@example.com", opsAndSecret, []byte("To: ops@example.net\n\nx\n"), ErrRejected},
		"MAIL FROM differs":  {"noreply@example.com", opsAndSecret, alert("alerts@example.com"), ErrRejected},
		"null MAIL FROM":     {"", opsAndSecret, alert("alerts@example.com"), ErrRejected},
		"From not ours":      {"alerts@example.org", opsAndSecret, alert("alerts@example.org"), ErrNoIdentity},
		"Sender not ours":    {"alerts@example.com", opsAndSecret, alert("alerts@example.com", "Sender: x@example.org\n"), ErrNoIdentity},
		// arisu@ is arisu's; the service has no grant on it.
		"Sender not granted": {"alerts@example.com", opsAndSecret, alert("alerts@example.com", "Sender: arisu@example.com\n"), ErrRejected},
		"two Senders":        {"alerts@example.com", opsAndSecret, alert("alerts@example.com", "Sender: noreply@example.com\nSender: noreply@example.com\n"), ErrRejected},
		// The operator's address: a service has no grant on it (tier
		// none), so there is no draft to stop at.
		"tier none":       {"ops@example.com", opsAndSecret, alert("ops@example.com"), ErrRejected},
		"bare CR":         {"alerts@example.com", opsAndSecret, alert("alerts@example.com", "X-A: a\rb\n"), ErrRejected},
		"long line":       {"alerts@example.com", opsAndSecret, alert("alerts@example.com", "X-A: "+strings.Repeat("a", 1000)+"\n"), ErrRejected},
		"bad recipient":   {"alerts@example.com", []string{"ops@example.net>\r\nRCPT TO:<x@example.org"}, alert("alerts@example.com"), ErrRejected},
		"no recipients":   {"alerts@example.com", nil, alert("alerts@example.com"), ErrRejected},
		"malformed field": {"alerts@example.com", opsAndSecret, []byte("From: alerts@example.com\nnot a header\n\nx\n"), ErrRejected},
	} {
		res, err := e.svc.SubmitRaw(ctx, e.monitor, c.mailFrom, c.rcpts, c.raw)
		if !errors.Is(err, c.want) || res != (Result{}) {
			t.Errorf("%s: %+v, %v; want %v", name, res, err, c.want)
		}
	}
	if n := e.count(`SELECT count(*) FROM outbox`) + e.count(`SELECT count(*) FROM drafts`) + e.count(`SELECT count(*) FROM raw_objects`); n != 0 {
		t.Errorf("%d rows after refusals", n)
	}
	if n := e.count(`SELECT count(*) FROM actions WHERE tool = $1 AND result ? 'rejected'`, toolRaw); n != 15 {
		t.Errorf("%d refusals recorded, want 15", n)
	}
	if _, err := e.svc.SubmitRaw(ctx, nil, "alerts@example.com", opsAndSecret, alert("alerts@example.com")); err == nil {
		t.Error("no viewer")
	}
}

// Every way a submission stops ends in a draft with the reason, over
// both entry points; nothing reaches the outbox.
func TestHoldReasons(t *testing.T) {
	t.Parallel()
	e := newEnv(t, transport.CloudflareCaps())
	ctx := t.Context()
	// A machine-classified message to reply to (RFC 3834 signals).
	auto := e.deliver(`
From: robot@example.net
To: arisu@example.com
Subject: Out of office
Message-ID: <ooo@example.net>
Auto-Submitted: auto-replied
Precedence: bulk

I am away.
`, "arisu")
	noSend := *e.monitor
	noSend.Scopes = nil

	type raw struct {
		v     *auth.Viewer
		from  string
		rcpts []string
		extra string
	}
	for _, c := range []struct {
		name  string
		setup func()
		raw   *raw
		draft *store.Draft // submitted by its author, arisu
		want  sendpolicy.HoldReason
	}{
		{name: "no submission:send", raw: &raw{&noSend, "alerts@example.com", []string{"ops@example.net"}, ""}, want: sendpolicy.HoldPolicy},
		{
			name: "tier draft", setup: func() { e.grant(e.alerts, e.monitor, sendpolicy.Draft, sendpolicy.ScopeAny, "") },
			raw: &raw{e.monitor, "alerts@example.com", []string{"ops@example.net"}, ""}, want: sendpolicy.HoldPolicy,
		},
		{name: "owner cap, raw", raw: &raw{e.arisu, "ops@example.com", []string{"ops@example.net"}, ""}, want: sendpolicy.HoldOwner},
		{name: "owner cap, draft", draft: &store.Draft{FromAddr: "ops@example.com", To: []store.Recipient{{Addr: "ops@example.net"}}, BodyMD: "x"}, want: sendpolicy.HoldOwner},
		{name: "suspended", setup: func() {
			e.grant(e.alerts, e.monitor, sendpolicy.Auto, sendpolicy.ScopeAny, "suspended_at = now()")
		}, raw: &raw{e.monitor, "alerts@example.com", []string{"ops@example.net"}, ""}, want: sendpolicy.HoldSuspended},
		{name: "allowlist", setup: func() {
			e.grant(e.alerts, e.monitor, sendpolicy.Auto, sendpolicy.ScopeAllowlist, "recipient_allowlist = '{example.net}'")
		}, raw: &raw{e.monitor, "alerts@example.com", []string{"ops@example.net", "x@example.org"}, ""}, want: sendpolicy.HoldRecipients},
		{
			name: "thread scope, new thread", setup: func() { e.grant(e.arisuID, e.arisu, sendpolicy.Auto, sendpolicy.ScopeThread, "") },
			draft: &store.Draft{FromAddr: "arisu@example.com", To: []store.Recipient{{Addr: "bob@example.net"}}, BodyMD: "x"}, want: sendpolicy.HoldRecipients,
		},
		{
			name: "loop, raw reply", setup: func() { e.grant(e.arisuID, e.arisu, sendpolicy.Auto, sendpolicy.ScopeAny, "") },
			raw: &raw{e.arisu, "arisu@example.com", []string{"robot@example.net"}, "In-Reply-To: <ooo@example.net>\n"}, want: sendpolicy.HoldLoop,
		},
		{
			name: "loop, draft reply", draft: &store.Draft{FromAddr: "arisu@example.com", InReplyToID: auto, To: []store.Recipient{{Addr: "robot@example.net"}}, BodyMD: "x"},
			want: sendpolicy.HoldLoop,
		},
		{name: "limit", setup: func() {
			e.grant(e.alerts, e.monitor, sendpolicy.Auto, sendpolicy.ScopeAny, "max_per_hour = 0")
		}, raw: &raw{e.monitor, "alerts@example.com", []string{"ops@example.net"}, ""}, want: sendpolicy.HoldLimit},
		{
			name: "review", setup: func() { e.grant(e.arisuID, e.arisu, sendpolicy.Review, sendpolicy.ScopeAny, "") },
			draft: &store.Draft{FromAddr: "arisu@example.com", To: []store.Recipient{{Addr: "bob@example.net"}}, BodyMD: "x"}, want: sendpolicy.HoldReview,
		},
	} {
		if c.setup != nil {
			c.setup()
		}
		var res Result
		var err error
		if c.raw != nil {
			res, err = e.svc.SubmitRaw(ctx, c.raw.v, c.raw.from, c.raw.rcpts, alert(c.raw.from, c.raw.extra))
		} else {
			d, cerr := e.db.CreateDraft(ctx, e.arisu, *c.draft, e.at())
			if cerr != nil {
				t.Fatalf("%s: %v", c.name, cerr)
			}
			res, err = e.svc.SubmitDraft(ctx, e.arisu, d.ID)
			if res.DraftID != d.ID {
				t.Errorf("%s: held as draft %s, not %s", c.name, res.DraftID, d.ID)
			}
		}
		if err != nil || res.Queued || res.HoldReason != string(c.want) || res.Detail == "" {
			t.Errorf("%s: %+v, %v; want %s", c.name, res, err, c.want)
			continue
		}
		got, err := e.db.DraftByID(ctx, e.op, res.DraftID)
		if err != nil || got.HoldReason != c.want || got.HoldDetail != res.Detail {
			t.Errorf("%s: draft %+v, %v", c.name, got, err)
		}
		if c.raw != nil {
			// Held raw: the normalized bytes under sent/, no message yet.
			// It shows the names the header gives To and Cc; Bcc has
			// none, its header is gone.
			if got.RawID == "" || got.Subject != "Disk füll" ||
				len(got.To) > 0 && !slices.Equal(got.To, []store.Recipient{{Name: "Ops, on call", Addr: "ops@example.net"}}) ||
				len(got.Bcc) > 0 && got.Bcc[0].Name != "" {
				t.Errorf("%s: raw draft %+v", c.name, got)
			}
			b := e.rawBytes(got.RawID)
			if !strings.Contains(b, "From: ") || strings.Contains(header(b), "Bcc") || strings.Contains(header(b), "CEO") {
				t.Errorf("%s: held bytes not normalized:\n%s", c.name, header(b))
			}
		}
	}
	if n := e.count(`SELECT count(*) FROM outbox`); n != 0 {
		t.Errorf("%d outbox rows", n)
	}
	if n := e.count(`SELECT count(*) FROM messages m JOIN raw_objects r ON r.content_hash = m.content_hash WHERE r.r2_key LIKE 'sent/%'`); n != 0 {
		t.Errorf("held raws became %d messages", n)
	}
}

// §8.9's hard cap over every entry point: on an identity a human owns,
// no one else's submission is queued, whatever the grant table says
// (written by hand here: `yuuka grant set` refuses it) and whoever
// confirms, unless it is the owner.
func TestOwnerCapOverEntryPoints(t *testing.T) {
	t.Parallel()
	e := newEnv(t, transport.CloudflareCaps())
	ctx := t.Context()
	other := e.principal("human", "other", auth.SubmissionSend, auth.DraftCreate)
	for _, sub := range []*auth.Viewer{e.arisu, e.monitor, e.guest} {
		for _, tier := range []sendpolicy.Tier{sendpolicy.None, sendpolicy.Draft, sendpolicy.Review, sendpolicy.Auto} {
			e.grant(e.star, sub, tier, sendpolicy.ScopeAny, "")
			name := fmt.Sprintf("%s with %s", sub, tier)

			res, err := e.svc.SubmitRaw(ctx, sub, "ops@example.com", []string{"x@example.net"}, alert("ops@example.com"))
			if res.Queued || (tier == sendpolicy.None) != errors.Is(err, ErrRejected) {
				t.Errorf("%s, SMTP: %+v, %v", name, res, err)
			}
			d, err := e.db.CreateDraft(ctx, sub, store.Draft{FromAddr: "ops@example.com", To: []store.Recipient{{Addr: "x@example.net"}}, BodyMD: "x"}, e.at())
			if err != nil {
				t.Fatal(err)
			}
			if res, err := e.svc.SubmitDraft(ctx, sub, d.ID); err != nil || res.Queued || res.HoldReason != string(sendpolicy.HoldOwner) {
				t.Errorf("%s, own draft: %+v, %v", name, res, err)
			}
			// Confirmed by a human who is not the owner.
			for _, c := range []*auth.Viewer{e.guest, other} {
				if c == sub {
					continue
				}
				if res, err := e.svc.SubmitDraft(ctx, c, d.ID); err != nil || res.Queued || res.HoldReason != string(sendpolicy.HoldOwner) {
					t.Errorf("%s, confirmed by %s: %+v, %v", name, c, res, err)
				}
			}
		}
	}
	if n := e.count(`SELECT count(*) FROM outbox`); n != 0 {
		t.Fatalf("%d outbox rows", n)
	}
	// The owner's confirmation is what lets it go.
	d, err := e.db.CreateDraft(ctx, e.arisu, store.Draft{FromAddr: "ops@example.com", To: []store.Recipient{{Addr: "x@example.net"}}, BodyMD: "x"}, e.at())
	if err != nil {
		t.Fatal(err)
	}
	if res, err := e.svc.SubmitDraft(ctx, e.op, d.ID); err != nil || !res.Queued {
		t.Errorf("confirmed by the owner: %+v, %v", res, err)
	}
}

// Concurrent submissions racing for max_per_hour: the grant's lock lets
// exactly the allowed number through, the rest are drafts.
func TestSubmitRace(t *testing.T) {
	t.Parallel()
	e := newEnv(t, transport.CloudflareCaps())
	e.grant(e.alerts, e.monitor, sendpolicy.Auto, sendpolicy.ScopeAny, "max_per_hour = 2")
	const n = 6
	var mu sync.Mutex
	var queued, limited int
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			res, err := e.svc.SubmitRaw(t.Context(), e.monitor, "alerts@example.com", []string{"ops@example.net"}, alert("alerts@example.com"))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				t.Error(err)
			case res.Queued:
				queued++
			case res.HoldReason == string(sendpolicy.HoldLimit):
				limited++
			}
		})
	}
	wg.Wait()
	if queued != 2 || limited != n-2 || e.count(`SELECT count(*) FROM outbox`) != 2 {
		t.Errorf("%d queued, %d limited; want 2 and %d", queued, limited, n-2)
	}
}

func TestSubmitDraftComposes(t *testing.T) {
	t.Parallel()
	e := newEnv(t, transport.CloudflareCaps())
	ctx := t.Context()
	parent := e.deliver(`
From: Bob <bob@example.net>
To: github@example.com
Subject: Hello
Message-ID: <parent@example.net>
In-Reply-To: <root@example.net>
References: <root@example.net>

Hello.
`, "github")
	d, err := e.db.CreateDraft(ctx, e.op, store.Draft{
		FromAddr: "github@example.com", InReplyToID: parent, To: []store.Recipient{{Name: "王小明", Addr: "bob@example.net"}},
		Cc: []store.Recipient{{Name: `Smith, "J"`, Addr: "j@example.org"}}, Bcc: []store.Recipient{{Name: "Archive", Addr: "archive@example.org"}},
		Subject: "Re: Hello", BodyMD: "Hi **Bob**\n",
	}, e.at())
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.SubmitDraft(ctx, e.op, d.ID)
	if err != nil || !res.Queued {
		t.Fatalf("SubmitDraft = %+v, %v", res, err)
	}
	raw := e.sent(res.OutboxID)
	h := header(raw)
	for _, want := range []string{
		"From: \"Operator\" <github@example.com>\r\n",
		"In-Reply-To: <parent@example.net>\r\n",
		"References: <root@example.net> <parent@example.net>\r\n",
		"Subject: Re: Hello\r\n",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("header lacks %q:\n%s", want, h)
		}
	}
	// The names go into To and Cc, encoded; the Bcc's into nothing.
	rc := mimeparse.ParseRecipients([]byte(raw))
	if len(rc.To) != 1 || rc.To[0] != (mimeparse.Mailbox{Name: "王小明", Addr: "bob@example.net"}) ||
		len(rc.Cc) != 1 || rc.Cc[0] != (mimeparse.Mailbox{Name: `Smith, "J"`, Addr: "j@example.org"}) {
		t.Errorf("recipients as sent: %+v", rc)
	}
	for _, not := range []string{"Message-ID", "Auto-Submitted", "Bcc", "archive@", "Archive"} {
		if strings.Contains(h, not) {
			t.Errorf("header has %q:\n%s", not, h)
		}
	}
	if !strings.Contains(raw, "Hi **Bob**\r\n\r\n--=20\r\nOperator, example.com") {
		t.Errorf("no signature below the delimiter:\n%s", raw)
	}
	if e.count(`SELECT count(*) FROM outbox_recipients WHERE outbox_id = $1 AND addr = 'archive@example.org' AND kind = 'bcc'`, res.OutboxID) != 1 {
		t.Error("the Bcc is not an envelope recipient")
	}
	// The sent copy threads with the message it answers.
	if e.count(`SELECT count(*) FROM messages a JOIN messages b ON a.thread_id = b.thread_id
		JOIN raw_objects r ON r.content_hash = b.content_hash JOIN outbox o ON o.raw_id = r.id
		WHERE a.id = $1 AND o.id = $2`, parent, res.OutboxID) != 1 {
		t.Error("the reply is not in the thread")
	}
	e.checkAudit(toolDraft, true, "Bob**")

	// Pressing send again: the draft went once.
	if _, err := e.svc.SubmitDraft(ctx, e.op, d.ID); !errors.Is(err, store.ErrInFlight) {
		t.Errorf("second send: %v", err)
	}
	if _, err := e.svc.SubmitDraft(ctx, e.op, ulid.New(e.at())); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown draft: %v", err)
	}
}

func TestSubmitDraftConcurrentSend(t *testing.T) {
	t.Parallel()
	e := newEnv(t, transport.CloudflareCaps())
	d, err := e.db.CreateDraft(t.Context(), e.op, store.Draft{FromAddr: "github@example.com", To: []store.Recipient{{Addr: "bob@example.net"}}, BodyMD: "x"}, e.at())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var queued atomic.Int64
	for range 4 {
		wg.Go(func() {
			res, err := e.svc.SubmitDraft(t.Context(), e.op, d.ID)
			if err == nil && res.Queued {
				queued.Add(1)
			} else if !errors.Is(err, store.ErrInFlight) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if queued.Load() != 1 || e.count(`SELECT count(*) FROM outbox`) != 1 {
		t.Errorf("queued %d times", queued.Load())
	}
}

func TestSubmitDraftMessageIDAndAgent(t *testing.T) {
	t.Parallel()
	caps := transport.CloudflareCaps()
	caps.CallerMessageID = true
	e := newEnv(t, caps)
	ctx := t.Context()
	e.grant(e.arisuID, e.arisu, sendpolicy.Auto, sendpolicy.ScopeAny, "")
	d, err := e.db.CreateDraft(ctx, e.arisu, store.Draft{FromAddr: "arisu@example.com", To: []store.Recipient{{Addr: "bob@example.net"}}, Subject: "Hi", BodyMD: "x"}, e.at())
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.SubmitDraft(ctx, e.arisu, d.ID)
	if err != nil || !res.Queued {
		t.Fatalf("SubmitDraft = %+v, %v", res, err)
	}
	h := header(e.sent(res.OutboxID))
	if !strings.Contains(h, "Message-ID: <") || !strings.Contains(h, "@example.com>\r\n") ||
		!strings.Contains(h, "Auto-Submitted: auto-generated\r\n") || !strings.Contains(h, "From: \"Arisu\" <arisu@example.com>\r\n") {
		t.Errorf("header:\n%s", h)
	}
}

// A held SMTP submission is confirmed with the bytes that were stored;
// re-submitted by its service under a decision that now asks for
// Auto-Submitted, it goes out normalized again.
func TestHeldRawResubmitted(t *testing.T) {
	t.Parallel()
	e := newEnv(t, transport.CloudflareCaps())
	ctx := t.Context()
	submit := func() Result {
		t.Helper()
		// No grant: a service on its own identity defaults to review +
		// thread, which holds a new thread.
		res, err := e.svc.SubmitRaw(ctx, e.monitor, "alerts@example.com", opsAndSecret, alert("alerts@example.com"))
		if err != nil || res.Queued || res.HoldReason != string(sendpolicy.HoldRecipients) {
			t.Fatalf("SubmitRaw = %+v, %v", res, err)
		}
		return res
	}
	held := submit()
	d, err := e.db.DraftByID(ctx, e.op, held.DraftID)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.SubmitDraft(ctx, e.op, held.DraftID)
	if err != nil || !res.Queued {
		t.Fatalf("confirm = %+v, %v", res, err)
	}
	var rawID, by, approved string
	if err := e.pool.QueryRow(ctx, `SELECT raw_id, submitted_by, approved_by FROM outbox WHERE id = $1`, res.OutboxID).
		Scan(&rawID, &by, &approved); err != nil {
		t.Fatal(err)
	}
	if rawID != d.RawID || by != e.monitor.PrincipalID || approved != e.op.PrincipalID {
		t.Errorf("confirmed: raw %s (held %s), by %s, approved %s", rawID, d.RawID, by, approved)
	}
	if e.count(`SELECT count(*) FROM outbox_recipients WHERE outbox_id = $1`, res.OutboxID) != 2 {
		t.Error("recipients")
	}

	held = submit()
	d, _ = e.db.DraftByID(ctx, e.op, held.DraftID)
	e.grant(e.alerts, e.monitor, sendpolicy.Auto, sendpolicy.ScopeAny, "")
	res, err = e.svc.SubmitDraft(ctx, e.monitor, held.DraftID)
	if err != nil || !res.Queued {
		t.Fatalf("resubmit = %+v, %v", res, err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT raw_id FROM outbox WHERE id = $1`, res.OutboxID).Scan(&rawID); err != nil {
		t.Fatal(err)
	}
	if rawID == d.RawID || !strings.Contains(header(e.sent(res.OutboxID)), "Auto-Submitted: auto-generated\r\n") ||
		strings.Contains(header(e.rawBytes(d.RawID)), "Auto-Submitted") {
		t.Errorf("resubmitted as raw %s (held %s):\n%s", rawID, d.RawID, header(e.sent(res.OutboxID)))
	}
}

func TestNormalizeIsIdempotent(t *testing.T) {
	t.Parallel()
	m, err := readRaw(alert("alerts@example.com", "Auto-Submitted: no\n"))
	if err != nil {
		t.Fatal(err)
	}
	n := normalization{
		From: mailAddr("Alerts", "alerts@example.com"), AutoSubmitted: sendpolicy.AutoSubmittedValue,
		Date: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC), MessageID: "x@example.com", Caps: transport.Caps{CallerMessageID: true},
	}
	once := m.normalize(n)
	m2, err := readRaw(once)
	if err != nil {
		t.Fatal(err)
	}
	n.MessageID = "y@example.com"
	n.Date = n.Date.Add(time.Hour)
	if twice := m2.normalize(n); string(twice) != string(once) {
		t.Errorf("not idempotent:\n%s\n---\n%s", once, twice)
	}
	if h := header(string(once)); strings.Contains(h, "Auto-Submitted: no") || strings.Count(h, "Auto-Submitted") != 1 {
		t.Errorf("Auto-Submitted: no not replaced:\n%s", h)
	}
}

func mailAddr(name, addr string) mail.Address { return mail.Address{Name: name, Address: addr} }
