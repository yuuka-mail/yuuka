package store_test

import (
	"crypto/sha256"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

func record(n int, contentHash [32]byte, to string) store.Ingest {
	at := time.Date(2026, 10, 31, 1, 0, n, 0, time.UTC)
	id := ulid.New(at)
	key := fmt.Sprintf("raw/2026/10/%s.eml", id)
	return store.Ingest{
		RawID: id, R2Key: key, LocalPath: key, Size: 10,
		SHA256:      sha256.Sum256(fmt.Appendf(nil, "raw-%d", n)),
		ContentHash: contentHash, ReceivedAt: at,
		DeliveryID: ulid.New(at), Account: "cf", Zone: "example.com", Prefix: "a",
		EnvelopeFrom: "s@example.net", EnvelopeTo: to,
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	t.Parallel()
	db, pool := storetest.New(t)
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("schema_migrations rows = %d, %v", n, err)
	}
}

// Two copies of one message to one address, committed concurrently, must
// yield exactly one delivery whichever transaction wins.
func TestConcurrentRedeliveries(t *testing.T) {
	t.Parallel()
	db, pool := storetest.New(t)
	ch := sha256.Sum256([]byte("content"))
	const n = 8
	var wg sync.WaitGroup
	outcomes := make([]store.Outcome, n)
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			outcomes[i], errs[i] = db.CommitIngest(t.Context(), record(i, ch, "a@example.com"))
		})
	}
	wg.Wait()
	delivered := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if outcomes[i] == store.Delivered {
			delivered++
		}
	}
	if delivered != 1 {
		t.Fatalf("delivered %d times, want 1 (%v)", delivered, outcomes)
	}
	var deliveries, dups int
	err := pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM deliveries),
		(SELECT count(*) FROM raw_objects r JOIN deliveries d ON d.raw_id = r.duplicate_of)`).Scan(&deliveries, &dups)
	if err != nil || deliveries != 1 || dups != n-1 {
		t.Fatalf("deliveries=%d duplicates=%d err=%v", deliveries, dups, err)
	}
}

func TestConcurrentSameRaw(t *testing.T) {
	t.Parallel()
	db, _ := storetest.New(t)
	rec := record(1, sha256.Sum256([]byte("c")), "a@example.com")
	var wg sync.WaitGroup
	outcomes := make([]store.Outcome, 4)
	for i := range outcomes {
		wg.Go(func() {
			var err error
			if outcomes[i], err = db.CommitIngest(t.Context(), rec); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	n := 0
	for _, o := range outcomes {
		if o == store.Delivered {
			n++
		} else if o != store.AlreadyIngested {
			t.Errorf("unexpected outcome %v", o)
		}
	}
	if n != 1 {
		t.Fatalf("outcomes %v", outcomes)
	}
}

func TestSameIDDifferentBytesFails(t *testing.T) {
	t.Parallel()
	db, _ := storetest.New(t)
	rec := record(1, sha256.Sum256([]byte("c")), "a@example.com")
	if _, err := db.CommitIngest(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	rec.SHA256 = sha256.Sum256([]byte("other bytes"))
	if _, err := db.CommitIngest(t.Context(), rec); err == nil {
		t.Fatal("same id with different bytes was accepted")
	}
}
