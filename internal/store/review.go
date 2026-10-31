package store

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/idna"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/extract"
	"github.com/yuuka-mail/yuuka/internal/leak"
	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// The facts of the review tier's checks (§8.9, sendpolicy/review.go),
// gathered inside WithDecision under the grant's lock, from what the
// submitter can see and nothing else: a check that read past the
// submitter's view would answer "is this the code of a message I cannot
// read" for them. Every lookup is by equality or by an indexed prefix;
// no outgoing value is ever written anywhere, and the verdict keeps
// where a value was found, not the value.

// ExtractionLookback is how far back the exfiltration check looks for
// codes and links that came in: 30 days. An expired code still names an
// account; a month covers the mail an agent is plausibly asked about.
const ExtractionLookback = 30 * 24 * time.Hour

// Bounds on what one review looks up. Mail beyond them escalates as
// unreadable: a review cannot vouch for what it did not check.
const (
	maxReviewRefs  = 50
	maxReviewHosts = 100
	// maxFindings bounds the values listed in a verdict; one is enough
	// to escalate.
	maxFindings = 20
)

// ReviewInput is the outgoing message as the checks read it, cut by
// internal/submit from the bytes that go out (or, for a draft, from a
// preview of them: the body does not depend on the decision).
type ReviewInput struct {
	// Tokens are the candidate values of the message (extract.Tokens),
	// each with the first place it occurs.
	Tokens []ReviewItem
	// Hosts are the hosts of its URLs, as written, with where.
	Hosts []ReviewItem
	// Refs are the messages it cites or carries.
	Refs []ReviewRef
	// Unreadable names the parts no check can read.
	Unreadable []string
}

// ReviewItem is a value and where in the message it is: subject, body,
// attachment 2 (notes.txt) ...
type ReviewItem struct {
	Value string
	Where string
}

// ReviewRef is one message referred to: by its id (a yuuka:// anchor or
// a draft's cite) or by its Message-ID (an attached message).
type ReviewRef struct {
	As        string // as written
	Where     string
	MessageID string // a message id, or
	HeaderID  string // a Message-ID without brackets
}

func (db *DB) reviewFacts(ctx context.Context, q dbtx, v *auth.Viewer, s Submission) (*sendpolicy.ReviewFacts, error) {
	c := s.Content
	f := &sendpolicy.ReviewFacts{Unreadable: slices.Clone(c.Unreadable)}
	var err error
	if len(c.Tokens) > extract.MaxTokens {
		f.Unreadable = append(f.Unreadable, fmt.Sprintf("more than %d candidate values", extract.MaxTokens))
	} else if f.Extracted, err = db.reviewExtracted(ctx, q, v, c.Tokens, s.Now); err != nil {
		return nil, err
	}
	if len(c.Refs) > maxReviewRefs {
		f.Unreadable = append(f.Unreadable, fmt.Sprintf("more than %d cited messages", maxReviewRefs))
	} else if f.Refs, err = db.reviewRefs(ctx, q, v, c.Refs); err != nil {
		return nil, err
	}
	if f.Links, err = db.reviewLinks(ctx, q, v, c.Hosts); err != nil {
		return nil, err
	}
	if len(f.Links) > maxReviewHosts {
		f.Unreadable = append(f.Unreadable, fmt.Sprintf("links to more than %d hosts", maxReviewHosts))
		f.Links = f.Links[:maxReviewHosts]
	}
	return f, nil
}

// reviewExtracted finds the otp and link values of mail that came in
// (a visible delivery: our own sent copies have none) in the lookback
// among tokens. One probe of the hash index per token.
func (db *DB) reviewExtracted(ctx context.Context, q dbtx, v *auth.Viewer, tokens []ReviewItem, now time.Time) ([]sendpolicy.Extracted, error) {
	if len(tokens) == 0 {
		return nil, nil
	}
	values, places := make([]string, len(tokens)), make([]string, len(tokens))
	for i, t := range tokens {
		values[i], places[i] = t.Value, t.Where
	}
	// Message ids are ULIDs of their arrival: the lower bound is the
	// smallest id of that millisecond.
	since := ulid.New(now.Add(-ExtractionLookback))[:10] + "0000000000000000"
	rows, err := q.Query(ctx, `
		SELECT DISTINCT ON (x.message_id, x.kind, t.place) x.kind, x.message_id, x.source, t.place
		FROM unnest($1::text[], $2::text[]) AS t(value, place)
		JOIN extractions x ON x.value = t.value
		WHERE x.kind IN ('otp', 'link') AND x.message_id >= $3
		  AND EXISTS (SELECT 1 FROM deliveries d WHERE d.message_id = x.message_id AND `+db.visibleDelivery(v, "d")+`)
		ORDER BY x.message_id, x.kind, t.place, x.source
		LIMIT $4`, values, places, since, maxFindings)
	if err != nil {
		return nil, fmt.Errorf("review: extracted values: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (sendpolicy.Extracted, error) {
		var e sendpolicy.Extracted
		err := r.Scan(&e.Kind, &e.MessageID, &e.Source, &e.Where)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("review: extracted values: %w", err)
	}
	return out, nil
}

// reviewRefs resolves each reference to a message the submitter can
// see, and that message's thread and correspondents (ReplyFacts).
func (db *DB) reviewRefs(ctx context.Context, q dbtx, v *auth.Viewer, refs []ReviewRef) ([]sendpolicy.Ref, error) {
	threads := map[string]ReplyFacts{}
	var out []sendpolicy.Ref
	for _, r := range refs {
		ref := sendpolicy.Ref{As: r.As, Where: r.Where}
		id := r.MessageID
		if id == "" && r.HeaderID != "" {
			err := q.QueryRow(ctx, `SELECT m.id FROM messages m WHERE m.message_id = $1 AND `+db.visibleMessage(v, "m")+`
				ORDER BY m.received_at DESC, m.id DESC LIMIT 1`, r.HeaderID).Scan(&id)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("review: message-id %s: %w", r.HeaderID, err)
			}
		}
		if id != "" {
			rf, ok := threads[id]
			if !ok {
				var err error
				rf, err = db.replyFacts(ctx, q, v, id)
				switch {
				case errors.Is(err, ErrNotFound):
				case err != nil:
					return nil, fmt.Errorf("review: %w", err)
				default:
					threads[id] = rf
				}
			}
			ref.MessageID, ref.ThreadID, ref.Correspondents = rf.MessageID, rf.ThreadID, rf.Correspondents
		}
		out = append(out, ref)
	}
	return out, nil
}

