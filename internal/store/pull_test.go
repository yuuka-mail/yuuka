package store_test

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// pullEnv is a migrated database with a person, for the tests of pull
// accounts.
type pullEnv struct {
	t    *testing.T
	db   *store.DB
	pool *pgxpool.Pool
	op   string // a human principal's id
	n    int
}

func newPullEnv(t *testing.T) *pullEnv {
	t.Helper()
	db, pool := storetest.New(t)
	e := &pullEnv{t: t, db: db, pool: pool}
	e.op = e.principal("human", "operator")
	return e
}

func (e *pullEnv) at() time.Time {
	e.n++
	return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(e.n) * time.Second)
}

func (e *pullEnv) principal(kind, name string) string {
	e.t.Helper()
	at := e.at()
	p := store.Principal{ID: ulid.New(at), Kind: kind, Name: name, DisplayName: name, CreatedAt: at}
	if err := e.db.CreatePrincipal(e.t.Context(), p, store.Action{ID: ulid.New(at), Tool: "principal.create", At: at}); err != nil {
		e.t.Fatal(err)
	}
	return p.ID
}

func (e *pullEnv) action(tool string) store.Action {
	at := e.at()
	return store.Action{ID: ulid.New(at), Actor: e.op, Tool: tool, At: at}
}

// sealed is a stand-in for an imap_password sealed for user@host:port;
// the store never looks inside it.
func (e *pullEnv) sealed(user, host string, port int, payload string) *secret.Record {
	e.t.Helper()
	aud, err := secret.IMAPS(user, host, port)
	if err != nil {
		e.t.Fatal(err)
	}
	return &secret.Record{
		ID: ulid.New(e.at()), Kind: store.SecretIMAPPassword, Audience: aud,
		Wrapper: "local", KeyRef: "0011223344556677", WrappedDEK: []byte("wrapped"), Payload: []byte(payload),
	}
}

func gmailConfig(addr string) store.SourceConfig {
	return store.SourceConfig{
		Address: addr, Host: "imap.example.com", Port: 993, Auth: store.SourceAuthPlain, Username: addr,
		Preset: store.PresetGmail, TrustedAuthservID: "mx.example.com", MaxMessageBytes: 50 << 20,
		DailyByteBudget: 2 << 30, PollSeconds: 60,
	}
}

