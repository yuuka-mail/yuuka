package store

import (
	"errors"
	"fmt"
	"strings"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/classify"
	"github.com/yuuka-mail/yuuka/internal/search"
	"github.com/yuuka-mail/yuuka/internal/triage/score"
)

// The filter tree of §16 compiled to SQL, the one place where a search
// condition gets its meaning, and the leaf predicates the message list
// shares with it. Every predicate reads the messages aliased m, their
// message_state s, the latest visible outbox row ob and the lateral g
// with g.downgraded, as Search and the list join them; facts other than
// the message's own (deliveries, outbox rows) are read through the
// visibility conditions, so a condition, negated or not, can never tell
// the caller of a delivery or an outbox row it may not see (§16.4).
//
// No text of the query is written into the SQL: every value is a
// parameter. The tree is bounded by the parser (search.MaxConditions,
// search.MaxDepth).

// searchDoc must stay identical to the expression of the messages_search
// trigram index, or the index is not used.
const searchDoc = `(m.subject || ' ' || m.from_name || ' ' || m.from_addr || ' ' || m.body_text)`

type compiler struct {
	db     *DB
	v      *auth.Viewer
	triage *score.Profile
	args   []any
}

func (c *compiler) arg(a any) string {
	c.args = append(c.args, a)
	return fmt.Sprintf("$%d", len(c.args))
}

// filter is the condition f. A predicate may be NULL where SQL makes it
// so (a missing message_state row); NULL is read as false, which AND and
// OR already do, so only NOT has to say it.
func (c *compiler) filter(f *search.Filter) (string, error) {
	if f.Op == "" {
		return c.cond(f.Cond)
	}
	parts := make([]string, len(f.Args))
	for i, a := range f.Args {
		p, err := c.filter(a)
		if err != nil {
			return "", err
		}
		parts[i] = p
	}
	switch f.Op {
	case search.And:
		return "(" + strings.Join(parts, " AND ") + ")", nil
	case search.Or:
		return "(" + strings.Join(parts, " OR ") + ")", nil
	case search.Not:
		return "NOT coalesce(" + strings.Join(parts, " OR ") + ", false)", nil
	}
	return "", fmt.Errorf("filter operator %q", f.Op)
}

func (c *compiler) cond(k search.Condition) (string, error) {
	switch k.Field {
	case search.Text:
		return searchDoc + ` ILIKE ` + c.like(k.Term), nil
	// A field is a substring of searchDoc, so also asking searchDoc
	// changes nothing but lets the trigram index find the candidates.
	case search.From:
		p := c.like(k.Term)
		return `(` + searchDoc + ` ILIKE ` + p + ` AND (m.from_name ILIKE ` + p + ` OR m.from_addr ILIKE ` + p + `))`, nil
	case search.Subject:
		p := c.like(k.Term)
		return `(` + searchDoc + ` ILIKE ` + p + ` AND m.subject ILIKE ` + p + `)`, nil
	case search.Body:
		p := c.like(k.Term)
		return `(` + searchDoc + ` ILIKE ` + p + ` AND m.body_text ILIKE ` + p + `)`, nil
	// The folded addresses of 0007; their names join with §16.7 step 2.
	// A row not read yet (NULL) has none.
	case search.To:
		return `EXISTS (SELECT 1 FROM unnest(m.to_addrs) a WHERE a ILIKE ` + c.like(k.Term) + `)`, nil
	case search.Cc:
		return `EXISTS (SELECT 1 FROM unnest(m.cc_addrs) a WHERE a ILIKE ` + c.like(k.Term) + `)`, nil
	case search.Bcc:
		// Envelope recipients are delivery facts (§13.4). Refused rather
		// than empty: -bcc:x would otherwise be everything.
		if !c.v.Has(auth.DeliveryRead) {
			return "", &search.Error{At: k.At, Scope: auth.DeliveryRead, Msg: "bcc: needs the delivery:read scope"}
		}
		// The names stay on the draft the row was submitted from (0014);
		// mail submitted over SMTP has none.
		p := c.like(k.Term)
		return `EXISTS (SELECT 1 ` + c.db.visibleOutbox(c.v, "m") + ` AND (EXISTS (SELECT 1 FROM outbox_recipients br
			WHERE br.outbox_id = so.id AND br.kind = 'bcc' AND br.addr_fold ILIKE ` + p + `)
			OR EXISTS (SELECT 1 FROM drafts bd CROSS JOIN unnest(bd.bcc_names) bn WHERE bd.id = so.draft_id AND bn ILIKE ` + p + `)))`, nil
	case search.ListID:
		return `coalesce(m.list_id, '') ILIKE ` + c.like(k.Term), nil
	case search.After:
		return `m.received_at >= ` + c.arg(k.Time), nil
	case search.Before:
		return `m.received_at < ` + c.arg(k.Time), nil
	case search.HasAttachment:
		return `m.has_attach`, nil
	case search.DeliveredTo:
		return c.db.deliveredTo(c.v, k.Prefix, k.Zone, c.arg), nil
	case search.Label:
		return hasLabel(k.Label, c.arg), nil
	case search.Archived:
		return isArchived, nil
	case search.Downgraded:
		return `g.downgraded`, nil
	case search.Sent:
		// ob is the latest visible outbox row whose sent copy m is.
		return `ob.id IS NOT NULL`, nil
	case search.Class:
		return classIn([]classify.Class{k.Class}, c.arg), nil
	case search.Triage:
		if c.triage == nil {
			return "", &search.Error{At: k.At, Msg: "triage: no decision model is configured here, so no mail is scored"}
		}
		f := AnswerFilter{Profile: *c.triage, Question: k.Question, Option: k.Option}
		if err := f.valid(); err != nil {
			return "", err
		}
		return f.condition(c.arg), nil
	}
	return "", errors.New("filter condition " + string(k.Field))
}

// like is a parameter matching term anywhere, case-insensitively.
func (c *compiler) like(term string) string {
	return c.arg("%" + likeEscape(term) + "%")
}

// likeEscape makes a search term literal inside an ILIKE pattern, whose
// default escape character is the backslash.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// deliveredTo: m has a visible delivery to prefix (at zone; any zone when
// it is ""). Only visible deliveries count, so neither it nor its
// negation reveals one the viewer cannot see.
func (db *DB) deliveredTo(v *auth.Viewer, prefix, zone string, arg func(any) string) string {
	w := `EXISTS (SELECT 1 FROM deliveries d WHERE d.message_id = m.id AND d.prefix = ` + arg(prefix)
	if zone != "" {
		w += ` AND d.zone = ` + arg(zone)
	}
	return w + ` AND ` + db.visibleDelivery(v, "d") + `)`
}

// hasLabel: m carries label (exact, case-sensitive). labels <> '{}' is
// implied by the containment; it is spelled out so the partial GIN index
// applies.
func hasLabel(label string, arg func(any) string) string {
	return `(s.labels <> '{}' AND s.labels @> ARRAY[` + arg(label) + `]::text[])`
}

const isArchived = `coalesce(s.archived, false)`

// classIn: the effective class of m (a human override wins) is one of cs.
func classIn(cs []classify.Class, arg func(any) string) string {
	vs := make([]int16, len(cs))
	for i, c := range cs {
		vs[i] = int16(c)
	}
	return `coalesce(s.class_override, m.class) = ANY(` + arg(vs) + `)`
}
