// Package devseed fills a development database with the synthetic corpus
// of testdata/eml, so the WebUI has realistic content to show. It is not
// part of yuuka: nothing in cmd/ imports it, and it writes straight into
// the content tables the way ingest would, without any raw store.
//
//	go run ./internal/devseed/cmd -n 2000
//
// Tests use Load to get the same data.
package devseed

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand/v2"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/contenthash"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Zone is the domain every seeded delivery goes to.
const Zone = "example.com"

// Claimed are the prefixes Load marks as handed out on purpose; the
// others stay discovered, so their mail is downgraded (§4.3).
var Claimed = []string{"alice", "github", "me", "svc-shop", "bank", "svc-ci"}

// variantSubjects are the subjects of generated mail; %d is a counter.
var variantSubjects = []string{
	"Your verification code is %06d",
	"[ci] build #%d passed on main",
	"[ci] build #%d failed on main",
	"Order #%d has shipped",
	"Statement for account ending %04d is ready",
	"Re: weekly sync notes (%d)",
	"New sign-in to your account (%d)",
	"Invoice %d from Example Hosting",
	"【示例】您的验证码是 %06d",
	"Digest: %d new posts in Example Forum",
	"Payment of %d.00 received",
	"Reminder: meeting moved (%d)",
}

// variantPrefixes receive the generated mail; the random-looking ones
// stay discovered.
var variantPrefixes = []string{"github", "svc-shop", "bank", "svc-ci", "alice", "me", "k3x9q", "newsletter-7", "github", "svc-ci"}

// Options configures Load.
type Options struct {
	// Dir holds the .eml corpus.
	Dir string
	// Variants is how many extra messages to generate from the corpus by
	// rewriting Subject and Message-ID.
	Variants int
	// Now is when the newest mail arrives; generated mail goes back
	// about three months from it.
	Now time.Time
	// Seed makes the generated spread reproducible.
	Seed uint64
}

var (
	subjectLine   = regexp.MustCompile(`(?m)^Subject:[^\r\n]*(\r?\n[ \t][^\r\n]*)*`)
	messageIDLine = regexp.MustCompile(`(?mi)^Message-ID:[^\r\n]*`)
	toLine        = regexp.MustCompile(`(?mi)^To:[^\r\n]*?([A-Za-z0-9._+-]+)@`)
)

// Load ingests every corpus file once, to the local part of its To
// header, then o.Variants generated messages, and claims the Claimed
// prefixes. It returns the number of deliveries written.
func Load(ctx context.Context, db *store.DB, pool *pgxpool.Pool, o Options) (int, error) {
	files, err := filepath.Glob(filepath.Join(o.Dir, "*.eml"))
	if err != nil || len(files) == 0 {
		return 0, fmt.Errorf("no .eml files in %s", o.Dir)
	}
	corpus := make([][]byte, len(files))
	for i, f := range files {
		if corpus[i], err = os.ReadFile(f); err != nil {
			return 0, err
		}
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	rng := rand.New(rand.NewPCG(o.Seed, o.Seed^0x79757563))
	n := 0
	// Claim first, so the newest mail is judged against the final state
	// (the downgrade is computed live anyway).
	for _, p := range Claimed {
		if _, err := pool.Exec(ctx, `
			INSERT INTO prefixes (zone, prefix, first_seen, last_seen, state) VALUES ($1, $2, $3, $3, $4)
			ON CONFLICT (zone, prefix) DO UPDATE SET state = $4`, Zone, p, o.Now, store.PrefixClaimed); err != nil {
			return n, fmt.Errorf("claim %s: %w", p, err)
		}
	}
	for i := range o.Variants {
		src := corpus[rng.IntN(len(corpus))]
		subject := fmt.Sprintf(variantSubjects[rng.IntN(len(variantSubjects))], rng.IntN(1000000))
		raw := subjectLine.ReplaceAll(src, []byte("Subject: "+mime.BEncoding.Encode("utf-8", subject)))
		raw = messageIDLine.ReplaceAll(raw, fmt.Appendf(nil, "Message-ID: <seed-%d-%d@example.net>", o.Seed, i))
		at := o.Now.Add(-time.Duration(rng.Int64N(int64(90 * 24 * time.Hour))))
		if err := deliver(ctx, db, raw, variantPrefixes[rng.IntN(len(variantPrefixes))], at); err != nil {
			return n, err
		}
		n++
	}
	// The corpus itself is the newest mail, a minute apart.
	for i, raw := range corpus {
		prefix := "me"
		if m := toLine.FindSubmatch(raw); m != nil {
			prefix = strings.ToLower(string(m[1]))
		}
		if err := deliver(ctx, db, raw, prefix, o.Now.Add(-time.Duration(len(corpus)-i)*time.Minute)); err != nil {
			return n, fmt.Errorf("%s: %w", files[i], err)
		}
		n++
	}
	return n, nil
}

func deliver(ctx context.Context, db *store.DB, raw []byte, prefix string, at time.Time) error {
	id := ulid.New(at)
	key := fmt.Sprintf("raw/%s/%s.eml", at.UTC().Format("2006/01"), id)
	in := store.Ingest{
		RawID: id, R2Key: key, LocalPath: key, Size: int64(len(raw)), SHA256: sha256.Sum256(raw),
		ContentHash: contenthash.V1(raw), ReceivedAt: at, DeliveryID: id, Account: "devseed",
		Zone: Zone, Prefix: prefix, EnvelopeFrom: "bounce@example.net", EnvelopeTo: prefix + "@" + Zone,
	}
	out, err := db.CommitIngest(ctx, in)
	if err != nil {
		return err
	}
	if out != store.Delivered {
		return nil
	}
	_, err = db.Fold(ctx, store.RawRef{
		DeliveryID: id, RawID: id, R2Key: key, LocalPath: key, ContentHash: in.ContentHash[:],
		Zone: Zone, Prefix: prefix, ReceivedAt: at,
	}, mimeparse.Parse(raw), authres.Extract(raw, "mx.cloudflare.net"))
	return err
}
