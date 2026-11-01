package ingest

import (
	"crypto/sha256"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/contenthash"
	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// addPullAccount makes the pull account "mailbox" for box@example.org,
// trusting authserv, with its INBOX recorded, and returns its id.
func (e *env) addPullAccount(t *testing.T, authserv string) string {
	t.Helper()
	ctx := t.Context()
	db := e.in.cfg.DB
	at := e.now
	op := store.Principal{ID: ulid.New(at), Kind: "human", Name: "operator", DisplayName: "operator", CreatedAt: at}
	if err := db.CreatePrincipal(ctx, op, store.Action{ID: ulid.New(at), Tool: "principal.create", At: at}); err != nil {
		t.Fatal(err)
	}
	c := store.SourceConfig{
		Address: "box@example.org", Host: "imap.example.org", Port: 993, Auth: store.SourceAuthPlain, Username: "box@example.org",
		TrustedAuthservID: authserv, MaxMessageBytes: 1 << 20, PollSeconds: 60,
	}
	aud, err := secret.IMAPS(c.Username, c.Host, c.Port)
	if err != nil {
		t.Fatal(err)
	}
	// The store never opens the secret; a stand-in will do.
	sealed := secret.Record{
		ID: ulid.New(at), Kind: store.SecretIMAPPassword, Audience: aud, Wrapper: "local",
		KeyRef: "0011223344556677", WrappedDEK: []byte("wrapped"), Payload: []byte("pw"),
	}
	src, err := db.AddPullAccount(ctx, "mailbox", c, sealed, op.ID, store.Action{ID: ulid.New(at), Actor: op.ID, Tool: "account.add", At: at})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.StartMailbox(ctx, src.AccountID, store.MailboxStart{Name: "INBOX", KeyKind: store.KeyLocation, UIDValidity: 7}); err != nil {
		t.Fatal(err)
	}
	return src.AccountID
}

// pull commits raw as UID uid of the pull account's INBOX, header only
// when headerOnly, and returns the reference the engine folds.
func (e *env) pull(t *testing.T, account string, uid uint32, raw []byte, headerOnly bool) store.RawRef {
	t.Helper()
	id := ulid.New(e.now.Add(time.Duration(uid) * time.Second))
	key := fmt.Sprintf("pull/2026/10/%s.eml", id)
	if err := e.local.Put(key, raw); err != nil {
		t.Fatal(err)
	}
	p := store.Pull{
		RawID: id, R2Key: key, LocalPath: key, Size: int64(len(raw)), SHA256: sha256.Sum256(raw),
		InternalDate: e.now, DeliveryID: ulid.New(e.now), AccountID: account,
		SourceKey:      store.LocationSourceKey(7, uid, "INBOX"),
		RemoteLocation: store.RemoteLocation{Mailbox: "INBOX", UIDValidity: 7, UID: uid},
	}
	var ch []byte
	if headerOnly {
		p.Omitted = store.OmittedTooLarge
	} else {
		h := contenthash.V1(raw)
		p.ContentHash, ch = &h, h[:]
	}
	if out, err := e.in.cfg.DB.CommitPull(t.Context(), p); err != nil || out != store.Delivered {
		t.Fatalf("commit pull: %v %v", out, err)
	}
	return store.RawRef{
		DeliveryID: p.DeliveryID, RawID: id, R2Key: key, LocalPath: key, ContentHash: ch,
		Zone: "example.org", Prefix: "box", ReceivedAt: e.now, AccountID: account,
	}
}

// signed is mail whose topmost Authentication-Results is authserv's.
func signed(authserv, subject string) []byte {
	return fmt.Appendf(nil, "Authentication-Results: %s; dmarc=pass header.from=example.net\r\n"+
		"From: Alice <alice@example.net>\r\nTo: box@example.org\r\nSubject: %s\r\n\r\nHi.\r\n", authserv, subject)
}

// Each delivery's auth facts are read with its own account's receiver
// (§10.1.1), on ingest and on reparse: a pulled mailbox trusts its own
// authserv-id, never the host's, and the host's account never the pulled
// one's. Pulled mail is queued for scoring only when the engine says so,
// and never by the backlog pass.
func TestPulledAuthserv(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var mu sync.Mutex
	var kicked []string
	e.in.cfg.NewMessage = func(id string) {
		mu.Lock()
		defer mu.Unlock()
		kicked = append(kicked, id)
	}
	kicks := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(kicked)
	}
	ctx := t.Context()
	account := e.addPullAccount(t, "mx.example.org")

	trusted := func(rawID string) (bool, string) {
		t.Helper()
		var ok bool
		var dmarc string
		if err := e.pool.QueryRow(ctx, `SELECT coalesce((auth->>'trusted')::bool, false), coalesce(auth->>'dmarc', '')
			FROM deliveries WHERE raw_id = $1`, rawID).Scan(&ok, &dmarc); err != nil {
			t.Fatal(err)
		}
		return ok, dmarc
	}

	own := e.pull(t, account, 1, signed("mx.example.org", "own receiver"), false)
	if _, err := e.in.FoldPulled(ctx, own, signed("mx.example.org", "own receiver"), false); err != nil {
		t.Fatal(err)
	}
	if ok, dmarc := trusted(own.RawID); !ok || dmarc != "pass" {
		t.Errorf("pulled mail with its receiver's results: trusted %v dmarc %q", ok, dmarc)
	}
	host := e.pull(t, account, 2, signed("mx.cloudflare.net", "host receiver"), false)
	if _, err := e.in.FoldPulled(ctx, host, signed("mx.cloudflare.net", "host receiver"), true); err != nil {
		t.Fatal(err)
	}
	if ok, _ := trusted(host.RawID); ok {
		t.Error("pulled mail trusted the host's authserv-id")
	}
	if n := kicks(); n != 1 {
		t.Errorf("%d messages queued for scoring, want the one the engine asked for", n)
	}
	cf := e.ingest(t, 0, "alice@example.com", signed("mx.example.org", "forged for cloudflare"))
	if ok, _ := trusted(cf.ID); ok {
		t.Error("Cloudflare mail trusted a pull account's authserv-id")
	}
	if n := kicks(); n != 2 {
		t.Errorf("Cloudflare mail not queued for scoring: %d", n)
	}

	// Header only: no content hash, its own message.
	header := []byte("Authentication-Results: mx.example.org; dmarc=fail header.from=example.net\r\n" +
		"From: x@example.net\r\nSubject: too large\r\n\r\n")
	big := e.pull(t, account, 3, header, true)
	if f, err := e.in.FoldPulled(ctx, big, header, false); err != nil || !f.NewMessage {
		t.Fatalf("fold header-only: %+v %v", f, err)
	}
	if ok, dmarc := trusted(big.RawID); !ok || dmarc != "fail" {
		t.Errorf("header-only: trusted %v dmarc %q", ok, dmarc)
	}
	// A multipart header without its body is not a broken message, and
	// says enough to show an attachment.
	mixed := []byte("From: x@example.net\r\nSubject: photos\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n")
	photos := e.pull(t, account, 5, mixed, true)
	f, err := e.in.FoldPulled(ctx, photos, mixed, false)
	if err != nil {
		t.Fatal(err)
	}
	var parseErr *string
	var attach bool
	if err := e.pool.QueryRow(ctx, `SELECT parse_error, has_attach FROM messages WHERE id = $1`, f.MessageID).
		Scan(&parseErr, &attach); err != nil {
		t.Fatal(err)
	}
	if parseErr != nil || !attach {
		t.Errorf("header-only multipart: parse_error %v, has_attach %v", parseErr, attach)
	}

	// The backlog pass folds a pulled delivery with its account's value
	// too, and does not queue it.
	left := e.pull(t, account, 4, signed("mx.example.org", "left for the backlog"), false)
	if st, err := e.in.ProcessBacklog(ctx); err != nil || st.Processed != 1 {
		t.Fatalf("backlog: %+v %v", st, err)
	}
	if ok, _ := trusted(left.RawID); !ok {
		t.Error("backlog folded pulled mail with the host's authserv-id")
	}
	if n := kicks(); n != 2 {
		t.Errorf("the backlog pass queued pulled mail for scoring: %d", n)
	}

	// Reparse recomputes each from its own account.
	if _, err := e.pool.Exec(ctx, `UPDATE deliveries SET auth = NULL`); err != nil {
		t.Fatal(err)
	}
	if st, err := e.in.Reparse(ctx, time.Time{}); err != nil || st.Failed != 0 {
		t.Fatalf("reparse: %+v %v", st, err)
	}
	for raw, want := range map[string]bool{own.RawID: true, host.RawID: false, cf.ID: false, big.RawID: true, left.RawID: true} {
		if ok, _ := trusted(raw); ok != want {
			t.Errorf("after reparse, raw %s trusted %v, want %v", raw, ok, want)
		}
	}
}
