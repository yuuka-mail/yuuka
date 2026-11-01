package store_test

import (
	"crypto/sha256"
	"errors"
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

// Switching account_access on (§13.8) changes nothing for a deployment
// that has no rows: mail written before 0019 to its Cloudflare account,
// received and sent, stays readable by every person, agent and service.
// What changes is what is new: a pull account is readable only by the
// person who added it, and a row overrides an account's default in both
// directions.
func TestDefaultReadUpgrade(t *testing.T) {
	t.Parallel()
	ownerURL, _ := storetest.URLs(t)
	pool, err := pgxpool.New(t.Context(), ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ctx := t.Context()
	db := store.New(pool)

	// A deployment at 0018: one Cloudflare account, people, an agent, a
	// service, received and sent mail, no account_access rows.
	if _, to, err := db.MigrateTo(ctx, 18); err != nil || to != 18 {
		t.Fatalf("migrate to 18: %d, %v", to, err)
	}
	cf := oldAccount(t, pool, "cloudflare")
	principal := func(kind, name string) *auth.Viewer {
		p, err := db.EnsurePrincipal(ctx, store.Principal{ID: ulid.New(time.Now()), Kind: kind, Name: name, DisplayName: name, CreatedAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		return &auth.Viewer{PrincipalID: p.ID, PrincipalKind: kind, PrincipalName: name}
	}
	op, other := principal("human", "operator"), principal("human", "family")
	agent, service := principal("agent", "arisu"), principal("service", "monitoring")
	host := &auth.Viewer{PrincipalID: store.SystemPrincipalID, PrincipalKind: "system", PrincipalName: "system"}

	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	tick := func() time.Time { at = at.Add(time.Minute); return at }
	before := map[string]string{
		"received": inbound(t, db, cf, "a", letter("x@example.net", "a@example.com", "received", "Message-ID: <r@example.net>\r\n", "hello"), tick()),
	}
	sent := letter("me@example.com", "x@example.net", "sent", "Message-ID: <s@example.com>\r\n", "sent body")
	rawID, _ := enqueue(t, pool, identity(t, pool, cf, "example.com"), sent, tick())
	before["sent"] = foldSent(t, db, rawID, sent).MessageID

	if _, to, err := db.Migrate(ctx); err != nil || to < 19 {
		t.Fatalf("migrate: %d, %v", to, err)
	}

	list := func(v *auth.Viewer) []string {
		t.Helper()
		ms, err := db.ListMessages(ctx, v, store.MessageQuery{MessageFilter: store.MessageFilter{Downgraded: store.IncludeDowngraded}})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, m := range ms {
			ids = append(ids, m.ID)
		}
		slices.Sort(ids)
		return ids
	}
	accounts := func(v *auth.Viewer) []string {
		t.Helper()
		as, err := db.VisibleAccounts(ctx, v)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, a := range as {
			names = append(names, a.Name)
		}
		return names
	}
	ids := func(ms map[string]string, names ...string) []string {
		var out []string
		for _, n := range names {
			out = append(out, ms[n])
		}
		slices.Sort(out)
		return out
	}
	old := ids(before, "received", "sent")
	everyone := []*auth.Viewer{op, other, agent, service, host, {PrincipalKind: "agent", PrincipalName: "no id"}}
	for _, v := range everyone {
		if got := list(v); !slices.Equal(got, old) {
			t.Errorf("after the upgrade %s sees %v, want %v", v.PrincipalName, got, old)
		}
		if got := accounts(v); !slices.Equal(got, []string{"cloudflare"}) {
			t.Errorf("after the upgrade %s reads accounts %v", v.PrincipalName, got)
		}
	}

	// A pull account, added by the operator, with one message.
	e := &pullEnv{t: t, db: db, pool: pool, op: op.PrincipalID}
	gmail := e.add("gmail", "me@gmail.example.com")
	e.mailbox(gmail.AccountID, "All", store.KeyGmail, 1, 0)
	raw := letter("x@example.net", "me@gmail.example.com", "pulled", "Message-ID: <p@example.net>\r\n", "pulled body")
	p := e.pull(gmail.AccountID, store.GmailSourceKey(1), store.RemoteLocation{Mailbox: "All", UIDValidity: 1, UID: 1}, raw)
	hash := contenthash.V1(raw)
	p.ContentHash, p.SHA256, p.Size = &hash, sha256.Sum256(raw), int64(len(raw))
	if _, err := db.CommitPull(ctx, p); err != nil {
		t.Fatal(err)
	}
	f, err := db.Fold(ctx, store.RawRef{
		DeliveryID: p.DeliveryID, RawID: p.RawID, R2Key: p.R2Key, LocalPath: p.LocalPath, ContentHash: hash[:],
		Zone: gmail.Zone(), Prefix: gmail.Prefix(), ReceivedAt: p.InternalDate,
	}, mimeparse.Parse(raw), authres.Extract(raw, gmail.TrustedAuthservID))
	if err != nil {
		t.Fatal(err)
	}
	pulled := f.MessageID
	withPulled := append(slices.Clone(old), pulled)
	slices.Sort(withPulled)

	for _, v := range everyone {
		want, wantAccts := old, []string{"cloudflare"}
		if v == op || v == host {
			want, wantAccts = withPulled, []string{"cloudflare", "gmail"}
		}
		if got := list(v); !slices.Equal(got, want) {
			t.Errorf("with a pull account %s sees %v, want %v", v.PrincipalName, got, want)
		}
		if got := accounts(v); !slices.Equal(got, wantAccts) {
			t.Errorf("with a pull account %s reads accounts %v, want %v", v.PrincipalName, got, wantAccts)
		}
	}
	d, err := db.MessageByID(ctx, op, pulled)
	if err != nil || len(d.Deliveries) != 1 {
		t.Fatalf("pulled message: %+v %v", d, err)
	}
	if f := d.Deliveries[0]; f.EnvelopeFrom != nil || f.EnvelopeTo != "me@gmail.example.com" || f.Zone != "gmail.example.com" || f.Prefix != "me" {
		t.Errorf("pulled delivery %+v", f)
	}
	if _, err := db.MessageByID(ctx, agent, pulled); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the agent reads the pulled message: %v", err)
	}

	// A row hides a readable-by-default account; removing it restores
	// the default.
	no, yes := false, true
	// current is the account's default and v's row, as a writer reads
	// them before deciding whether its change widens.
	current := func(account string, v *auth.Viewer) (bool, *bool) {
		t.Helper()
		a, err := db.AccountByName(ctx, account)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := db.AccountAccess(ctx, account)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.PrincipalID == v.PrincipalID {
				return a.DefaultRead, &r.CanRead
			}
		}
		return a.DefaultRead, nil
	}
	set := func(account string, v *auth.Viewer, canRead *bool) {
		t.Helper()
		def, was := current(account, v)
		if err := db.SetAccountAccess(ctx, account, v.PrincipalID, def, was, canRead, e.action("account.access")); err != nil {
			t.Fatal(err)
		}
	}
	set("cloudflare", agent, &no)
	if got := list(agent); len(got) != 0 {
		t.Errorf("hidden from the agent, it sees %v", got)
	}
	if got := accounts(agent); len(got) != 0 {
		t.Errorf("hidden from the agent, it reads %v", got)
	}
	if got := list(service); !slices.Equal(got, old) {
		t.Errorf("the service lost mail with the agent: %v", got)
	}
	set("cloudflare", agent, nil)
	if got := list(agent); !slices.Equal(got, old) {
		t.Errorf("back to the default the agent sees %v", got)
	}

	// The other way: a row opens the pull account to one more person;
	// a readable default opens it to all but whoever a row excludes.
	set("gmail", other, &yes)
	if got := list(other); !slices.Equal(got, withPulled) {
		t.Errorf("given gmail, %s sees %v", other.PrincipalName, got)
	}
	// Decided against what was read: a default or a row that changed
	// since is stale, and nothing is written.
	if err := db.SetAccountDefaultRead(ctx, "gmail", true, true, e.action("account.access")); !errors.Is(err, store.ErrStale) {
		t.Errorf("default read as true while false: %v", err)
	}
	if err := db.SetAccountAccess(ctx, "gmail", service.PrincipalID, true, nil, &yes, e.action("account.access")); !errors.Is(err, store.ErrStale) {
		t.Errorf("access with the default read as true while false: %v", err)
	}
	if err := db.SetAccountAccess(ctx, "gmail", other.PrincipalID, false, nil, &no, e.action("account.access")); !errors.Is(err, store.ErrStale) {
		t.Errorf("access with no row read while there is one: %v", err)
	}
	if err := db.SetAccountAccess(ctx, "gmail", other.PrincipalID, false, &no, nil, e.action("account.access")); !errors.Is(err, store.ErrStale) {
		t.Errorf("access with a row read as false while true: %v", err)
	}
	if got := list(other); !slices.Equal(got, withPulled) {
		t.Errorf("a stale write changed what %s sees: %v", other.PrincipalName, got)
	}
	if err := db.SetAccountDefaultRead(ctx, "gmail", false, true, e.action("account.access")); err != nil {
		t.Fatal(err)
	}
	set("gmail", service, &no)
	for v, want := range map[*auth.Viewer][]string{agent: withPulled, service: old, op: withPulled} {
		if got := list(v); !slices.Equal(got, want) {
			t.Errorf("gmail readable by default: %s sees %v, want %v", v.PrincipalName, got, want)
		}
	}
	rows, err := db.AccountAccess(ctx, "gmail")
	if err != nil || len(rows) != 3 {
		t.Errorf("gmail rows %+v %v", rows, err)
	}
	if err := db.SetAccountAccess(ctx, "nope", agent.PrincipalID, false, nil, &yes, e.action("account.access")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("no such account: %v", err)
	}
}
