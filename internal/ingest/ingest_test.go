package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/contenthash"
	"github.com/yuuka-mail/yuuka/internal/contract"
	"github.com/yuuka-mail/yuuka/internal/localraw"
	"github.com/yuuka-mail/yuuka/internal/notify"
	"github.com/yuuka-mail/yuuka/internal/r2/r2fake"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

type recordingNotifier struct {
	mu    sync.Mutex
	lines []string
}

func (n *recordingNotifier) NewMail(_ context.Context, m notify.Mail) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	// The dispatcher decides who may be told by the delivery (§13.8).
	if !ulid.Valid(m.DeliveryID) || !ulid.Valid(m.MessageID) {
		panic(fmt.Sprintf("new mail without its delivery or message: %+v", m))
	}
	n.lines = append(n.lines, m.Text)
	return nil
}

func (n *recordingNotifier) got() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.lines...)
}

type env struct {
	in      *Ingester
	raw     *r2fake.Bucket
	pending *r2fake.Bucket
	pool    *pgxpool.Pool
	local   localraw.Dir
	notes   *recordingNotifier
	now     time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, pool := storetest.New(t)
	e := &env{
		raw:     r2fake.New(),
		pending: r2fake.New(),
		pool:    pool,
		local:   localraw.Dir{Root: t.TempDir()},
		notes:   &recordingNotifier{},
		now:     time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC),
	}
	e.in = New(Config{
		Raw: e.raw, Pending: e.pending, Local: e.local, DB: db,
		Account: "cf-test", TrustedAuthservID: "mx.cloudflare.net", Notifier: e.notes,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return e.now },
	})
	return e
}

// deliver plays the Worker: marker into pending/, raw with metadata into raw/.
func (e *env) deliver(t *testing.T, at time.Time, from, to string, body []byte) contract.Marker {
	t.Helper()
	m := e.marker(at, from, to, body)
	e.pending.Put(contract.PendingKey(m.ID), contract.Marshal(m), nil)
	e.raw.Put(m.RawKey, body, map[string]string{contract.MetadataKey: contract.EncodeMetadata(contract.Marshal(m))})
	return m
}

func (e *env) marker(at time.Time, from, to string, body []byte) contract.Marker {
	at = at.UTC().Truncate(time.Millisecond)
	id := ulid.New(at)
	return contract.Marker{
		ID: id, ReceivedAt: at, EnvelopeFrom: from, EnvelopeTo: to,
		RawKey: contract.RawKey(at, id), RawSize: int64(len(body)),
	}
}