// reviewLinks says for each linked host whether its organizational
// domain is known: one of our identities' zones, the From of mail that
// came in authenticated (visible, not downgraded, DMARC pass), or an
// address we sent mail to. The grant's allowlist is Decide's.
func (db *DB) reviewLinks(ctx context.Context, q dbtx, v *auth.Viewer, hosts []ReviewItem) ([]sendpolicy.LinkDomain, error) {
	if len(hosts) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `SELECT DISTINCT zone FROM identities WHERE disabled_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("review: identity zones: %w", err)
	}
	zones, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("review: identity zones: %w", err)
	}
	own := map[string]bool{}
	for _, z := range zones {
		if d := leak.OrgDomain(asciiHost(z)); d != "" {
			own[d] = true
		}
	}
	seen := map[string]string{} // organizational domain -> why known, "" unknown
	byHost := map[string]bool{}
	var out []sendpolicy.LinkDomain
	for _, h := range hosts {
		host := asciiHost(h.Value)
		if byHost[host] {
			continue
		}
		byHost[host] = true
		l := sendpolicy.LinkDomain{Host: host, Where: h.Where}
		if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err != nil {
			l.Domain = leak.OrgDomain(host)
		}
		if l.Domain != "" {
			why, ok := seen[l.Domain]
			if !ok {
				if why, err = db.knownDomain(ctx, q, v, l.Domain, own); err != nil {
					return nil, err
				}
				seen[l.Domain] = why
			}
			l.Seen = why
		}
		out = append(out, l)
		if len(out) > maxReviewHosts {
			break
		}
	}
	return out, nil
}

func (db *DB) knownDomain(ctx context.Context, q dbtx, v *auth.Viewer, org string, own map[string]bool) (string, error) {
	if own[org] {
		return "identity", nil
	}
	// The From domain is stored as written (lowercased); an
	// internationalized one may be in either form.
	forms := []string{org}
	if u, err := idna.Lookup.ToUnicode(org); err == nil && u != org {
		forms = append(forms, u)
	}
	for _, d := range forms {
		// The domain itself, or a subdomain: on the reversed domain
		// (messages_from_domain_rev, 0011) an equality, or the range of
		// strings that start with the reversed ".domain". The range is
		// written with the pattern operators the index's opclass has,
		// as LIKE 'prefix%' would be planned, so a cached generic plan
		// uses the index too.
		dot := reverseRunes("." + d) // ends in "."; "/" is the next byte
		var inbound bool
		err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM messages m
			WHERE (reverse(split_part(m.from_addr, '@', 2)) = $1
			       OR reverse(split_part(m.from_addr, '@', 2)) ~>=~ $2 AND reverse(split_part(m.from_addr, '@', 2)) ~<~ $3)
			  AND EXISTS (SELECT 1 FROM deliveries d JOIN prefixes p ON p.zone = d.zone AND p.prefix = d.prefix
			              WHERE d.message_id = m.id AND `+db.visibleDelivery(v, "d")+` AND `+notDowngraded+`
			                AND d.auth->>'dmarc' = 'pass'))`,
			reverseRunes(d), dot, strings.TrimSuffix(dot, ".")+"/").Scan(&inbound)
		if err != nil {
			return "", fmt.Errorf("review: inbound domain: %w", err)
		}
		if inbound {
			return "inbound", nil
		}
		var sent bool
		err = q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM outbox_recipients r
			JOIN outbox o ON o.id = r.outbox_id JOIN identities i ON i.id = o.identity_id
			WHERE o.state IN `+contactedStates+` AND `+db.readableAccount(v, "i.account_id")+`
			  AND (split_part(r.addr_fold, '@', 2) = $1 OR split_part(r.addr_fold, '@', 2) LIKE $2))`,
			d, "%."+d).Scan(&sent)
		if err != nil {
			return "", fmt.Errorf("review: sent domain: %w", err)
		}
		if sent {
			return "sent", nil
		}
	}
	return "", nil
}

// asciiHost is a host as the link-domain check compares it: lowercased,
// without a final dot, internationalized names in ASCII. A name IDNA
// refuses stays as it is, and OrgDomain then decides whether it is one.
func asciiHost(h string) string {
	h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
	if a, err := idna.Lookup.ToASCII(h); err == nil {
		return a
	}
	return h
}

// reverseRunes is Postgres's reverse() for UTF-8 text.
func reverseRunes(s string) string {
	r := []rune(s)
	slices.Reverse(r)
	return string(r)
}
