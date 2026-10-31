// Package devseed fills a development database with the synthetic corpus
// of testdata/eml, so the WebUI has realistic content to show. It is not
// part of yuuka: nothing in cmd/ imports it, and it writes straight into
// the content tables the way ingest would. The raw messages go into a
// local raw directory only if one is given; the structured view reads
// them from there.
//
//	go run ./internal/devseed/cmd -n 2000
//
// Tests use Load to get the same data.
package devseed

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/rand/v2"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/contenthash"
	"github.com/yuuka-mail/yuuka/internal/localraw"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Zone is the domain every seeded delivery goes to.
const Zone = "example.com"

// Claimed are the prefixes Load marks as handed out on purpose; the
// others stay discovered, so their mail is downgraded (§4.3).
var Claimed = []string{"alice", "github", "me", "svc-shop", "bank", "svc-ci"}

// Expected are the domains some claimed prefixes were given to, so the
// leak check (§3 principle 3) has something to judge: the generated
// mail lands on prefixes at random, and some of it from other senders.
var Expected = map[string][]string{"github": {"example.net"}, "svc-shop": {"example.cn"}}

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
	// RawDir, if set, receives every raw message at its local path, as
	// YUUKA_DATA_DIR does in yuuka serve.
	RawDir string
	// Proposals has agent:arisu propose a rule and a label on corpus
	// mail, pending, so the WebUI's proposal views have cards. Tests that
	// make their own proposals leave it off.
	Proposals bool
}