// claim marks prefixes of example.com as claimed, so their mail escapes
// the §4.3 downgrade and is notified.
func (e *env) claim(t *testing.T, prefixes ...string) {
	t.Helper()
	for _, p := range prefixes {
		_, err := e.pool.Exec(t.Context(), `
			INSERT INTO prefixes (zone, prefix, first_seen, last_seen, state) VALUES ('example.com', $1, $2, $2, 1)
			ON CONFLICT (zone, prefix) DO UPDATE SET state = 1`, p, e.now)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func (e *env) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// message is a minimal mail as Cloudflare hands it over: its own Received
// line on top, which differs per recipient and per delivery attempt.
func message(rcpt, txid string) []byte {
	return fmt.Appendf(nil, "Received: from mail.example.net (mail.example.net [192.0.2.10])\r\n"+
		"\tby cloudflare-email.net (cloudflare) id %s for <%s>;\r\n"+
		"\tFri, 31 Oct 2026 01:41:38 +0000\r\n"+
		"From: Alice <alice@example.net>\r\nTo: %s\r\nSubject: hello\r\n"+
		"Message-ID: <m1@example.net>\r\n\r\nHi.\r\n", txid, rcpt, rcpt)
}

func corpus(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "contenthash", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestIngestEndToEnd(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.claim(t, "github")
	body := message("GitHub@Example.COM", "tx1")
	m := e.deliver(t, e.now, "alice@example.net", "GitHub@Example.COM", body)

	out, err := e.in.Ingest(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	e.in.Wait()
	if out != store.Delivered {
		t.Fatalf("outcome %v", out)
	}

	sum, ch := sha256.Sum256(body), contenthash.V1(body)
	var (
		size                                       int64
		gotSum, gotCH                              []byte
		zone, prefix, envFrom, envTo, srcKey, acct string
	)
	err = e.pool.QueryRow(t.Context(), `
		SELECT r.size, r.sha256, r.content_hash, d.zone, d.prefix, d.envelope_from, d.envelope_to, d.source_key, a.name
		FROM raw_objects r JOIN deliveries d ON d.raw_id = r.id JOIN accounts a ON a.id = d.account_id
		WHERE r.id = $1`, m.ID).
		Scan(&size, &gotSum, &gotCH, &zone, &prefix, &envFrom, &envTo, &srcKey, &acct)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(body)) || !bytes.Equal(gotSum, sum[:]) || !bytes.Equal(gotCH, ch[:]) {
		t.Errorf("raw row: size %d sha %x content %x", size, gotSum, gotCH)
	}
	if zone != "example.com" || prefix != "github" || envTo != "GitHub@Example.COM" || envFrom != "alice@example.net" || acct != "cf-test" {
		t.Errorf("delivery row: %s %s %s %s %s", zone, prefix, envFrom, envTo, acct)
	}
	if srcKey != store.PushSourceKey(ch, "GitHub@Example.COM") {
		t.Errorf("source_key %s", srcKey)
	}

	path, _ := e.local.Path(m.RawKey)
	if local, err := os.ReadFile(path); err != nil || !bytes.Equal(local, body) {
		t.Errorf("local copy: %v", err)
	}
	if e.pending.Has(contract.PendingKey(m.ID)) {
		t.Error("pending marker not deleted after commit")
	}
	if got, want := e.notes.got(), []string{notify.Summary("example.com", "github", body)}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("notifications %q, want %q", got, want)
	}
}

// The measured case of §2.1.1/§4.1: the sender retries, Cloudflare adds a
// new Received/ARC set, so the raw differs but the content hash does not.
func TestRedeliveryIsDeduped(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.claim(t, "github")
	first := e.deliver(t, e.now, "alice@example.net", "github@example.com", corpus(t, "cloudflare-a.eml"))
	second := e.deliver(t, e.now.Add(10*time.Minute), "alice@example.net", "github@example.com", corpus(t, "cloudflare-a-redelivered.eml"))

	if out, err := e.in.Ingest(t.Context(), first); err != nil || out != store.Delivered {
		t.Fatalf("first: %v %v", out, err)
	}
	if out, err := e.in.Ingest(t.Context(), second); err != nil || out != store.Redelivery {
		t.Fatalf("second: %v %v", out, err)
	}
	e.in.Wait()

	if n := e.count(t, `SELECT count(*) FROM deliveries`); n != 1 {
		t.Fatalf("%d deliveries", n)
	}
	if n := e.count(t, `SELECT count(*) FROM raw_objects WHERE id = $1 AND duplicate_of = $2`, second.ID, first.ID); n != 1 {
		t.Fatal("second raw not marked duplicate_of the first")
	}
	if n := e.count(t, `SELECT count(*) FROM raw_objects`); n != 2 {
		t.Fatalf("%d raw rows; the redelivered raw must still be recorded", n)
	}
	if got := e.notes.got(); len(got) != 1 {
		t.Fatalf("notified %d times, want once", len(got))
	}
	if e.pending.Has(contract.PendingKey(second.ID)) {
		t.Error("marker of the duplicate not deleted")
	}
}

// One SMTP transaction to two catch-all addresses: the Worker runs once
// per RCPT TO, so there are two raws and two deliveries.
func TestTwoRecipientsTwoDeliveries(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.claim(t, "a", "b")
	a := e.deliver(t, e.now, "alice@example.net", "a@example.com", message("a@example.com", "tx1"))
	b := e.deliver(t, e.now, "alice@example.net", "b@example.com", message("b@example.com", "tx1"))
	for _, m := range []contract.Marker{a, b} {
		if out, err := e.in.Ingest(t.Context(), m); err != nil || out != store.Delivered {
			t.Fatalf("%s: %v %v", m.EnvelopeTo, out, err)
		}
	}
	e.in.Wait()
	if n := e.count(t, `SELECT count(DISTINCT prefix) FROM deliveries`); n != 2 {
		t.Fatalf("%d deliveries", n)
	}
	if got := e.notes.got(); len(got) != 2 {
		t.Fatalf("notified %d times", len(got))
	}
}

// Two Worker invocations can store byte-identical raws under different ids.
// Each still gets its own raw row; the delivery is decided by source_key:
// a different recipient is a new delivery, the same one is a redelivery.
func TestIdenticalBytesUnderDifferentIDs(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	body := message("a@example.com", "tx1")
	first := e.deliver(t, e.now, "alice@example.net", "a@example.com", body)
	other := e.deliver(t, e.now.Add(time.Millisecond), "alice@example.net", "b@example.com", body)
	again := e.deliver(t, e.now.Add(2*time.Millisecond), "alice@example.net", "a@example.com", body)

	want := []store.Outcome{store.Delivered, store.Delivered, store.Redelivery}
	for i, m := range []contract.Marker{first, other, again} {
		if out, err := e.in.Ingest(t.Context(), m); err != nil || out != want[i] {
			t.Fatalf("%s: got %v %v, want %v", m.ID, out, err, want[i])
		}
	}
	e.in.Wait()
	if n := e.count(t, `SELECT count(*) FROM raw_objects`); n != 3 {
		t.Fatalf("%d raw rows, want one per object", n)
	}
	if n := e.count(t, `SELECT count(*) FROM deliveries`); n != 2 {
		t.Fatalf("%d deliveries, want 2", n)
	}
}

func TestByteIdenticalReingestIsNoop(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.claim(t, "a")
	body := message("a@example.com", "tx1")
	m := e.deliver(t, e.now, "", "a@example.com", body)
	if _, err := e.in.Ingest(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	// The webhook and reconcile both seeing the marker.
	e.pending.Put(contract.PendingKey(m.ID), contract.Marshal(m), nil)
	out, err := e.in.Ingest(t.Context(), m)
	if err != nil || out != store.AlreadyIngested {
		t.Fatalf("re-ingest: %v %v", out, err)
	}
	e.in.Wait()
	if n := e.count(t, `SELECT (SELECT count(*) FROM raw_objects) + (SELECT count(*) FROM deliveries)`); n != 2 {
		t.Fatalf("%d rows, want 2", n)
	}
	if e.pending.Has(contract.PendingKey(m.ID)) {
		t.Error("marker not deleted on re-ingest")
	}
	if got := e.notes.got(); len(got) != 1 {
		t.Fatalf("notified %d times", len(got))
	}
}

func TestSizeMismatchCommitsNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	m := e.deliver(t, e.now, "s@example.net", "a@example.com", message("a@example.com", "tx1"))
	m.RawSize++
	if _, err := e.in.Ingest(t.Context(), m); err == nil {
		t.Fatal("size mismatch accepted")
	}
	if n := e.count(t, `SELECT count(*) FROM raw_objects`); n != 0 {
		t.Fatal("rows written")
	}
	if !e.pending.Has(contract.PendingKey(m.ID)) {
		t.Fatal("marker deleted although nothing was committed")
	}
}

func TestReconcile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ready := e.deliver(t, e.now.Add(-time.Hour), "s@example.net", "a@example.com", message("a@example.com", "tx1"))

	// Markers whose raw never arrived, younger and older than the grace.
	young := e.marker(e.now.Add(-OrphanGrace+time.Minute), "s@example.net", "b@example.com", []byte("x"))
	old := e.marker(e.now.Add(-OrphanGrace), "s@example.net", "c@example.com", []byte("x"))
	for _, m := range []contract.Marker{young, old} {
		e.pending.Put(contract.PendingKey(m.ID), contract.Marshal(m), nil)
	}
	// A marker that does not parse is kept for a human.
	bad := contract.PendingKey(ulid.New(e.now))
	e.pending.Put(bad, []byte(`{"v":2}`), nil)

	st, err := e.in.Reconcile(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := ReconcileStats{Markers: 4, Ingested: 1, Waiting: 1, Orphaned: 1, Failed: 1}
	if st != want {
		t.Fatalf("stats %+v, want %+v", st, want)
	}
	for key, keep := range map[string]bool{
		contract.PendingKey(ready.ID): false,
		contract.PendingKey(young.ID): true,
		contract.PendingKey(old.ID):   false,
		bad:                           true,
	} {
		if e.pending.Has(key) != keep {
			t.Errorf("%s: kept=%v, want %v", key, !keep, keep)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM deliveries`); n != 1 {
		t.Fatalf("%d deliveries", n)
	}

	// Once the young marker's raw shows up, the next pass ingests it.
	e.raw.Put(young.RawKey, []byte("x"), nil)
	st, err = e.in.Reconcile(t.Context())
	if err != nil || st.Ingested != 1 || e.pending.Has(contract.PendingKey(young.ID)) {
		t.Fatalf("second pass: %+v %v", st, err)
	}
}

// A raw object without a row (its marker lost) is recovered from the
// yuuka-marker metadata alone.
func TestFullScanRecoversFromMetadata(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	done := e.deliver(t, e.now, "s@example.net", "a@example.com", message("a@example.com", "tx1"))
	if _, err := e.in.Ingest(t.Context(), done); err != nil {
		t.Fatal(err)
	}
	lost := e.deliver(t, e.now, "", "Bounce+X@example.com", message("Bounce+X@example.com", "tx2"))
	if err := e.pending.Delete(t.Context(), contract.PendingKey(lost.ID)); err != nil {
		t.Fatal(err)
	}
	noMeta := e.marker(e.now, "s@example.net", "c@example.com", []byte("y"))
	e.raw.Put(noMeta.RawKey, []byte("y"), nil)

	st, err := e.in.FullScan(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st != (ScanStats{RawObjects: 3, Missing: 2, Recovered: 1, Failed: 1}) {
		t.Fatalf("stats %+v", st)
	}
	var from, to, prefix string
	err = e.pool.QueryRow(t.Context(), `SELECT envelope_from, envelope_to, prefix FROM deliveries WHERE raw_id = $1`, lost.ID).Scan(&from, &to, &prefix)
	if err != nil || from != "" || to != "Bounce+X@example.com" || prefix != "bounce+x" {
		t.Fatalf("recovered delivery %q %q %q %v", from, to, prefix, err)
	}
}