// add makes the pull account name for address, readable by e.op.
func (e *pullEnv) add(name, address string) store.Source {
	e.t.Helper()
	c := gmailConfig(address)
	s, err := e.db.AddPullAccount(e.t.Context(), name, c, *e.sealed(c.Username, c.Host, c.Port, "pw"), e.op, e.action("account.add"))
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *pullEnv) mailbox(account, name string, kind store.KeyKind, uidvalidity, high uint32) store.Mailbox {
	e.t.Helper()
	m, err := e.db.StartMailbox(e.t.Context(), account, store.MailboxStart{Name: name, KeyKind: kind, UIDValidity: uidvalidity, High: high})
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

// pull is the commit of raw, fetched from loc of account under key.
func (e *pullEnv) pull(account, key string, loc store.RemoteLocation, raw []byte) store.Pull {
	at := e.at()
	id := ulid.New(at)
	sum, hash := sha256.Sum256(raw), sha256.Sum256(append([]byte("content:"), raw...))
	return store.Pull{
		RawID: id, R2Key: fmt.Sprintf("pull/2026/11/%s.eml", id), LocalPath: fmt.Sprintf("pull/2026/11/%s.eml", id),
		Size: int64(len(raw)), SHA256: sum, ContentHash: &hash, InternalDate: at.Add(-time.Hour),
		DeliveryID: ulid.New(at), AccountID: account, SourceKey: key, RemoteLocation: loc,
	}
}

// 0019 applies to a deployment with mail (0018): every existing account
// is a Cloudflare account that stays readable by default, its deliveries
// gain no remote facts, and the new constraints hold.
func TestMigratePullAccounts(t *testing.T) {
	t.Parallel()
	ownerURL, _ := storetest.URLs(t)
	pool, err := pgxpool.New(t.Context(), ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ctx := t.Context()
	db := store.New(pool)
	if _, to, err := db.MigrateTo(ctx, 18); err != nil || to != 18 {
		t.Fatalf("migrate to 18: %d, %v", to, err)
	}
	acct := oldAccount(t, pool, "cloudflare")
	if _, err := db.CommitIngest(ctx, record(acct, 1, sha256.Sum256([]byte("c")), "a@example.com")); err != nil {
		t.Fatal(err)
	}
	if _, to, err := db.Migrate(ctx); err != nil || to < 19 {
		t.Fatalf("migrate: %d, %v", to, err)
	}

	var kind, sync, writeback string
	var defaultRead, offload bool
	if err := pool.QueryRow(ctx, `SELECT kind, default_read, sync_policy, writeback_policy, offload_policy IS NULL
		FROM accounts WHERE id = $1`, acct).Scan(&kind, &defaultRead, &sync, &writeback, &offload); err != nil {
		t.Fatal(err)
	}
	if kind != store.AccountCloudflare || !defaultRead || sync != "full" || writeback != "off" || !offload {
		t.Errorf("account after 0019: %s default_read=%v %s %s offload-null=%v", kind, defaultRead, sync, writeback, offload)
	}
	if n := count(t, pool, `SELECT count(*) FROM deliveries WHERE remote_state IS NULL AND envelope_from IS NOT NULL`); n != 1 {
		t.Errorf("%d push deliveries as they were", n)
	}
	// It is still the account YUUKA_ACCOUNT names, and still deduplicates.
	if id, err := db.EnsureCloudflareAccount(ctx, "cloudflare"); err != nil || id != acct {
		t.Fatalf("ensure: %s %v", id, err)
	}
	if out, err := db.CommitIngest(ctx, record(acct, 2, sha256.Sum256([]byte("c")), "a@example.com")); err != nil || out != store.Redelivery {
		t.Errorf("redelivery after the upgrade: %v %v", out, err)
	}

	raw, del := ulid.New(time.Now()), ulid.New(time.Now())
	if _, err := pool.Exec(ctx, `INSERT INTO raw_objects (id, r2_key, local_path, size, sha256, received_at)
		VALUES ($1, 'pull/2026/11/'||$1||'.eml', 'x', 1, $2, now())`, raw, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		// A new account must say whether it is readable by default.
		`INSERT INTO accounts (id, name, kind) VALUES ('` + ulid.New(time.Now()) + `', 'nodefault', 'imap')`,
		`INSERT INTO accounts (id, name, kind, default_read) VALUES ('` + ulid.New(time.Now()) + `', 'api', 'gmail_api', false)`,
		`UPDATE accounts SET sync_policy = 'metadata'`,
		`UPDATE accounts SET writeback_policy = 'read'`,
		`UPDATE accounts SET offload_policy = '{}'`,
		// No envelope sender without a remote location.
		`INSERT INTO deliveries (id, account_id, source_key, raw_id, zone, prefix, envelope_to, received_at)
			VALUES ('` + del + `', '` + acct + `', 'k', '` + raw + `', 'example.com', 'a', 'a@example.com', now())`,
		// A location is whole and has a state.
		`INSERT INTO deliveries (id, account_id, source_key, raw_id, zone, prefix, envelope_to, received_at, remote_mailbox, remote_uidvalidity, remote_uid)
			VALUES ('` + del + `', '` + acct + `', 'k', '` + raw + `', 'example.com', 'a', 'a@example.com', now(), 'INBOX', 1, 1)`,
		`INSERT INTO deliveries (id, account_id, source_key, raw_id, zone, prefix, envelope_to, received_at, remote_mailbox, remote_uid, remote_state)
			VALUES ('` + del + `', '` + acct + `', 'k', '` + raw + `', 'example.com', 'a', 'a@example.com', now(), 'INBOX', 1, 1)`,
		// Only present until M3.6; offloaded_at only when offloaded.
		`INSERT INTO deliveries (id, account_id, source_key, raw_id, zone, prefix, envelope_to, received_at, remote_mailbox, remote_uidvalidity, remote_uid, remote_state)
			VALUES ('` + del + `', '` + acct + `', 'k', '` + raw + `', 'example.com', 'a', 'a@example.com', now(), 'INBOX', 1, 1, 2)`,
		`INSERT INTO deliveries (id, account_id, source_key, raw_id, zone, prefix, envelope_to, received_at, remote_mailbox, remote_uidvalidity, remote_uid, remote_state, offloaded_at)
			VALUES ('` + del + `', '` + acct + `', 'k', '` + raw + `', 'example.com', 'a', 'a@example.com', now(), 'INBOX', 1, 1, 1, now())`,
		// Pulled facts on a push delivery.
		`INSERT INTO deliveries (id, account_id, source_key, raw_id, zone, prefix, envelope_from, envelope_to, received_at, remote_labels)
			VALUES ('` + del + `', '` + acct + `', 'k', '` + raw + `', 'example.com', 'a', '', 'a@example.com', now(), '{x}')`,
	} {
		if _, err := pool.Exec(ctx, bad); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
	// What a pulled delivery is.
	if _, err := pool.Exec(ctx, `INSERT INTO deliveries (id, account_id, source_key, raw_id, zone, prefix, envelope_to, received_at,
			remote_mailbox, remote_uidvalidity, remote_uid, remote_state, remote_labels, remote_omitted)
		VALUES ($1, $2, 'gm:0000000000000001', $3, 'example.com', 'a', 'a@example.com', now(), 'INBOX', 1, 1, 1, '{}', 1)`,
		del, acct, raw); err != nil {
		t.Errorf("pulled delivery: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO secrets (id, kind, audience, wrapper, key_ref, wrapped_dek, payload)
		VALUES ($1, 'imap_password', 'imaps://u@imap.example.com:993', 'local', 'k', '\x01', '\x01')`, ulid.New(time.Now())); err != nil {
		t.Errorf("imap_password: %v", err)
	}
}

// Adding, configuring, pausing and removing a pull source: revisions,
// destinations that move only with a password sealed for them, the
// adder's access row, and names and addresses that stay taken.
func TestPullSource(t *testing.T) {
	t.Parallel()
	e := newPullEnv(t)
	ctx := t.Context()
	c := gmailConfig("Me@Example.COM")

	if _, err := e.db.AddPullAccount(ctx, "gmail", c, *e.sealed(c.Username, "imap.example.net", c.Port, "pw"), e.op, e.action("account.add")); err == nil {
		t.Error("password sealed for another host accepted")
	}
	smtp, _ := secret.SMTPS(c.Username, c.Host, c.Port)
	wrongKind := e.sealed(c.Username, c.Host, c.Port, "pw")
	wrongKind.Kind, wrongKind.Audience = store.SecretSMTPPassword, smtp
	if _, err := e.db.AddPullAccount(ctx, "gmail", c, *wrongKind, e.op, e.action("account.add")); err == nil {
		t.Error("smtp_password accepted")
	}
	if _, err := e.db.AddPullAccount(ctx, "gmail", c, *e.sealed(c.Username, c.Host, c.Port, "pw"), ulid.New(time.Now()), e.action("account.add")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("no such reader: %v", err)
	}
	for _, bad := range []func(*store.SourceConfig){
		func(c *store.SourceConfig) { c.Address = "no-at" },
		func(c *store.SourceConfig) { c.Auth = "xoauth2" },
		func(c *store.SourceConfig) { c.Preset = "outlook" },
		func(c *store.SourceConfig) { c.PollSeconds = 1 },
		func(c *store.SourceConfig) { c.MaxMessageBytes = 0 },
		func(c *store.SourceConfig) { c.Mailboxes = []string{} },
		func(c *store.SourceConfig) { c.Mailboxes = []string{"INBOX", "INBOX"} },
		func(c *store.SourceConfig) { c.TrustedAuthservID = "mx example" },
	} {
		b := c
		bad(&b)
		if _, err := store.CheckSourceConfig(b); err == nil {
			t.Errorf("accepted %+v", b)
		}
	}

	pw := e.sealed(c.Username, c.Host, c.Port, "pw1")
	src, err := e.db.AddPullAccount(ctx, "gmail", c, *pw, e.op, e.action("account.add"))
	if err != nil {
		t.Fatal(err)
	}
	if src.Address != "me@example.com" || src.Zone() != "example.com" || src.Prefix() != "me" || src.SecretID != pw.ID ||
		src.Revision != 1 || src.AccountName != "gmail" || src.Kind != "imap" {
		t.Errorf("added %+v", src)
	}
	acct, err := e.db.AccountByName(ctx, "gmail")
	if err != nil || acct.Kind != store.AccountIMAP || acct.DefaultRead || acct.ID != src.AccountID {
		t.Errorf("account %+v %v", acct, err)
	}
	if rows, err := e.db.AccountAccess(ctx, "gmail"); err != nil || len(rows) != 1 || rows[0].PrincipalID != e.op || !rows[0].CanRead {
		t.Errorf("access %+v %v", rows, err)
	}
	// The name and the address are taken.
	if _, err := e.db.AddPullAccount(ctx, "gmail", gmailConfig("other@example.com"), *e.sealed("other@example.com", c.Host, c.Port, "pw"), e.op, e.action("account.add")); !errors.Is(err, store.ErrDuplicate) {
		t.Errorf("same name: %v", err)
	}
	if _, err := e.db.AddPullAccount(ctx, "gmail2", c, *e.sealed(c.Username, c.Host, c.Port, "pw"), e.op, e.action("account.add")); !errors.Is(err, store.ErrDuplicate) {
		t.Errorf("same address: %v", err)
	}
	if _, err := e.db.EnsureCloudflareAccount(ctx, "gmail"); err == nil {
		t.Error("a pull account taken for the Cloudflare one")
	}

	// Settings that keep the destination keep the password.
	c2 := c
	c2.PollSeconds, c2.Mailboxes = 120, []string{"[Gmail]/全部邮件"}
	if _, err := e.db.ConfigureSource(ctx, "gmail", c2, src.Revision+1, nil, e.action("account.set")); !errors.Is(err, store.ErrStale) {
		t.Errorf("stale revision: %v", err)
	}
	src, err = e.db.ConfigureSource(ctx, "gmail", c2, src.Revision, nil, e.action("account.set"))
	if err != nil || src.SecretID != pw.ID || src.PollSeconds != 120 || !slices.Equal(src.Mailboxes, c2.Mailboxes) || src.Revision != 2 {
		t.Fatalf("set %+v %v", src, err)
	}
	// Any part of the destination needs a new password.
	for _, move := range []func(*store.SourceConfig){
		func(c *store.SourceConfig) { c.Host = "imap.example.net" },
		func(c *store.SourceConfig) { c.Port = 994 },
		func(c *store.SourceConfig) { c.Username = "other" },
	} {
		m := c2
		move(&m)
		if _, err := e.db.ConfigureSource(ctx, "gmail", m, src.Revision, nil, e.action("account.set")); !errors.Is(err, store.ErrNeedsSecret) {
			t.Errorf("moved without a password: %+v %v", m, err)
		}
	}
	moved := c2
	moved.Host = "imap.example.net"
	pw2 := e.sealed(moved.Username, moved.Host, moved.Port, "pw2")
	src, err = e.db.ConfigureSource(ctx, "gmail", moved, src.Revision, pw2, e.action("account.set"))
	if err != nil || src.SecretID != pw2.ID {
		t.Fatalf("moved: %+v %v", src, err)
	}
	if _, err := e.db.Secret(ctx, pw.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("old password kept: %v", err)
	}

	// The worker pauses for a refused login only at the revision it
	// logged in with; a new password ends that pause, an operator's
	// pause only resume ends.
	if err := e.db.PauseSource(ctx, "gmail", src.Revision-1, store.PauseAuthFailed, e.action("account.pause")); !errors.Is(err, store.ErrStale) {
		t.Errorf("stale auth failure: %v", err)
	}
	if err := e.db.PauseSource(ctx, "gmail", src.Revision, store.PauseAuthFailed, e.action("account.pause")); err != nil {
		t.Fatal(err)
	}
	src = e.source("gmail")
	if src.PauseReason != store.PauseAuthFailed || src.PausedAt == nil {
		t.Fatalf("paused %+v", src)
	}
	pw3 := e.sealed(moved.Username, moved.Host, moved.Port, "pw3")
	if src, err = e.db.ConfigureSource(ctx, "gmail", moved, src.Revision, pw3, e.action("account.set")); err != nil || src.PausedAt != nil {
		t.Fatalf("new password: %+v %v", src, err)
	}
	if err := e.db.PauseSource(ctx, "gmail", src.Revision, store.PauseOperator, e.action("account.pause")); err != nil {
		t.Fatal(err)
	}
	src = e.source("gmail")
	if err := e.db.PauseSource(ctx, "gmail", src.Revision, store.PauseAuthFailed, e.action("account.pause")); err != nil {
		t.Fatal(err)
	}
	pw4 := e.sealed(moved.Username, moved.Host, moved.Port, "pw4")
	if src, err = e.db.ConfigureSource(ctx, "gmail", moved, e.source("gmail").Revision, pw4, e.action("account.set")); err != nil ||
		src.PauseReason != store.PauseOperator {
		t.Fatalf("operator's pause: %+v %v", src, err)
	}
	if err := e.db.ResumeSource(ctx, "gmail", src.Revision, e.action("account.resume")); err != nil {
		t.Fatal(err)
	}
	if src = e.source("gmail"); src.PausedAt != nil {
		t.Errorf("resumed %+v", src)
	}
	all, err := e.db.EnabledSources(ctx)
	if err != nil || len(all) != 1 || all[0].Secret.ID != pw4.ID || string(all[0].Secret.Payload) != "pw4" {
		t.Errorf("enabled %+v %v", all, err)
	}

	// Removal keeps the account and its name, erases the password.
	if err := e.db.DisableSource(ctx, "gmail", src.Revision, e.action("account.rm")); err != nil {
		t.Fatal(err)
	}
	if src = e.source("gmail"); src.DisabledAt == nil || src.SecretID != "" {
		t.Errorf("removed %+v", src)
	}
	if _, err := e.db.Secret(ctx, pw4.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("password kept: %v", err)
	}
	if a, err := e.db.AccountByName(ctx, "gmail"); err != nil || a.DisabledAt == nil {
		t.Errorf("account %+v %v", a, err)
	}
	if _, err := e.db.ConfigureSource(ctx, "gmail", moved, src.Revision, e.sealed(moved.Username, moved.Host, moved.Port, "pw"), e.action("account.set")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("removed source set again: %v", err)
	}
	if all, err := e.db.EnabledSources(ctx); err != nil || len(all) != 0 {
		t.Errorf("enabled after rm %+v %v", all, err)
	}
	if _, err := e.db.AddPullAccount(ctx, "gmail", c, *e.sealed(c.Username, c.Host, c.Port, "pw"), e.op, e.action("account.add")); !errors.Is(err, store.ErrDuplicate) {
		t.Errorf("name reused: %v", err)
	}
	// The address is free again, under another name.
	e.add("gmail-again", "me@example.com")

	if n := count(t, e.pool, `SELECT count(*) FROM actions WHERE tool LIKE 'account.%'`); n != 10 {
		t.Errorf("%d account actions", n)
	}
}

func (e *pullEnv) source(name string) store.Source {
	e.t.Helper()
	s, err := e.db.SourceByName(e.t.Context(), name)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

// The source keys are a one-way door (§10.1): their spelling is fixed
// here.
func TestSourceKeys(t *testing.T) {
	t.Parallel()
	if k := store.GmailSourceKey(0x16b2ee0d6f8e4f3a); k != "gm:16b2ee0d6f8e4f3a" {
		t.Error(k)
	}
	if k := store.GmailSourceKey(1); k != "gm:0000000000000001" {
		t.Error(k)
	}
	if k, err := store.ObjectIDSourceKey("M6d99ac3275bb4e"); err != nil || k != "oid:M6d99ac3275bb4e" {
		t.Error(k, err)
	}
	for _, bad := range []string{"", "a b", "a:b", strings.Repeat("a", 256)} {
		if _, err := store.ObjectIDSourceKey(bad); err == nil {
			t.Errorf("objectid %q accepted", bad)
		}
	}
	if k := store.LocationSourceKey(1700000000, 42, "Archive:2026/日本"); k != "loc:1700000000:42:Archive:2026/日本" {
		t.Error(k)
	}
}

// One pulled message's commit (§10.1.3): delivered to the mailbox's own
// address with no envelope sender, idempotent for the same raw, a
// duplicate for the same source key, header only when it says why, and
// refused for a folder that moved on or a key of the wrong kind.
func TestCommitPull(t *testing.T) {
	t.Parallel()
	e := newPullEnv(t)
	ctx := t.Context()
	src := e.add("gmail", "me@example.com")
	e.mailbox(src.AccountID, "[Gmail]/All Mail", store.KeyGmail, 7, 100)

	loc := store.RemoteLocation{Mailbox: "[Gmail]/All Mail", UIDValidity: 7, UID: 101}
	p := e.pull(src.AccountID, store.GmailSourceKey(0xabc), loc, []byte("one"))
	p.Labels = []string{`\Inbox`, "旅行"}
	if out, err := e.db.CommitPull(ctx, p); err != nil || out != store.Delivered {
		t.Fatalf("commit: %v %v", out, err)
	}
	var zone, prefix, to, mailbox string
	var from *string
	var uidvalidity, uid int64
	var state int
	var labels []string
	var received time.Time
	var completeness int
	if err := e.pool.QueryRow(ctx, `
		SELECT d.zone, d.prefix, d.envelope_from, d.envelope_to, d.received_at, d.remote_mailbox, d.remote_uidvalidity,
			d.remote_uid, d.remote_state, d.remote_labels, r.completeness
		FROM deliveries d JOIN raw_objects r ON r.id = d.raw_id WHERE d.id = $1`, p.DeliveryID).Scan(
		&zone, &prefix, &from, &to, &received, &mailbox, &uidvalidity, &uid, &state, &labels, &completeness); err != nil {
		t.Fatal(err)
	}
	if zone != "example.com" || prefix != "me" || from != nil || to != "me@example.com" || !received.Equal(p.InternalDate) ||
		mailbox != loc.Mailbox || uidvalidity != 7 || uid != 101 || state != 1 || !slices.Equal(labels, p.Labels) || completeness != 1 {
		t.Errorf("delivery %s %s %v %s %v %s %d %d %d %v %d", zone, prefix, from, to, received, mailbox, uidvalidity, uid, state, labels, completeness)
	}
	// The same raw again (an ack lost after the commit).
	if out, err := e.db.CommitPull(ctx, p); err != nil || out != store.AlreadyIngested {
		t.Errorf("again: %v %v", out, err)
	}
	// Another raw with the same key: the race the source-key filter
	// lost, or a crash between the commit and the cursor.
	q := e.pull(src.AccountID, p.SourceKey, loc, []byte("one"))
	if out, err := e.db.CommitPull(ctx, q); err != nil || out != store.Redelivery {
		t.Errorf("same key: %v %v", out, err)
	}
	var dup string
	if err := e.pool.QueryRow(ctx, `SELECT duplicate_of FROM raw_objects WHERE id = $1`, q.RawID).Scan(&dup); err != nil || dup != p.RawID {
		t.Errorf("duplicate_of %s %v", dup, err)
	}

	// Too large: the header only, and why.
	h := e.pull(src.AccountID, store.GmailSourceKey(0xabd), store.RemoteLocation{Mailbox: loc.Mailbox, UIDValidity: 7, UID: 102}, []byte("head"))
	h.ContentHash = nil
	if _, err := e.db.CommitPull(ctx, h); err == nil {
		t.Error("header only without a reason")
	}
	h.Omitted = store.OmittedTooLarge
	if out, err := e.db.CommitPull(ctx, h); err != nil || out != store.Delivered {
		t.Fatalf("header only: %v %v", out, err)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM deliveries d JOIN raw_objects r ON r.id = d.raw_id
		WHERE d.id = $1 AND r.completeness = 2 AND r.content_hash IS NULL AND d.remote_omitted = 1 AND d.remote_labels IS NULL`, h.DeliveryID); n != 1 {
		t.Error("header-only delivery not as committed")
	}
	full := e.pull(src.AccountID, store.GmailSourceKey(0xabe), store.RemoteLocation{Mailbox: loc.Mailbox, UIDValidity: 7, UID: 103}, []byte("x"))
	full.Omitted = store.OmittedUnfetchable
	if _, err := e.db.CommitPull(ctx, full); err == nil {
		t.Error("a full raw said to be header only")
	}

	for name, bad := range map[string]store.Pull{
		"stale uidvalidity": e.pull(src.AccountID, store.GmailSourceKey(1), store.RemoteLocation{Mailbox: loc.Mailbox, UIDValidity: 6, UID: 5}, []byte("y")),
		"location key":      e.pull(src.AccountID, store.LocationSourceKey(7, 5, loc.Mailbox), store.RemoteLocation{Mailbox: loc.Mailbox, UIDValidity: 7, UID: 5}, []byte("y")),
		"short gm key":      e.pull(src.AccountID, "gm:abc", store.RemoteLocation{Mailbox: loc.Mailbox, UIDValidity: 7, UID: 5}, []byte("y")),
		"unknown mailbox":   e.pull(src.AccountID, store.GmailSourceKey(2), store.RemoteLocation{Mailbox: "INBOX", UIDValidity: 7, UID: 5}, []byte("y")),
		"not a pull account": e.pull(storetest.Account(t, e.db), store.GmailSourceKey(3),
			store.RemoteLocation{Mailbox: loc.Mailbox, UIDValidity: 7, UID: 5}, []byte("y")),
	} {
		if out, err := e.db.CommitPull(ctx, bad); err == nil {
			t.Errorf("%s: %v", name, out)
		}
	}
	if _, err := e.db.CommitPull(ctx, e.pull(src.AccountID, store.GmailSourceKey(1), store.RemoteLocation{Mailbox: loc.Mailbox, UIDValidity: 6, UID: 5}, []byte("y"))); !errors.Is(err, store.ErrStaleMailbox) {
		t.Errorf("stale: %v", err)
	}

	// A location-keyed folder takes exactly its location's key.
	e.mailbox(src.AccountID, "Archive", store.KeyLocation, 9, 0)
	at := store.RemoteLocation{Mailbox: "Archive", UIDValidity: 9, UID: 1}
	if _, err := e.db.CommitPull(ctx, e.pull(src.AccountID, store.LocationSourceKey(9, 2, "Archive"), at, []byte("z"))); !errors.Is(err, store.ErrSourceKey) {
		t.Errorf("another location's key: %v", err)
	}
	if out, err := e.db.CommitPull(ctx, e.pull(src.AccountID, store.LocationSourceKey(9, 1, "Archive"), at, []byte("z"))); err != nil || out != store.Delivered {
		t.Errorf("location key: %v %v", out, err)
	}

	// Concurrent commits of one key deliver once.
	key := store.GmailSourceKey(0xfff)
	var wg sync.WaitGroup
	outs := make(chan store.Outcome, 8)
	pulls := make([]store.Pull, 8)
	for i := range pulls {
		pulls[i] = e.pull(src.AccountID, key, store.RemoteLocation{Mailbox: loc.Mailbox, UIDValidity: 7, UID: 200}, []byte("race"))
	}
	for _, p := range pulls {
		wg.Go(func() {
			out, err := e.db.CommitPull(ctx, p)
			if err != nil {
				t.Error(err)
			}
			outs <- out
		})
	}
	wg.Wait()
	close(outs)
	delivered := 0
	for o := range outs {
		if o == store.Delivered {
			delivered++
		}
	}
	if delivered != 1 || count(t, e.pool, `SELECT count(*) FROM deliveries WHERE source_key = $1`, key) != 1 {
		t.Errorf("%d delivered", delivered)
	}
}

// The cursors (§10.1.2, §10.1.3) move only forward, only under the
// UIDVALIDITY that was read, and only by compare-and-set; together with
// the commit order they keep the crash table of §10.1.3 true.
func TestPullCursors(t *testing.T) {
	t.Parallel()
	e := newPullEnv(t)
	ctx := t.Context()
	src := e.add("gmail", "me@example.com")
	a := src.AccountID
	const box = "INBOX"
	m := e.mailbox(a, box, store.KeyGmail, 7, 100)
	if m.High != 100 || m.Low != 0 || m.KeyKind != store.KeyGmail {
		t.Fatalf("started %+v", m)
	}
	// Seen again with another key kind: the first one stays (§10.1).
	if again := e.mailbox(a, box, store.KeyLocation, 8, 5); again.KeyKind != store.KeyGmail || again.UIDValidity != 7 || again.High != 100 {
		t.Errorf("restarted %+v", again)
	}

	moved := func(ok bool, err error) bool {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !moved(e.db.AdvanceHigh(ctx, a, box, 7, 150, 2)) {
		t.Error("high did not move")
	}
	if moved(e.db.AdvanceHigh(ctx, a, box, 7, 120, 5)) || moved(e.db.AdvanceHigh(ctx, a, box, 7, 150, 5)) {
		t.Error("high moved back")
	}
	if moved(e.db.AdvanceHigh(ctx, a, box, 6, 200, 5)) {
		t.Error("high moved under another UIDVALIDITY")
	}
	if !moved(e.db.AdvanceLow(ctx, a, box, 7, 90, 1)) || moved(e.db.AdvanceLow(ctx, a, box, 7, 95, 1)) {
		t.Error("low")
	}
	if !moved(e.db.AdvanceLow(ctx, a, box, 7, 1, 0)) {
		t.Error("low to the bottom")
	}
	if !moved(e.db.RecordMailboxStatus(ctx, a, box, 7, 151, 140, e.at())) || moved(e.db.RecordMailboxStatus(ctx, a, box, 6, 151, 140, e.at())) {
		t.Error("status")
	}
	boxes, err := e.db.SourceMailboxes(ctx, a)
	if err != nil || len(boxes) != 1 {
		t.Fatal(boxes, err)
	}
	if b := boxes[0]; b.High != 150 || b.Low != 1 || b.DraftsSkipped != 3 || b.UIDNextSeen != 151 || b.MessagesSeen != 140 || b.LastRoundAt == nil {
		t.Errorf("after %+v", b)
	}

	// The crash table at store level. Committed, then the cursor: a
	// crash in between leaves the delivery, which the next round's
	// filter finds before fetching any body, and the cursor below it.
	loc := store.RemoteLocation{Mailbox: box, UIDValidity: 7, UID: 151}
	p := e.pull(a, store.GmailSourceKey(151), loc, []byte("151"))
	if _, err := e.db.CommitPull(ctx, p); err != nil {
		t.Fatal(err)
	}
	known, err := e.db.KnownSourceKeys(ctx, a, []string{p.SourceKey, store.GmailSourceKey(152)})
	if err != nil || len(known) != 1 || known[p.SourceKey].RemoteLocation != loc || known[p.SourceKey].ID != p.DeliveryID {
		t.Errorf("known %+v %v", known, err)
	}
	if b, _ := e.db.SourceMailboxes(ctx, a); b[0].High != 150 {
		t.Error("the commit moved the cursor")
	}
	// A failure is recorded before the cursor passes it.
	f := store.PullFailure{AccountID: a, RemoteLocation: store.RemoteLocation{Mailbox: box, UIDValidity: 7, UID: 152}, Size: -1, Detail: "NO", LastAt: e.at()}
	if n, err := e.db.RecordPullFailure(ctx, f); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if !moved(e.db.AdvanceHigh(ctx, a, box, 7, 152, 0)) {
		t.Error("high past the commit and the failure")
	}

	// UIDVALIDITY changed: catch-up starts over, backfill has not
	// started, and only a location-keyed folder has a rescan pending.
	if moved(e.db.ResetMailbox(ctx, a, box, 6, 9, 10)) {
		t.Error("reset from a UIDVALIDITY it is not at")
	}
	if !moved(e.db.ResetMailbox(ctx, a, box, 7, 9, 10)) {
		t.Fatal("reset")
	}
	if moved(e.db.AdvanceHigh(ctx, a, box, 7, 300, 0)) {
		t.Error("a worker of the old UIDVALIDITY moved the new cursor")
	}
	if _, err := e.db.CommitPull(ctx, e.pull(a, store.GmailSourceKey(153), store.RemoteLocation{Mailbox: box, UIDValidity: 7, UID: 153}, []byte("x"))); !errors.Is(err, store.ErrStaleMailbox) {
		t.Errorf("commit under the old UIDVALIDITY: %v", err)
	}
	b, _ := e.db.SourceMailboxes(ctx, a)
	if b[0].UIDValidity != 9 || b[0].High != 10 || b[0].Low != 0 || b[0].RescanUIDValidity != 0 || b[0].UIDNextSeen != 0 {
		t.Errorf("reset gmail folder %+v", b[0])
	}
	e.mailbox(a, "Archive", store.KeyLocation, 3, 40)
	if !moved(e.db.ResetMailbox(ctx, a, "Archive", 3, 4, 12)) {
		t.Fatal("reset archive")
	}
	if b, _ = e.db.SourceMailboxes(ctx, a); b[0].Name != "Archive" || b[0].RescanUIDValidity != 3 {
		t.Errorf("reset location folder %+v", b[0])
	}
	if moved(e.db.FinishRescan(ctx, a, "Archive", 2)) || !moved(e.db.FinishRescan(ctx, a, "Archive", 3)) {
		t.Error("finish rescan")
	}
}

// The UIDVALIDITY remap (§10.1.3) rewrites a delivery's location, and a
// location key with it, in place and only from the values it read.
func TestRelocate(t *testing.T) {
	t.Parallel()
	e := newPullEnv(t)
	ctx := t.Context()
	a := e.add("gmail", "me@example.com").AccountID
	e.mailbox(a, "All", store.KeyGmail, 7, 0)
	e.mailbox(a, "Archive", store.KeyLocation, 3, 0)

	gm := e.pull(a, store.GmailSourceKey(1), store.RemoteLocation{Mailbox: "All", UIDValidity: 7, UID: 5}, []byte("gm"))
	if _, err := e.db.CommitPull(ctx, gm); err != nil {
		t.Fatal(err)
	}
	from := store.PulledDelivery{ID: gm.DeliveryID, SourceKey: gm.SourceKey, RemoteLocation: gm.RemoteLocation}
	to := from
	to.UIDValidity, to.UID = 8, 50
	changedKey := to
	changedKey.SourceKey = store.GmailSourceKey(2)
	if _, err := e.db.Relocate(ctx, a, from, changedKey); err == nil {
		t.Error("a stable key rewritten")
	}
	if ok, err := e.db.Relocate(ctx, a, from, to); err != nil || !ok {
		t.Fatalf("relocate gm: %v %v", ok, err)
	}
	if ok, err := e.db.Relocate(ctx, a, from, to); err != nil || ok {
		t.Errorf("relocated twice from the same read: %v %v", ok, err)
	}
	if k, _ := e.db.KnownSourceKeys(ctx, a, []string{gm.SourceKey}); k[gm.SourceKey].RemoteLocation != to.RemoteLocation {
		t.Errorf("after relocate %+v", k)
	}

	// A location-keyed folder: matched by the raw's sha256 under the old
	// UIDVALIDITY, and its key follows its location.
	raws := [][]byte{[]byte("first"), []byte("second")}
	var old []store.Pull
	for i, raw := range raws {
		loc := store.RemoteLocation{Mailbox: "Archive", UIDValidity: 3, UID: uint32(i + 1)}
		p := e.pull(a, store.LocationSourceKey(3, uint32(i+1), "Archive"), loc, raw)
		if _, err := e.db.CommitPull(ctx, p); err != nil {
			t.Fatal(err)
		}
		old = append(old, p)
	}
	if _, err := e.db.ResetMailbox(ctx, a, "Archive", 3, 4, 0); err != nil {
		t.Fatal(err)
	}
	matches, err := e.db.RescanMatches(ctx, a, "Archive", 3, sha256.Sum256(raws[1]))
	if err != nil || len(matches) != 1 || matches[0].ID != old[1].DeliveryID {
		t.Fatalf("matches %+v %v", matches, err)
	}
	newLoc := store.RemoteLocation{Mailbox: "Archive", UIDValidity: 4, UID: 1}
	bad := store.PulledDelivery{SourceKey: store.LocationSourceKey(4, 2, "Archive"), RemoteLocation: newLoc}
	if _, err := e.db.Relocate(ctx, a, matches[0], bad); !errors.Is(err, store.ErrSourceKey) {
		t.Errorf("a key of another location: %v", err)
	}
	next := store.PulledDelivery{SourceKey: store.LocationSourceKey(4, 1, "Archive"), RemoteLocation: newLoc}
	if ok, err := e.db.Relocate(ctx, a, matches[0], next); err != nil || !ok {
		t.Fatalf("relocate loc: %v %v", ok, err)
	}
	// The other one cannot take the same new location.
	m0, _ := e.db.RescanMatches(ctx, a, "Archive", 3, sha256.Sum256(raws[0]))
	if _, err := e.db.Relocate(ctx, a, m0[0], next); !errors.Is(err, store.ErrDuplicate) {
		t.Errorf("two deliveries at one location: %v", err)
	}
	k, _ := e.db.KnownSourceKeys(ctx, a, []string{next.SourceKey, old[1].SourceKey})
	if len(k) != 1 || k[next.SourceKey].ID != old[1].DeliveryID {
		t.Errorf("after the rescan %+v", k)
	}
	// Nothing else changed: the raw and the delivery are the same rows.
	if n := count(t, e.pool, `SELECT count(*) FROM deliveries WHERE id = $1 AND raw_id = $2`, old[1].DeliveryID, old[1].RawID); n != 1 {
		t.Error("relocate touched more than the location")
	}
}

// Failures (§10.1.3) count attempts, reopen when they fail again, and
// keep the server's words as bounded, storable data.
func TestPullFailures(t *testing.T) {
	t.Parallel()
	e := newPullEnv(t)
	ctx := t.Context()
	a := e.add("gmail", "me@example.com").AccountID
	e.mailbox(a, "INBOX", store.KeyGmail, 7, 0)
	loc := store.RemoteLocation{Mailbox: "INBOX", UIDValidity: 7, UID: 9}
	detail := "NO \x00bad\xff " + strings.Repeat("é", 600)
	for want := 1; want <= 3; want++ {
		n, err := e.db.RecordPullFailure(ctx, store.PullFailure{AccountID: a, RemoteLocation: loc, Size: 123, Detail: detail, LastAt: e.at()})
		if err != nil || n != want {
			t.Fatalf("attempt %d: %d %v", want, n, err)
		}
	}
	if _, err := e.db.RecordPullFailure(ctx, store.PullFailure{AccountID: a, RemoteLocation: store.RemoteLocation{Mailbox: "Nowhere", UIDValidity: 1, UID: 1}, LastAt: e.at()}); err == nil {
		t.Error("a failure in a folder never started")
	}
	open, err := e.db.PullFailures(ctx, a, false)
	if err != nil || len(open) != 1 || open[0].Attempts != 3 || open[0].Size != 123 || open[0].RemoteLocation != loc {
		t.Fatalf("open %+v %v", open, err)
	}
	if d := open[0].Detail; len(d) > 1024 || strings.ContainsRune(d, 0) || !strings.HasPrefix(d, "NO ") {
		t.Errorf("detail %q", d)
	}
	if err := e.db.ResolvePullFailure(ctx, a, loc, e.at()); err != nil {
		t.Fatal(err)
	}
	if open, _ = e.db.PullFailures(ctx, a, false); len(open) != 0 {
		t.Errorf("resolved still open %+v", open)
	}
	if all, _ := e.db.PullFailures(ctx, a, true); len(all) != 1 || all[0].ResolvedAt == nil {
		t.Errorf("all %+v", all)
	}
	if n, err := e.db.RecordPullFailure(ctx, store.PullFailure{AccountID: a, RemoteLocation: loc, Size: -1, Detail: "again", LastAt: e.at()}); err != nil || n != 4 {
		t.Errorf("reopened %d %v", n, err)
	}
	if open, _ = e.db.PullFailures(ctx, a, false); len(open) != 1 || open[0].Size != 123 || open[0].Detail != "again" {
		t.Errorf("reopened %+v", open)
	}
}

// The requests `account sync` leaves for serve, the daily budget, and
// the per-account trusted authserv-id ingest and reparse read.
func TestPullRequestsAndBudget(t *testing.T) {
	t.Parallel()
	e := newPullEnv(t)
	ctx := t.Context()
	src := e.add("gmail", "me@example.com")
	cf := storetest.Account(t, e.db)
	a := src.AccountID

	if r, err := e.db.TakeSyncRequest(ctx, a); err != nil || r.Sync || r.RetryFailed {
		t.Errorf("nothing asked: %+v %v", r, err)
	}
	if err := e.db.RequestSync(ctx, "gmail", true, e.at()); err != nil {
		t.Fatal(err)
	}
	if err := e.db.RequestSync(ctx, "gmail", false, e.at()); err != nil {
		t.Fatal(err)
	}
	if r, err := e.db.TakeSyncRequest(ctx, a); err != nil || !r.Sync || !r.RetryFailed {
		t.Errorf("asked: %+v %v", r, err)
	}
	if r, err := e.db.TakeSyncRequest(ctx, a); err != nil || r.Sync {
		t.Errorf("taken twice: %+v %v", r, err)
	}
	if err := e.db.RequestSync(ctx, "cf", false, e.at()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a Cloudflare account: %v", err)
	}
	if s := e.source("gmail"); s.Revision != src.Revision {
		t.Error("a request moved the revision")
	}

	day := time.Date(2026, 11, 1, 23, 0, 0, 0, time.UTC)
	for _, step := range []struct {
		at   time.Time
		n    int64
		want int64
	}{{day, 100, 100}, {day.Add(30 * time.Minute), 50, 150}, {day.Add(2 * time.Hour), 7, 7}} {
		if used, err := e.db.AddBudget(ctx, a, step.at, step.n); err != nil || used != step.want {
			t.Errorf("budget at %v: %d %v, want %d", step.at, used, err, step.want)
		}
	}

	ids, err := e.db.TrustedAuthservIDs(ctx)
	if err != nil || ids[a] != "mx.example.com" || len(ids) != 1 {
		t.Errorf("authserv-ids %v %v", ids, err)
	}
	if _, ok := ids[cf]; ok {
		t.Error("the Cloudflare account has a pull authserv-id")
	}
}
