package store_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Upgrading a database whose identities name transports (0016): every
// name becomes a placeholder transport, every identity a chain of one,
// and identities.transport is gone.
func TestMigrateTransportsToTable(t *testing.T) {
	t.Parallel()
	ownerURL, _ := storetest.URLs(t)
	pool, err := pgxpool.New(t.Context(), ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ctx := t.Context()
	db := store.New(pool)
	if _, to, err := db.MigrateTo(ctx, 16); err != nil || to != 16 {
		t.Fatalf("migrate to 16: %d, %v", to, err)
	}
	account := ulid.New(time.Now())
	if _, err := pool.Exec(ctx, `INSERT INTO accounts (id, name, kind) VALUES ($1, 'cf', 'cf_catchall')`, account); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{} // prefix -> identity id
	for k, x := range []struct{ prefix, transport string }{{"a", "cloudflare"}, {"b", "work.mail"}, {"c", "cloudflare"}} {
		id := ulid.New(time.Now().Add(time.Duration(k) * time.Millisecond))
		if _, err := pool.Exec(ctx, `INSERT INTO identities (id, account_id, zone, prefix, transport, owner_id)
			VALUES ($1, $2, 'example.com', $3, $4, $5)`, id, account, x.prefix, x.transport, systemID); err != nil {
			t.Fatal(err)
		}
		ids[x.prefix] = id
	}

	if _, to, err := db.Migrate(ctx); err != nil || to < 17 {
		t.Fatalf("migrate: %d, %v", to, err)
	}

	trs, err := db.Transports(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tr := range trs {
		names = append(names, tr.Name)
		if !ulid.Valid(tr.ID) || tr.ConfiguredAt != nil || tr.DisabledAt != nil || tr.Host != "" || tr.SecretID != "" ||
			tr.Caps != nil || tr.Kind != "smtp" || tr.Revision != 1 {
			t.Errorf("placeholder %+v", tr)
		}
	}
	if !slices.Equal(names, []string{"cloudflare", "work.mail"}) {
		t.Errorf("transports %v", names)
	}
	for prefix, want := range map[string]string{"a": "cloudflare", "b": "work.mail", "c": "cloudflare"} {
		i, err := db.IdentityByID(ctx, ids[prefix])
		if err != nil {
			t.Fatal(err)
		}
		if i.Transport != want || !slices.Equal(i.Transports, []string{want}) {
			t.Errorf("%s@: transport %q, chain %v; want %s", prefix, i.Transport, i.Transports, want)
		}
	}
	var gone bool
	if err := pool.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'identities' AND column_name = 'transport')`).Scan(&gone); err != nil || !gone {
		t.Errorf("identities.transport still there (%v)", err)
	}
}

// transportEnv is a migrated database with an account, for the tests
// of transports, secrets and chains.
type transportEnv struct {
	t       *testing.T
	db      *store.DB
	account string
	n       int
}

func newTransportEnv(t *testing.T) *transportEnv {
	t.Helper()
	db, _ := storetest.New(t)
	return &transportEnv{t: t, db: db, account: storetest.Account(t, db)}
}

func (e *transportEnv) action(tool string) store.Action {
	e.n++
	at := time.Date(2026, 11, 1, 12, 0, e.n, 0, time.UTC)
	return store.Action{ID: ulid.New(at), Actor: store.SystemPrincipalID, Tool: tool, At: at}
}

// sealed is a stand-in for a secret.Record sealed for user@host:port. The
// store neither encrypts nor checks the ciphertext, so its bytes are
// arbitrary.
func (e *transportEnv) sealed(user, host string, port int, payload string) *secret.Record {
	e.t.Helper()
	aud, err := secret.SMTPS(user, host, port)
	if err != nil {
		e.t.Fatal(err)
	}
	e.n++
	return &secret.Record{
		ID: ulid.New(time.Date(2026, 11, 1, 12, 0, e.n, 0, time.UTC)), Kind: store.SecretSMTPPassword, Audience: aud,
		Wrapper: "local", KeyRef: "0011223344556677", WrappedDEK: []byte("wrapped"), Payload: []byte(payload),
	}
}

func (e *transportEnv) transport(name string) store.Transport {
	e.t.Helper()
	tr, err := e.db.TransportByName(e.t.Context(), name)
	if err != nil {
		e.t.Fatal(err)
	}
	return tr
}

// Configuring a transport: placeholders from the environment, revision
// checks, and a destination that moves only with a secret sealed for it.
func TestConfigureTransport(t *testing.T) {
	t.Parallel()
	e := newTransportEnv(t)
	ctx := t.Context()

	// Several processes starting at once each ensure the same names.
	errs := make(chan error, 4)
	for range 4 {
		go func() { errs <- e.db.EnsureTransports(ctx, []string{"cf", "work", "cf"}) }()
	}
	for range 4 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if err := e.db.EnsureTransports(ctx, []string{"bad name"}); err == nil {
		t.Error("invalid name ensured")
	}
	if all, err := e.db.Transports(ctx, true); err != nil || len(all) != 2 {
		t.Fatalf("transports %v %v", all, err)
	}
	cf := e.transport("cf")
	if cf.ConfiguredAt != nil || cf.Revision != 1 {
		t.Fatalf("placeholder %+v", cf)
	}

	c := store.TransportConfig{Name: "cf", Host: "SMTP.Example.COM", Port: 465, Auth: store.TransportAuthPlain, Username: "user", Caps: []string{}}
	if _, err := e.db.ConfigureTransport(ctx, c, cf.Revision, nil, e.action("transport.set")); !errors.Is(err, store.ErrNeedsSecret) {
		t.Errorf("placeholder without a secret: %v", err)
	}
	if _, err := e.db.ConfigureTransport(ctx, c, cf.Revision, e.sealed("user", "smtp.example.net", 465, "pw"), e.action("transport.set")); err == nil {
		t.Error("secret sealed for another host accepted")
	}
	nilCaps := c
	nilCaps.Caps = nil
	if _, err := e.db.ConfigureTransport(ctx, nilCaps, cf.Revision, e.sealed("user", "smtp.example.com", 465, "pw"), e.action("transport.set")); err == nil {
		t.Error("no caps accepted")
	}
	s1 := e.sealed("user", "smtp.example.com", 465, "pw1")
	got, err := e.db.ConfigureTransport(ctx, c, cf.Revision, s1, e.action("transport.set"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != "smtp.example.com" || got.ConfiguredAt == nil || got.SecretID != s1.ID || got.Revision != 2 || got.Caps == nil {
		t.Errorf("configured %+v", got)
	}
	stored, err := e.db.Secret(ctx, s1.ID)
	if err != nil || stored.Audience != s1.Audience || string(stored.Payload) != "pw1" || stored.Revision != 1 {
		t.Errorf("secret %+v %v", stored, err)
	}

	// The revision read is the one written over.
	c.Caps = []string{"message-id", "message-id"}
	if _, err := e.db.ConfigureTransport(ctx, c, cf.Revision, nil, e.action("transport.set")); !errors.Is(err, store.ErrStale) {
		t.Errorf("stale revision: %v", err)
	}
	// Caps alone keep the secret.
	got, err = e.db.ConfigureTransport(ctx, c, got.Revision, nil, e.action("transport.set"))
	if err != nil || got.SecretID != s1.ID || !slices.Equal(got.Caps, []string{"message-id"}) || got.Revision != 3 {
		t.Fatalf("caps: %+v %v", got, err)
	}
	// Any part of the destination needs a new secret.
	for _, move := range []func(*store.TransportConfig){
		func(c *store.TransportConfig) { c.Host = "smtp.example.net" },
		func(c *store.TransportConfig) { c.Port = 587 },
		func(c *store.TransportConfig) { c.Username = "other" },
	} {
		m := c
		move(&m)
		if _, err := e.db.ConfigureTransport(ctx, m, got.Revision, nil, e.action("transport.set")); !errors.Is(err, store.ErrNeedsSecret) {
			t.Errorf("moved to %+v without a secret: %v", m, err)
		}
	}
	c.Host = "smtp.example.net"
	s2 := e.sealed("user", "smtp.example.net", 465, "pw2")
	if got, err = e.db.ConfigureTransport(ctx, c, got.Revision, s2, e.action("transport.set")); err != nil || got.SecretID != s2.ID {
		t.Fatalf("moved with a secret: %+v %v", got, err)
	}
	if _, err := e.db.Secret(ctx, s1.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("old secret: %v", err)
	}
	// No authentication, no secret.
	none := store.TransportConfig{Name: "cf", Host: "smtp.example.net", Port: 465, Auth: store.TransportAuthNone, Caps: []string{}}
	if _, err := e.db.ConfigureTransport(ctx, none, got.Revision, e.sealed("user", "smtp.example.net", 465, "x"), e.action("transport.set")); err == nil {
		t.Error("secret for auth none accepted")
	}
	if got, err = e.db.ConfigureTransport(ctx, none, got.Revision, nil, e.action("transport.set")); err != nil || got.SecretID != "" {
		t.Fatalf("auth none: %+v %v", got, err)
	}
	if _, err := e.db.Secret(ctx, s2.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("secret after auth none: %v", err)
	}

	// Creating names a new transport.
	if _, err := e.db.ConfigureTransport(ctx, none, 0, nil, e.action("transport.add")); !errors.Is(err, store.ErrDuplicate) {
		t.Errorf("create over an existing name: %v", err)
	}
	none.Name = "fresh"
	if got, err = e.db.ConfigureTransport(ctx, none, 0, nil, e.action("transport.add")); err != nil || got.Revision != 1 || got.ConfiguredAt == nil {
		t.Errorf("create: %+v %v", got, err)
	}
	none.Name = "absent"
	if _, err := e.db.ConfigureTransport(ctx, none, 1, nil, e.action("transport.set")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("rewrite of a missing transport: %v", err)
	}
}

// A secret is rewritten in place only for its own destination, at the
// revision read; sealed_at follows the payload, not a rewrap.
func TestUpdateSecret(t *testing.T) {
	t.Parallel()
	e := newTransportEnv(t)
	ctx := t.Context()
	s := e.sealed("user", "smtp.example.com", 465, "pw1")
	c := store.TransportConfig{Name: "cf", Host: "smtp.example.com", Port: 465, Auth: store.TransportAuthPlain, Username: "user", Caps: []string{}}
	if _, err := e.db.ConfigureTransport(ctx, c, 0, s, e.action("transport.add")); err != nil {
		t.Fatal(err)
	}
	cur, err := e.db.Secret(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}

	rewrapped := cur.Record
	rewrapped.KeyRef, rewrapped.WrappedDEK = "8899aabbccddeeff", []byte("rewrapped")
	rev, err := e.db.UpdateSecret(ctx, rewrapped, cur.Revision, e.action("secret.rewrap"))
	if err != nil || rev != 2 {
		t.Fatalf("rewrap: %d %v", rev, err)
	}
	after, err := e.db.Secret(ctx, s.ID)
	if err != nil || after.KeyRef != "8899aabbccddeeff" || !after.SealedAt.Equal(cur.SealedAt) || string(after.Payload) != "pw1" {
		t.Errorf("after rewrap %+v %v", after, err)
	}
	if _, err := e.db.UpdateSecret(ctx, rewrapped, cur.Revision, e.action("secret.rewrap")); !errors.Is(err, store.ErrStale) {
		t.Errorf("stale revision: %v", err)
	}
	moved := after.Record
	moved.Audience = "smtps://user@smtp.example.net:465"
	if _, err := e.db.UpdateSecret(ctx, moved, after.Revision, e.action("secret.set")); err == nil {
		t.Error("audience changed in place")
	}
	rotated := after.Record
	rotated.Payload = []byte("pw2")
	if rev, err = e.db.UpdateSecret(ctx, rotated, after.Revision, e.action("secret.set")); err != nil || rev != 3 {
		t.Fatalf("rotate: %d %v", rev, err)
	}
	if final, err := e.db.Secret(ctx, s.ID); err != nil || final.SealedAt.Equal(after.SealedAt) || string(final.Payload) != "pw2" {
		t.Errorf("after rotate %+v %v", final, err)
	}
	missing := rotated
	missing.ID = ulid.New(time.Now())
	if _, err := e.db.UpdateSecret(ctx, missing, 1, e.action("secret.set")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing secret: %v", err)
	}
	if all, err := e.db.Secrets(ctx); err != nil || len(all) != 1 {
		t.Errorf("secrets %v %v", all, err)
	}
}

// Removing a transport disables it and erases its secret; the row stays,
// so the names in send history still resolve.
func TestDisableTransport(t *testing.T) {
	t.Parallel()
	e := newTransportEnv(t)
	ctx := t.Context()
	s := e.sealed("user", "smtp.example.com", 465, "pw")
	c := store.TransportConfig{Name: "cf", Host: "smtp.example.com", Port: 465, Auth: store.TransportAuthPlain, Username: "user", Caps: []string{"dkim"}}
	tr, err := e.db.ConfigureTransport(ctx, c, 0, s, e.action("transport.add"))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.DisableTransport(ctx, "cf", tr.Revision+1, e.action("transport.rm")); !errors.Is(err, store.ErrStale) {
		t.Errorf("stale revision: %v", err)
	}
	if err := e.db.DisableTransport(ctx, "cf", tr.Revision, e.action("transport.rm")); err != nil {
		t.Fatal(err)
	}
	if err := e.db.DisableTransport(ctx, "cf", tr.Revision+1, e.action("transport.rm")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second rm: %v", err)
	}
	gone := e.transport("cf")
	if gone.DisabledAt == nil || gone.SecretID != "" || gone.Host != "smtp.example.com" || gone.Username != "user" || gone.Revision != tr.Revision+1 {
		t.Errorf("disabled %+v", gone)
	}
	if _, err := e.db.Secret(ctx, s.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("secret after rm: %v", err)
	}
	if enabled, err := e.db.Transports(ctx, false); err != nil || len(enabled) != 0 {
		t.Errorf("enabled %v %v", enabled, err)
	}
	// Placeholders do not come back over a removed transport.
	if err := e.db.EnsureTransports(ctx, []string{"cf"}); err != nil || e.transport("cf").DisabledAt == nil {
		t.Errorf("ensure over a removed transport: %v", err)
	}
	// Configuring it again enables it, with a secret again.
	if _, err := e.db.ConfigureTransport(ctx, c, gone.Revision, nil, e.action("transport.set")); !errors.Is(err, store.ErrNeedsSecret) {
		t.Errorf("re-enable without a secret: %v", err)
	}
	back, err := e.db.ConfigureTransport(ctx, c, gone.Revision, e.sealed("user", "smtp.example.com", 465, "pw"), e.action("transport.set"))
	if err != nil || back.DisabledAt != nil || back.SecretID == "" {
		t.Errorf("re-enabled %+v %v", back, err)
	}
}

// An identity's chain: contiguous positions from 0, at most eight
// enabled transports, each once.
func TestIdentityChain(t *testing.T) {
	t.Parallel()
	e := newTransportEnv(t)
	ctx := t.Context()
	var names []string
	for k := range 9 {
		names = append(names, fmt.Sprintf("t%d", k))
	}
	storetest.Transports(t, e.db, names...)

	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	i := store.Identity{ID: ulid.New(at), AccountID: e.account, Zone: "example.com", Prefix: "me", Transport: "nope", OwnerID: systemID, CreatedAt: at}
	if err := e.db.AddIdentity(ctx, i, e.action("identity.add")); !errors.Is(err, store.ErrNoTransport) {
		t.Fatalf("unknown transport: %v", err)
	}
	if _, err := e.db.IdentityByID(ctx, i.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("identity left behind: %v", err)
	}
	i.Transport = "t0"
	if err := e.db.AddIdentity(ctx, i, e.action("identity.add")); err != nil {
		t.Fatal(err)
	}

	chain := func() []string {
		t.Helper()
		trs, err := e.db.IdentityTransports(ctx, i.ID)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, tr := range trs {
			out = append(out, tr.Name)
		}
		return out
	}
	if got := chain(); !slices.Equal(got, []string{"t0"}) {
		t.Errorf("chain %v", got)
	}
	for _, bad := range [][]string{nil, names, {"t1", "t1"}, {"t1", "nope"}} {
		if err := e.db.SetIdentityTransports(ctx, i.ID, bad, e.action("identity.route")); err == nil {
			t.Errorf("chain %v accepted", bad)
		}
	}
	if err := e.db.SetIdentityTransports(ctx, i.ID, []string{"t1", "nope"}, e.action("identity.route")); !errors.Is(err, store.ErrNoTransport) {
		t.Errorf("unknown member: %v", err)
	}
	if err := e.db.SetIdentityTransports(ctx, i.ID, names[:8], e.action("identity.route")); err != nil {
		t.Fatal(err)
	}
	if got := chain(); !slices.Equal(got, names[:8]) {
		t.Errorf("chain of eight %v", got)
	}
	if err := e.db.SetIdentityTransports(ctx, i.ID, []string{"t5", "t2"}, e.action("identity.route")); err != nil {
		t.Fatal(err)
	}
	got, err := e.db.IdentityByID(ctx, i.ID)
	if err != nil || got.Transport != "t5" || !slices.Equal(got.Transports, []string{"t5", "t2"}) {
		t.Errorf("identity %+v %v", got, err)
	}
	// A disabled transport cannot join a chain.
	t3 := e.transport("t3")
	if err := e.db.DisableTransport(ctx, "t3", t3.Revision, e.action("transport.rm")); err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetIdentityTransports(ctx, i.ID, []string{"t3"}, e.action("identity.route")); !errors.Is(err, store.ErrNoTransport) {
		t.Errorf("disabled member: %v", err)
	}
	if err := e.db.DisableIdentity(ctx, i.ID, e.action("identity.disable")); err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetIdentityTransports(ctx, i.ID, []string{"t0"}, e.action("identity.route")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("disabled identity: %v", err)
	}
}
