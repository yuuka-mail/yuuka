package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/contenthash"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// sendEnv is a database with the principals and identities of §13.5's
// examples: the operator owns *@example.com, arisu owns arisu@ and the
// monitoring service is meant for alerts@ (owned by the operator).
type sendEnv struct {
	t        *testing.T
	db       *store.DB
	pool     *pgxpool.Pool
	account  string
	op       *auth.Viewer
	arisu    *auth.Viewer
	monitor  *auth.Viewer
	star     store.Identity
	arisuID  store.Identity
	alertsID store.Identity
	seq      atomic.Int64
}

var base = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func newSendEnv(t *testing.T) *sendEnv {
	t.Helper()
	db, pool := storetest.New(t)
	e := &sendEnv{t: t, db: db, pool: pool, account: storetest.Account(t, db)}
	e.op = e.principal("human", "operator", auth.AllScopes...)
	e.arisu = e.principal("agent", "arisu", auth.MessageRead, auth.DraftCreate, auth.SubmissionSend)
	e.monitor = e.principal("service", "monitoring", auth.SubmissionSend)
	e.star = e.identity("*@example.com", e.op.PrincipalID)
	e.arisuID = e.identity("arisu@example.com", e.arisu.PrincipalID)
	e.alertsID = e.identity("alerts@example.com", e.op.PrincipalID)
	return e
}

func (e *sendEnv) at() time.Time { return base.Add(time.Duration(e.seq.Add(1)) * time.Millisecond) }

func (e *sendEnv) principal(kind, name string, scopes ...auth.Scope) *auth.Viewer {
	e.t.Helper()
	at := e.at()
	p := store.Principal{ID: ulid.New(at), Kind: kind, Name: name, DisplayName: name, CreatedAt: at}
	if err := e.db.CreatePrincipal(e.t.Context(), p, store.Action{ID: ulid.New(at), Tool: "principal.create", At: at}); err != nil {
		e.t.Fatal(err)
	}
	return &auth.Viewer{PrincipalID: p.ID, PrincipalKind: kind, PrincipalName: name, Scopes: scopes}
}