var (
	subjectLine   = regexp.MustCompile(`(?m)^Subject:[^\r\n]*(\r?\n[ \t][^\r\n]*)*`)
	messageIDLine = regexp.MustCompile(`(?mi)^Message-ID:[^\r\n]*`)
	replyLines    = regexp.MustCompile(`(?mi)^(In-Reply-To|References):`)
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
	// An account of its own, so seeded mail stays apart from what a dev
	// serve ingests under YUUKA_ACCOUNT.
	account, err := db.EnsureCloudflareAccount(ctx, "devseed")
	if err != nil {
		return n, err
	}
	// Claim first, so the newest mail is judged against the final state
	// (the downgrade is computed live anyway).
	for _, p := range Claimed {
		expected := Expected[p]
		if expected == nil {
			expected = []string{}
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO prefixes (zone, prefix, first_seen, last_seen, state, expected_domains) VALUES ($1, $2, $3, $3, $4, $5)
			ON CONFLICT (zone, prefix) DO UPDATE SET state = $4, expected_domains = $5`, Zone, p, o.Now, store.PrefixClaimed, expected); err != nil {
			return n, fmt.Errorf("claim %s: %w", p, err)
		}
	}
	for i := range o.Variants {
		src := corpus[rng.IntN(len(corpus))]
		subject := fmt.Sprintf(variantSubjects[rng.IntN(len(variantSubjects))], rng.IntN(1000000))
		raw := subjectLine.ReplaceAll(src, []byte("Subject: "+mime.BEncoding.Encode("utf-8", subject)))
		raw = messageIDLine.ReplaceAll(raw, fmt.Appendf(nil, "Message-ID: <seed-%d-%d@example.net>", o.Seed, i))
		// Variants of a reply would all join the conversation below.
		raw = replyLines.ReplaceAll(raw, []byte("X-Seed-Was-$1:"))
		at := o.Now.Add(-time.Duration(rng.Int64N(int64(90 * 24 * time.Hour))))
		if _, err := deliver(ctx, db, account, o.RawDir, raw, variantPrefixes[rng.IntN(len(variantPrefixes))], at); err != nil {
			return n, err
		}
		n++
	}
	// A conversation around the corpus reply, so threads have more than
	// one message: the question it answers, and an answer to it.
	if reply, err := os.ReadFile(filepath.Join(o.Dir, "alternative-reply.eml")); err == nil {
		turns := []struct {
			r  *strings.Replacer
			at time.Duration
		}{
			{strings.NewReplacer("From: Bob Example <Bob@Example.org>", "From: Alice <alice@example.com>",
				"To: Alice <alice@example.com>", "To: Bob Example <bob@example.org>",
				"Subject: Re: Lunch on Friday?", "Subject: Lunch on Friday?",
				"<reply-2@example.org>", "<orig-1@example.com>",
				"Friday works. Noon at the usual place?", "Are you free for lunch on Friday?"), -3 * time.Hour},
			{strings.NewReplacer("From: Bob Example <Bob@Example.org>", "From: Alice <alice@example.com>",
				"To: Alice <alice@example.com>", "To: Bob Example <bob@example.org>",
				"Message-ID: <reply-2@example.org>", "Message-ID: <reply-3@example.com>",
				"In-Reply-To: <orig-1@example.com>", "In-Reply-To: <reply-2@example.org>",
				"Friday works. Noon at the usual place?", "Noon it is. I'll book a table for two."), 0},
		}
		for _, t := range turns {
			if _, err := deliver(ctx, db, account, o.RawDir, []byte(t.r.Replace(string(reply))), "alice", o.Now.Add(t.at)); err != nil {
				return n, err
			}
			n++
		}
	}
	// The corpus itself is the newest mail, a minute apart.
	ids := map[string]string{}
	for i, raw := range corpus {
		prefix := "me"
		if m := toLine.FindSubmatch(raw); m != nil {
			prefix = strings.ToLower(string(m[1]))
		}
		id, err := deliver(ctx, db, account, o.RawDir, raw, prefix, o.Now.Add(-time.Duration(len(corpus)-i)*time.Minute))
		if err != nil {
			return n, fmt.Errorf("%s: %w", files[i], err)
		}
		ids[filepath.Base(files[i])] = id
		n++
	}
	if o.Proposals {
		if err := propose(ctx, db, ids, o.Now); err != nil {
			return n, err
		}
	}
	return n, nil
}

// proposals are what agent:arisu proposes, on the corpus file named: one
// of each kind (§8.5), so both cards and what they open have one.
var proposals = []struct {
	file string
	p    store.NewProposal
}{
	{"html-only-newsletter.eml", store.NewProposal{
		Kind: store.ProposalRule,
		Payload: []byte(`{"v":1,"match":{"all":[{"field":"list.id","op":"eq","value":"weekly.news.example.net"},` +
			`{"field":"from.verified","op":"eq","value":true}]},"actions":[{"kind":"label","label":"newsletters"},{"kind":"archive"}]}`),
		Rationale: "Example Weekly arrives every week and is never answered; label it and keep it out of the inbox.",
	}},
	{"plain-quoted-list.eml", store.NewProposal{
		Kind:      store.ProposalAnnotate,
		Payload:   []byte(`{"v":1,"scope":"thread","actions":[{"kind":"label","label":"travel"}]}`),
		Rationale: "This thread plans a trip.",
	}},
}

// propose creates agent:arisu and its pending proposals, through the
// store call the API's propose uses. Only a person decides them.
func propose(ctx context.Context, db *store.DB, ids map[string]string, now time.Time) error {
	p, err := db.EnsurePrincipal(ctx, store.Principal{ID: ulid.New(now), Kind: "agent", Name: "arisu", DisplayName: "Arisu", CreatedAt: now})
	if err != nil {
		return err
	}
	v := &auth.Viewer{PrincipalID: p.ID, PrincipalKind: p.Kind, PrincipalName: p.Name, Scopes: []auth.Scope{auth.MessageRead, auth.MessageAnnotate}}
	for _, x := range proposals {
		id := ids[x.file]
		if id == "" {
			continue
		}
		np := x.p
		np.MessageID = id
		np.Cites = []store.Cite{{URI: "yuuka://message/" + id}}
		a := store.Action{ID: ulid.New(now), Actor: p.ID, Tool: "proposal.create", At: now}
		if _, _, err := db.CreateProposal(ctx, v, np, a); err != nil && !errors.Is(err, store.ErrDuplicate) {
			return fmt.Errorf("propose on %s: %w", x.file, err)
		}
	}
	return nil
}

// deliver commits and folds raw as delivered to prefix and returns the
// message id, empty when the delivery was a duplicate.
func deliver(ctx context.Context, db *store.DB, account, rawDir string, raw []byte, prefix string, at time.Time) (string, error) {
	id := ulid.New(at)
	key := fmt.Sprintf("raw/%s/%s.eml", at.UTC().Format("2006/01"), id)
	if rawDir != "" {
		if err := (localraw.Dir{Root: rawDir}).Put(key, raw); err != nil {
			return "", err
		}
	}
	in := store.Ingest{
		RawID: id, R2Key: key, LocalPath: key, Size: int64(len(raw)), SHA256: sha256.Sum256(raw),
		ContentHash: contenthash.V1(raw), ReceivedAt: at, DeliveryID: id, AccountID: account,
		Zone: Zone, Prefix: prefix, EnvelopeFrom: "bounce@example.net", EnvelopeTo: prefix + "@" + Zone,
	}
	out, err := db.CommitIngest(ctx, in)
	if err != nil {
		return "", err
	}
	if out != store.Delivered {
		return "", nil
	}
	f, err := db.Fold(ctx, store.RawRef{
		DeliveryID: id, RawID: id, R2Key: key, LocalPath: key, ContentHash: in.ContentHash[:],
		Zone: Zone, Prefix: prefix, ReceivedAt: at,
	}, mimeparse.Parse(raw), authres.Extract(raw, "mx.cloudflare.net"))
	return f.MessageID, err
}