func (e *sendEnv) identity(addr, owner string) store.Identity {
	e.t.Helper()
	zone, prefix, err := store.SplitIdentityAddress(addr)
	if err != nil {
		e.t.Fatal(err)
	}
	at := e.at()
	i := store.Identity{ID: ulid.New(at), AccountID: e.account, Zone: zone, Prefix: prefix, Transport: "cf", OwnerID: owner, CreatedAt: at}
	if err := e.db.AddIdentity(e.t.Context(), i, store.Action{ID: ulid.New(at), Actor: store.SystemPrincipalID, Tool: "identity.add", At: at}); err != nil {
		e.t.Fatalf("add %s: %v", addr, err)
	}
	got, err := e.db.IdentityByID(e.t.Context(), i.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

// deliver commits and folds raw as delivered to prefix@zone.
func (e *sendEnv) deliver(raw, prefix string) string {
	e.t.Helper()
	at := e.at()
	id := ulid.New(at)
	key := fmt.Sprintf("raw/2026/11/%s.eml", id)
	b := []byte(strings.ReplaceAll(strings.TrimLeft(raw, "\n"), "\n", "\r\n"))
	ch := contenthash.V1(b)
	in := store.Ingest{
		RawID: id, R2Key: key, LocalPath: key, Size: int64(len(b)), SHA256: sha256.Sum256(append([]byte(prefix), b...)),
		ContentHash: ch, ReceivedAt: at, DeliveryID: id, AccountID: e.account,
		Zone: "example.com", Prefix: prefix, EnvelopeTo: prefix + "@example.com",
	}
	if _, err := e.db.CommitIngest(e.t.Context(), in); err != nil {
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

// sentRaw inserts a sent copy; with like set, its content hash is that
// of message like, as if the copy had been folded into it.
func (e *sendEnv) sentRaw(like string) string {
	e.t.Helper()
	at := e.at()
	id := ulid.New(at)
	sum := sha256.Sum256([]byte(id))
	var ch []byte
	if like != "" {
		if err := e.pool.QueryRow(e.t.Context(), `SELECT content_hash FROM messages WHERE id = $1`, like).Scan(&ch); err != nil {
			e.t.Fatal(err)
		}
	} else {
		ch = sum[:]
	}
	if _, err := e.pool.Exec(e.t.Context(), `INSERT INTO raw_objects (id, r2_key, local_path, size, sha256, received_at, content_hash)
		VALUES ($1, $2, $2, 1, $3, $4, $5)`, id, "sent/2026/11/"+id+".eml", sum[:], at, ch); err != nil {
		e.t.Fatal(err)
	}
	return id
}

type outboxRow struct {
	state      string
	identity   store.Identity
	by         string
	approvedBy string // "" = by
	raw        string // "" = a fresh sent copy
	draft      string
	created    time.Time
	to         []string
}

func (e *sendEnv) outbox(o outboxRow) string {
	e.t.Helper()
	if o.state == "" {
		o.state = "sent"
	}
	if o.approvedBy == "" {
		o.approvedBy = o.by
	}
	if o.raw == "" {
		o.raw = e.sentRaw("")
	}
	id := ulid.New(e.at())
	var next any
	if o.state == "queued" || o.state == "failed" {
		next = o.created
	}
	var approved any = o.approvedBy
	if o.state == "withdrawn" {
		approved = nil
	}
	var draft any
	if o.draft != "" {
		draft = o.draft
	}
	if _, err := e.pool.Exec(e.t.Context(), `INSERT INTO outbox (id, state, next_retry_at, identity_id, from_addr, submitted_by,
		approved_by, raw_id, draft_id, created_at) VALUES ($1, $2, $3, $4, 'x@example.com', $5, $6, $7, $8, $9)`,
		id, o.state, next, o.identity.ID, o.by, approved, o.raw, draft, o.created); err != nil {
		e.t.Fatal(err)
	}
	for _, r := range o.to {
		if _, err := e.pool.Exec(e.t.Context(), `INSERT INTO outbox_recipients (outbox_id, addr, addr_fold, kind)
			VALUES ($1, $2, $3, 'to')`, id, r, strings.ToLower(r)); err != nil {
			e.t.Fatal(err)
		}
	}
	return id
}

func (e *sendEnv) actions(tool string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.t.Context(), `SELECT count(*) FROM actions WHERE tool = $1`, tool).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestIdentities(t *testing.T) {
	t.Parallel()
	e := newSendEnv(t)
	ctx := t.Context()

	accts, err := e.db.Accounts(ctx)
	if err != nil || len(accts) != 1 || accts[0].ID != e.account {
		t.Fatalf("accounts = %v, %v", accts, err)
	}
	if e.star.Address() != "*@example.com" || e.arisuID.Address() != "arisu@example.com" || e.arisuID.OwnerKind != "agent" {
		t.Errorf("identities %+v %+v", e.star, e.arisuID)
	}
	at := e.at()
	dup := store.Identity{ID: ulid.New(at), AccountID: e.account, Zone: "example.com", Transport: "cf", OwnerID: e.op.PrincipalID}
	if err := e.db.AddIdentity(ctx, dup, store.Action{ID: ulid.New(at), Tool: "identity.add", At: at}); !errors.Is(err, store.ErrDuplicate) {
		t.Errorf("second *@example.com: %v", err)
	}
	dup.Zone = "Example.com"
	if err := e.db.AddIdentity(ctx, dup, store.Action{ID: ulid.New(at), Tool: "identity.add", At: at}); err == nil {
		t.Error("uppercase zone accepted")
	}

	for addr, want := range map[string]string{
		"GitHub@Example.COM": e.star.ID + " github@example.com",
		"alerts@example.com": e.alertsID.ID + " alerts@example.com",
		"Arisu@example.com":  e.arisuID.ID + " arisu@example.com",
	} {
		i, from, err := e.db.ResolveIdentity(ctx, addr)
		if err != nil || i.ID+" "+from != want {
			t.Errorf("resolve %s: %s %s, %v; want %s", addr, i.ID, from, err, want)
		}
	}
	for _, addr := range []string{"x@example.org", "*@example.com", "nobody", "a b@example.com"} {
		if _, _, err := e.db.ResolveIdentity(ctx, addr); err == nil {
			t.Errorf("resolve %q: no error", addr)
		}
	}
	if _, _, err := e.db.ResolveIdentity(ctx, "x@example.org"); !errors.Is(err, store.ErrNoIdentity) {
		t.Errorf("unknown zone: %v", err)
	}

	// Disabling the exact identity hands its address back to the pattern.
	at = e.at()
	if err := e.db.DisableIdentity(ctx, e.alertsID.ID, store.Action{ID: ulid.New(at), Tool: "identity.disable", At: at}); err != nil {
		t.Fatal(err)
	}
	if err := e.db.DisableIdentity(ctx, e.alertsID.ID, store.Action{ID: ulid.New(at), Tool: "identity.disable", At: at}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("disable twice: %v", err)
	}
	if i, _, err := e.db.ResolveIdentity(ctx, "alerts@example.com"); err != nil || i.ID != e.star.ID {
		t.Errorf("after disable: %s, %v", i.ID, err)
	}
	if _, err := e.db.IdentityAt(ctx, "example.com", "alerts"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("IdentityAt a disabled one: %v", err)
	}
	if i, err := e.db.IdentityAt(ctx, "example.com", ""); err != nil || i.ID != e.star.ID {
		t.Errorf("IdentityAt pattern: %v", err)
	}
	enabled, _ := e.db.Identities(ctx, false)
	all, _ := e.db.Identities(ctx, true)
	if len(enabled) != 2 || len(all) != 3 || all[0].Prefix != "" {
		t.Errorf("list: %d enabled, %d all", len(enabled), len(all))
	}
	if e.actions("identity.disable") != 1 || e.actions("identity.add") != 3 {
		t.Error("actions not recorded")
	}
}

const (
	bobMail = `
From: Bob <bob@example.net>
To: github@example.com, arisu@example.com
Subject: Hello
Message-ID: <m1@example.net>
Date: Mon, 2 Nov 2026 10:00:00 +0000

Hi there.
`
	carolReply = `
From: Carol <Carol@Example.org>
To: github@example.com
Subject: Re: Hello
Message-ID: <m2@example.org>
In-Reply-To: <m1@example.net>
References: <m1@example.net>
Date: Mon, 2 Nov 2026 11:00:00 +0000

Me too.
`
	autoReply = `
From: Vacation <bob@example.net>
To: github@example.com
Subject: Re: Hello
Message-ID: <m3@example.net>
In-Reply-To: <m2@example.org>
Auto-Submitted: auto-replied
Date: Mon, 2 Nov 2026 12:00:00 +0000

I am away.
`
)

func TestReplyIdentities(t *testing.T) {
	t.Parallel()
	e := newSendEnv(t)
	ctx := t.Context()
	m1 := e.deliver(bobMail, "github")
	if again := e.deliver(bobMail, "arisu"); again != m1 {
		t.Fatalf("second copy is %s, not %s", again, m1)
	}
	froms := func(v *auth.Viewer, id string) []string {
		t.Helper()
		cs, err := e.db.ReplyIdentities(ctx, v, id)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range cs {
			out = append(out, c.FromAddr+"/"+c.Identity.Address())
		}
		return out
	}
	// Each answers as the identity it owns first.
	if got := froms(e.op, m1); !slices.Equal(got, []string{"github@example.com/*@example.com", "arisu@example.com/arisu@example.com"}) {
		t.Errorf("operator: %v", got)
	}
	if got := froms(e.arisu, m1); !slices.Equal(got, []string{"arisu@example.com/arisu@example.com", "github@example.com/*@example.com"}) {
		t.Errorf("arisu: %v", got)
	}
	// The service owns neither: exact before pattern.
	if got := froms(e.monitor, m1); !slices.Equal(got, []string{"arisu@example.com/arisu@example.com", "github@example.com/*@example.com"}) {
		t.Errorf("monitoring: %v", got)
	}
	// Mail we sent: the identity it went out as.
	m2 := e.deliver(carolReply, "github")
	e.outbox(outboxRow{identity: e.arisuID, by: e.arisu.PrincipalID, raw: e.sentRaw(m2), created: base, to: []string{"bob@example.net"}})
	if got := froms(e.arisu, m2); !slices.Equal(got, []string{"x@example.com/arisu@example.com", "github@example.com/*@example.com"}) {
		t.Errorf("sent copy: %v", got)
	}
	if _, err := e.db.ReplyIdentities(ctx, e.op, ulid.New(base)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing message: %v", err)
	}
	if _, err := e.db.ReplyIdentities(ctx, nil, m1); err == nil {
		t.Error("nil viewer")
	}
}

func TestGrants(t *testing.T) {
	t.Parallel()
	e := newSendEnv(t)
	ctx := t.Context()
	set := func(g store.Grant) error {
		at := e.at()
		return e.db.SetGrant(ctx, g, store.Action{ID: ulid.New(at), Actor: store.SystemPrincipalID, Tool: "grant.set", At: at})
	}
	n := 5
	// The owner cap is refused, not stored.
	for _, tier := range []sendpolicy.Tier{sendpolicy.Review, sendpolicy.Auto} {
		err := set(store.Grant{IdentityID: e.alertsID.ID, PrincipalID: e.monitor.PrincipalID, Policy: tier, RecipientScope: sendpolicy.ScopeAny})
		if !errors.Is(err, store.ErrOwnerCap) {
			t.Errorf("%s on a human identity: %v", tier, err)
		}
	}
	if err := set(store.Grant{IdentityID: e.alertsID.ID, PrincipalID: e.monitor.PrincipalID, Policy: sendpolicy.Draft, RecipientScope: sendpolicy.ScopeAny}); err != nil {
		t.Errorf("draft on a human identity: %v", err)
	}
	if err := set(store.Grant{IdentityID: e.star.ID, PrincipalID: e.op.PrincipalID, Policy: sendpolicy.Auto, RecipientScope: sendpolicy.ScopeAny}); err != nil {
		t.Errorf("owner's own grant: %v", err)
	}
	g := store.Grant{
		IdentityID: e.arisuID.ID, PrincipalID: e.arisu.PrincipalID, Policy: sendpolicy.Auto,
		RecipientScope: sendpolicy.ScopeAllowlist, Allowlist: []string{"example.net"}, MaxPerHour: &n, HoldSeconds: 60,
	}
	if err := set(g); err != nil {
		t.Fatal(err)
	}
	if err := set(store.Grant{IdentityID: e.arisuID.ID, PrincipalID: e.arisu.PrincipalID, Policy: "sometimes", RecipientScope: sendpolicy.ScopeAny}); err == nil {
		t.Error("unknown tier")
	}
	if err := set(store.Grant{IdentityID: ulid.New(base), PrincipalID: e.arisu.PrincipalID, Policy: sendpolicy.Auto, RecipientScope: sendpolicy.ScopeAny}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing identity: %v", err)
	}
	got, err := e.db.GrantFor(ctx, e.arisuID.ID, e.arisu.PrincipalID)
	if err != nil || got.Policy != sendpolicy.Auto || got.MaxPerHour == nil || *got.MaxPerHour != 5 || got.MaxPerDay != nil ||
		!slices.Equal(got.Allowlist, []string{"example.net"}) || got.IdentityAddress != "arisu@example.com" || got.PrincipalName != "arisu" {
		t.Errorf("grant = %+v, %v", got, err)
	}
	// Replace, keeping created_at; suspend.
	created := got.CreatedAt
	g.MaxPerHour, g.SuspendedAt = nil, &base
	if err := set(g); err != nil {
		t.Fatal(err)
	}
	got, _ = e.db.GrantFor(ctx, e.arisuID.ID, e.arisu.PrincipalID)
	if got.MaxPerHour != nil || got.SuspendedAt == nil || !got.CreatedAt.Equal(created) || !got.ForPolicy().Suspended {
		t.Errorf("replaced grant = %+v", got)
	}
	all, _ := e.db.Grants(ctx, "", "")
	mine, _ := e.db.Grants(ctx, "", e.arisu.PrincipalID)
	if len(all) != 3 || len(mine) != 1 || all[0].IdentityAddress != "*@example.com" {
		t.Errorf("list: %d, %d", len(all), len(mine))
	}
	at := e.at()
	a := store.Action{ID: ulid.New(at), Tool: "grant.remove", At: at}
	if err := e.db.RemoveGrant(ctx, e.arisuID.ID, e.arisu.PrincipalID, a); err != nil {
		t.Fatal(err)
	}
	a.ID = ulid.New(e.at())
	if err := e.db.RemoveGrant(ctx, e.arisuID.ID, e.arisu.PrincipalID, a); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("remove twice: %v", err)
	}
	if _, err := e.db.GrantFor(ctx, e.arisuID.ID, e.arisu.PrincipalID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("after remove: %v", err)
	}
	if e.actions("grant.set") != 4 || e.actions("grant.remove") != 1 {
		t.Errorf("actions: %d set, %d remove", e.actions("grant.set"), e.actions("grant.remove"))
	}
}

// Standings preview what Decide says about every submission as an
// identity, before its recipients are known.
func TestStandings(t *testing.T) {
	t.Parallel()
	e := newSendEnv(t)
	ctx := t.Context()
	type row struct {
		tier    sendpolicy.Tier
		hold    sendpolicy.HoldReason
		granted bool
	}
	got := func(v *auth.Viewer) map[string]row {
		t.Helper()
		ss, err := e.db.Standings(ctx, v)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]row{}
		if len(ss) != 3 {
			t.Errorf("%s: %d standings, want every identity", v, len(ss))
		}
		for _, s := range ss {
			if s.Tier == sendpolicy.None { // not usable: only the rest are compared
				if s.Hold != sendpolicy.HoldPolicy && s.Hold != sendpolicy.HoldOwner {
					t.Errorf("%s as %s: tier none, hold %q", v, s.Identity.Address(), s.Hold)
				}
				continue
			}
			out[s.Identity.Address()] = row{s.Tier, s.Hold, s.Granted}
			if s.Capped != (s.Identity.OwnerKind == "human" && s.Identity.OwnerID != v.PrincipalID) {
				t.Errorf("%s as %s: capped %v", v, s.Identity.Address(), s.Capped)
			}
		}
		return out
	}
	check := func(v *auth.Viewer, want map[string]row) {
		t.Helper()
		if g := got(v); fmt.Sprint(g) != fmt.Sprint(want) {
			t.Errorf("%s: %v, want %v", v, g, want)
		}
	}
	// The defaults of §13.5: the operator sends as their own; arisu is
	// reviewed as itself and drafts as the operator; the service has
	// nothing it may use.
	check(e.op, map[string]row{
		"*@example.com": {sendpolicy.Auto, "", false}, "alerts@example.com": {sendpolicy.Auto, "", false},
	})
	check(e.arisu, map[string]row{
		"*@example.com":      {sendpolicy.Draft, sendpolicy.HoldOwner, false},
		"alerts@example.com": {sendpolicy.Draft, sendpolicy.HoldOwner, false},
		"arisu@example.com":  {sendpolicy.Review, sendpolicy.HoldReview, false},
	})
	check(e.monitor, map[string]row{})

	at := e.at()
	if err := e.db.SetGrant(ctx, store.Grant{
		IdentityID: e.arisuID.ID, PrincipalID: e.arisu.PrincipalID, Policy: sendpolicy.Auto, RecipientScope: sendpolicy.ScopeAny,
	}, store.Action{ID: ulid.New(at), Tool: "grant.set", At: at}); err != nil {
		t.Fatal(err)
	}
	if r := got(e.arisu)["arisu@example.com"]; r != (row{sendpolicy.Auto, "", true}) {
		t.Errorf("arisu granted auto: %+v", r)
	}
	// The credential counts: without submission:send all is held.
	noSend := *e.arisu
	noSend.Scopes = []auth.Scope{auth.DraftCreate}
	if r := got(&noSend)["arisu@example.com"]; r.hold != sendpolicy.HoldPolicy {
		t.Errorf("without submission:send: %+v", r)
	}
	if _, err := e.db.Standings(ctx, nil); err == nil {
		t.Error("nil viewer")
	}
}

func TestDrafts(t *testing.T) {
	t.Parallel()
	e := newSendEnv(t)
	ctx := t.Context()
	m1 := e.deliver(bobMail, "github")
	other := e.principal("agent", "momoi", auth.DraftCreate)

	d, err := e.db.CreateDraft(ctx, e.arisu, store.Draft{
		FromAddr: "GitHub@example.com", InReplyToID: m1, To: []string{"bob@example.net"}, Subject: "Re: Hello", BodyMD: "Hi **Bob**",
	}, e.at())
	if err != nil {
		t.Fatal(err)
	}
	if d.IdentityID != e.star.ID || d.FromAddr != "github@example.com" || d.PrincipalID != e.arisu.PrincipalID || d.Cc == nil {
		t.Errorf("created %+v", d)
	}
	for name, bad := range map[string]store.Draft{
		"unknown From":       {FromAddr: "me@example.org", BodyMD: "x"},
		"identity mismatch":  {FromAddr: "github@example.com", IdentityID: e.arisuID.ID, BodyMD: "x"},
		"bad recipient":      {FromAddr: "arisu@example.com", To: []string{"bob@example.net\r\nBcc: eve@example.org"}, BodyMD: "x"},
		"no @":               {FromAddr: "arisu@example.com", To: []string{"bob"}, BodyMD: "x"},
		"missing reply":      {FromAddr: "arisu@example.com", InReplyToID: ulid.New(base), BodyMD: "x"},
		"cites not an array": {FromAddr: "arisu@example.com", Cites: []byte(`{}`), BodyMD: "x"},
	} {
		_, err := e.db.CreateDraft(ctx, e.arisu, bad, e.at())
		want := store.ErrInvalidDraft
		if name == "unknown From" {
			want = store.ErrNoIdentity
		}
		if !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", name, err, want)
		}
	}
	if _, err := e.db.CreateDraft(ctx, nil, store.Draft{FromAddr: "arisu@example.com"}, e.at()); err == nil {
		t.Error("nil viewer")
	}

	// Visibility: the author and humans.
	for _, v := range []*auth.Viewer{e.arisu, e.op} {
		if got, err := e.db.DraftByID(ctx, v, d.ID); err != nil || got.BodyMD != "Hi **Bob**" || got.InReplyToID != m1 {
			t.Errorf("%s reads: %+v, %v", v, got, err)
		}
	}
	if _, err := e.db.DraftByID(ctx, other, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another agent reads: %v", err)
	}
	if ds, _ := e.db.Drafts(ctx, other, store.DraftQuery{}); len(ds) != 0 {
		t.Errorf("another agent lists %d", len(ds))
	}
	if _, err := e.db.UpdateDraft(ctx, other, store.Draft{ID: d.ID, FromAddr: "arisu@example.com"}, e.at()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another agent updates: %v", err)
	}

	// Submitted, it is held (the owner cap), then edited: the edit
	// clears the hold.
	hold := func(d store.Draft) error {
		return e.db.WithDecision(ctx, e.arisu, store.Submission{
			FromAddr: d.FromAddr, IdentityID: d.IdentityID, Recipients: d.To, InReplyToID: d.InReplyToID, Now: e.at(),
		}, func(ctx context.Context, p *store.PolicyTx) error { return p.HoldDraft(ctx, d.ID, e.at()) })
	}
	if err := hold(d); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.db.DraftByID(ctx, e.op, d.ID); got.HoldReason != sendpolicy.HoldOwner || !strings.Contains(got.HoldDetail, "drafts only") {
		t.Errorf("held: %+v", got)
	}
	upd, err := e.db.UpdateDraft(ctx, e.arisu, store.Draft{
		ID: d.ID, FromAddr: "arisu@example.com", InReplyToID: m1,
		To: []string{"bob@example.net"}, Bcc: []string{"carol@example.org"}, BodyMD: "Hi",
	}, e.at())
	if err != nil {
		t.Fatal(err)
	}
	if upd.IdentityID != e.arisuID.ID || upd.HoldReason != "" || upd.Bcc[0] != "carol@example.org" || !upd.UpdatedAt.After(upd.CreatedAt) {
		t.Errorf("updated: %+v", upd)
	}

	// An SMTP submission held as a draft carries its raw and is not edited.
	msg := []byte("From: alerts@example.com\r\nTo: ops@example.net\r\n\r\nDisk full.\r\n")
	at := e.at()
	raw := store.NewSentRaw(ulid.New(at), "sent/2026/11/x.eml", msg, at)
	var smtp store.Draft
	err = e.db.WithDecision(ctx, e.monitor, store.Submission{FromAddr: "alerts@example.com", Recipients: []string{"ops@example.net"}, Now: at},
		func(ctx context.Context, p *store.PolicyTx) error {
			var err error
			smtp, err = p.CreateDraft(ctx, store.Draft{FromAddr: "alerts@example.com", To: []string{"ops@example.net"}}, &raw, msg, at)
			return err
		})
	if err != nil {
		t.Fatal(err)
	}
	if smtp.RawID != raw.ID || smtp.HoldReason != sendpolicy.HoldOwner || smtp.PrincipalID != e.monitor.PrincipalID {
		t.Errorf("held SMTP submission: %+v", smtp)
	}
	if _, err := e.db.UpdateDraft(ctx, e.monitor, store.Draft{ID: smtp.ID, FromAddr: "alerts@example.com", BodyMD: "x"}, e.at()); !errors.Is(err, store.ErrRawDraft) {
		t.Error("edited an SMTP draft")
	}
	// Raw bodies and hold reasons are a decision's to record.
	for name, bad := range map[string]store.Draft{
		"raw":         {FromAddr: "alerts@example.com", RawID: raw.ID},
		"hold reason": {FromAddr: "alerts@example.com", BodyMD: "x", HoldReason: sendpolicy.HoldLimit},
	} {
		if _, err := e.db.CreateDraft(ctx, e.monitor, bad, e.at()); err == nil {
			t.Errorf("%s: no error", name)
		}
	}

	// Once in the outbox it is history.
	e.outbox(outboxRow{
		state: "queued", identity: e.arisuID, by: e.arisu.PrincipalID, approvedBy: store.SystemPrincipalID,
		draft: d.ID, created: base, to: []string{"bob@example.net"},
	})
	if _, err := e.db.UpdateDraft(ctx, e.arisu, upd, e.at()); !errors.Is(err, store.ErrInFlight) {
		t.Errorf("update in flight: %v", err)
	}
	if err := hold(upd); !errors.Is(err, store.ErrInFlight) {
		t.Errorf("hold in flight: %v", err)
	}
	if err := e.db.DeleteDraft(ctx, e.arisu, d.ID, e.at()); !errors.Is(err, store.ErrInFlight) {
		t.Errorf("delete in flight: %v", err)
	}
	pending, _ := e.db.Drafts(ctx, e.op, store.DraftQuery{})
	every, _ := e.db.Drafts(ctx, e.op, store.DraftQuery{Open: true, Held: true, InFlight: true})
	if len(pending) != 1 || pending[0].ID != smtp.ID || len(every) != 2 || every[1].OutboxState != "queued" {
		t.Errorf("lists: %d pending, %d all", len(pending), len(every))
	}
	if sent, _ := e.db.Drafts(ctx, e.op, store.DraftQuery{InFlight: true}); len(sent) != 1 || sent[0].ID != d.ID {
		t.Errorf("in flight: %+v", sent)
	}
	if open, _ := e.db.Drafts(ctx, e.op, store.DraftQuery{Open: true}); len(open) != 0 {
		t.Errorf("open: %+v", open)
	}
	// The cursor continues after the first page.
	page, _ := e.db.Drafts(ctx, e.op, store.DraftQuery{Open: true, Held: true, InFlight: true, Limit: 1})
	rest, _ := e.db.Drafts(ctx, e.op, store.DraftQuery{
		Open: true, Held: true, InFlight: true,
		Before: &store.DraftCursor{UpdatedAt: page[0].UpdatedAt, ID: page[0].ID},
	})
	if len(page) != 1 || len(rest) != 1 || page[0].ID != every[0].ID || rest[0].ID != every[1].ID {
		t.Errorf("paged: %d then %d", len(page), len(rest))
	}
	if every[0].AuthorKind != "service" || every[0].AuthorName != "monitoring" {
		t.Errorf("author: %+v", every[0])
	}
	if err := e.db.DeleteDraft(ctx, e.monitor, smtp.ID, e.at()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.DraftByID(ctx, e.op, smtp.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleted: %v", err)
	}

	var changes int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM changes WHERE kind = 'draft'`).Scan(&changes); err != nil || changes != 5 {
		t.Errorf("draft changes = %d, %v", changes, err)
	}
	var actor string
	var args map[string]any
	if err := e.pool.QueryRow(ctx, `SELECT actor, args FROM actions WHERE tool = 'draft.hold'`).Scan(&actor, &args); err != nil ||
		actor != e.arisu.PrincipalID || args["hold_reason"] != "owner" || args["draft_id"] != d.ID {
		t.Errorf("hold action: %s %v %v", actor, args, err)
	}
	for _, tool := range []string{"draft.create", "draft.update", "draft.delete"} {
		if e.actions(tool) == 0 {
			t.Errorf("no %s", tool)
		}
	}
}

func TestPolicyFacts(t *testing.T) {
	t.Parallel()
	e := newSendEnv(t)
	ctx := t.Context()
	m1 := e.deliver(bobMail, "github")
	m2 := e.deliver(carolReply, "github")
	m3 := e.deliver(autoReply, "github")
	var thread string
	if err := e.pool.QueryRow(ctx, `SELECT thread_id FROM messages WHERE id = $1`, m1).Scan(&thread); err != nil {
		t.Fatal(err)
	}

	now := base.Add(48 * time.Hour)
	arisu := e.arisu.PrincipalID
	// Earlier mail: dave was reached in this thread through a sent copy,
	// erin through a draft replying into it; frank only by a withdrawn
	// message, which reached nobody.
	e.outbox(outboxRow{
		identity: e.arisuID, by: arisu, approvedBy: store.SystemPrincipalID, raw: e.sentRaw(m2),
		created: now.Add(-30 * time.Minute), to: []string{"Dave@example.net", "bob@example.net"},
	})
	dr, err := e.db.CreateDraft(ctx, e.arisu, store.Draft{FromAddr: "arisu@example.com", InReplyToID: m1, BodyMD: "x"}, e.at())
	if err != nil {
		t.Fatal(err)
	}
	e.outbox(outboxRow{
		identity: e.arisuID, by: arisu, approvedBy: store.SystemPrincipalID, draft: dr.ID,
		created: now.Add(-2 * time.Hour), to: []string{"erin@example.net", "bob@example.net"},
	})
	e.outbox(outboxRow{
		state: "withdrawn", identity: e.arisuID, by: arisu, raw: e.sentRaw(m2),
		created: now.Add(-3 * time.Hour), to: []string{"frank@example.net"},
	})
	// Older than a day, and someone else's: not counted for arisu.
	e.outbox(outboxRow{
		identity: e.arisuID, by: arisu, approvedBy: store.SystemPrincipalID, raw: e.sentRaw(m2),
		created: now.Add(-25 * time.Hour), to: []string{"bob@example.net", "grace@example.net"},
	})
	e.outbox(outboxRow{identity: e.star, by: e.op.PrincipalID, created: now.Add(-time.Minute), to: []string{"henry@example.net"}})

	rf, err := e.db.ReplyFacts(ctx, e.arisu, m2)
	if err != nil {
		t.Fatal(err)
	}
	// The mail arrived on an unclaimed prefix, so its To does not count
	// (§8.9), except m2's: we sent that content too (dave's row), and our
	// own mail's To counts.
	want := []string{
		"bob@example.net", "carol@example.org", "dave@example.net", "erin@example.net",
		"github@example.com", "grace@example.net",
	}
	if rf.ThreadID != thread || !slices.Equal(rf.Correspondents, want) || rf.Machine {
		t.Errorf("reply facts = %+v, want participants %v", rf, want)
	}
	if rf, _ := e.db.ReplyFacts(ctx, e.arisu, m3); !rf.Machine {
		t.Error("Auto-Submitted reply is not machine")
	}
	// A human override to human does not clear it; one to machine sets it.
	if _, err := e.pool.Exec(ctx, `INSERT INTO message_state (message_id, class_override) VALUES ($1, 2), ($2, 1)`, m3, m1); err != nil {
		t.Fatal(err)
	}
	if rf, _ := e.db.ReplyFacts(ctx, e.arisu, m3); !rf.Machine {
		t.Error("override to human cleared machine")
	}
	if rf, _ := e.db.ReplyFacts(ctx, e.arisu, m1); !rf.Machine {
		t.Error("override to machine ignored")
	}

	c, err := e.db.SendCounters(ctx, e.arisuID.ID, arisu, []string{"BOB@example.net", "frank@example.net", "zoe@example.net"}, thread, now)
	if err != nil {
		t.Fatal(err)
	}
	// Rows of the last hour/day by arisu as arisu@: 1 / 3 (withdrawn
	// included). New in the last day: dave and erin (bob and grace were
	// reached 25 hours ago; frank's row was withdrawn).
	if c.SentLastHour != 1 || c.SentLastDay != 3 || c.NewRecipientsLastDay != 2 ||
		!slices.Equal(c.FirstContact, []string{"frank@example.net", "zoe@example.net"}) ||
		c.AutoInThread["bob@example.net"] != 2 || len(c.AutoInThread) != 1 {
		t.Errorf("counters = %+v", c)
	}
	if c, _ := e.db.SendCounters(ctx, e.arisuID.ID, arisu, []string{"bob@example.net"}, "", now); c.AutoInThread != nil {
		t.Errorf("not a reply: %+v", c.AutoInThread)
	}

	// The whole input, then the decision.
	f, err := e.db.PolicyInput(ctx, e.arisu, store.Submission{
		FromAddr: "Arisu@example.com", Recipients: []string{"carol@example.org"},
		InReplyToID: m2, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	in := f.Input
	if f.FromAddr != "arisu@example.com" || in.Identity.ID != e.arisuID.ID || in.Submitter.ID != arisu || !in.CanSubmit ||
		in.Grant != nil || in.Reply == nil || len(in.Reply.Correspondents) != len(want) || in.Counters.SentLastDay != 3 {
		t.Errorf("input = %+v", in)
	}
	// No grant row: arisu on its own identity defaults to review.
	if d := sendpolicy.Decide(in); d.Queue || d.HoldReason != sendpolicy.HoldReview {
		t.Errorf("decision = %+v", d)
	}
	at := e.at()
	if err := e.db.SetGrant(ctx, store.Grant{
		IdentityID: e.arisuID.ID, PrincipalID: arisu, Policy: sendpolicy.Auto,
		RecipientScope: sendpolicy.ScopeThread,
	}, store.Action{ID: ulid.New(at), Tool: "grant.set", At: at}); err != nil {
		t.Fatal(err)
	}
	f, _ = e.db.PolicyInput(ctx, e.arisu, store.Submission{
		FromAddr: "arisu@example.com", Recipients: []string{"carol@example.org"},
		InReplyToID: m2, Now: now,
	})
	if d := sendpolicy.Decide(f.Input); !d.Queue || d.ApprovedBy != store.SystemPrincipalID || d.AutoSubmitted != sendpolicy.AutoSubmittedValue {
		t.Errorf("auto decision = %+v", d)
	}
	f, _ = e.db.PolicyInput(ctx, e.arisu, store.Submission{
		FromAddr: "arisu@example.com", Recipients: []string{"carol@example.org"},
		InReplyToID: m3, Now: now,
	})
	if d := sendpolicy.Decide(f.Input); d.HoldReason != sendpolicy.HoldLoop {
		t.Errorf("reply to an auto-reply = %+v", d)
	}
	// arisu writing as the operator's address: the cap.
	f, _ = e.db.PolicyInput(ctx, e.arisu, store.Submission{FromAddr: "github@example.com", Recipients: []string{"bob@example.net"}, Now: now})
	if d := sendpolicy.Decide(f.Input); d.HoldReason != sendpolicy.HoldOwner || d.Tier != sendpolicy.Draft {
		t.Errorf("as the operator = %+v", d)
	}
	// The operator confirms that draft: the author's grant is the one
	// counted, the operator approves.
	f, _ = e.db.PolicyInput(ctx, e.op, store.Submission{
		FromAddr: "github@example.com", IdentityID: e.star.ID,
		Recipients: []string{"bob@example.net"}, DraftAuthorID: arisu, Now: now,
	})
	if f.Input.Submitter.ID != arisu || f.Input.ConfirmedBy == nil || f.Input.ConfirmedBy.ID != e.op.PrincipalID {
		t.Errorf("confirm input = %+v", f.Input)
	}
	if d := sendpolicy.Decide(f.Input); !d.Queue || d.ApprovedBy != e.op.PrincipalID || d.AutoSubmitted != "" {
		t.Errorf("confirm = %+v", d)
	}
	// A new exact identity takes the address the draft was made for: the
	// draft's identity no longer covers it, and nothing goes out as it.
	e.identity("github@example.com", e.op.PrincipalID)
	f, err = e.db.PolicyInput(ctx, e.op, store.Submission{
		FromAddr: "github@example.com", IdentityID: e.star.ID,
		Recipients: []string{"bob@example.net"}, DraftAuthorID: arisu, Now: now,
	})
	if err != nil || f.Identity.ID != e.star.ID || !f.Input.Identity.Disabled {
		t.Errorf("superseded identity: %+v, %v", f.Input.Identity, err)
	}
	if d := sendpolicy.Decide(f.Input); d.Queue {
		t.Errorf("superseded identity sent: %+v", d)
	}
	if _, err := e.db.PolicyInput(ctx, e.monitor, store.Submission{FromAddr: "ceo@example.org", Now: now}); !errors.Is(err, store.ErrNoIdentity) {
		t.Errorf("unknown From: %v", err)
	}
	if _, err := e.db.PolicyInput(ctx, nil, store.Submission{FromAddr: "arisu@example.com"}); err == nil {
		t.Error("nil viewer")
	}

	// The locked variant reads the same facts inside its transaction and
	// decides on them itself.
	var inTx store.PolicyFacts
	var d sendpolicy.Decision
	err = e.db.WithDecision(ctx, e.arisu, store.Submission{
		FromAddr: "arisu@example.com", Recipients: []string{"carol@example.org"},
		InReplyToID: m2, Now: now,
	}, func(_ context.Context, p *store.PolicyTx) error {
		inTx, d = p.Facts(), p.Decision()
		return nil
	})
	if err != nil || inTx.Input.Grant == nil || inTx.Input.Counters.SentLastDay != 3 || d != sendpolicy.Decide(inTx.Input) || !d.Queue {
		t.Errorf("WithDecision: %+v, %+v, %v", inTx.Input, d, err)
	}
}
