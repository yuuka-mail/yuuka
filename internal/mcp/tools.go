package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/classify"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// SearchInput is the input of search_messages.
type SearchInput struct {
	Q      string          `json:"q,omitempty" jsonschema:"words that must all occur in subject, sender or body (case-insensitive substrings); empty matches everything"`
	Prefix string          `json:"prefix,omitempty" jsonschema:"only mail delivered to this local part, or to local@domain"`
	Class  agentview.Class `json:"class,omitempty" jsonschema:"only mail of this class"`
	Since  string          `json:"since,omitempty" jsonschema:"only mail received at or after this time, RFC 3339 or YYYY-MM-DD"`
	Label  string          `json:"label,omitempty" jsonschema:"only mail carrying this label (exact, case-sensitive)"`
	Limit  int             `json:"limit,omitempty" jsonschema:"at most this many hits, default 20, at most 100"`
	// The §4.3 downgrade hides spam-like mail from default views; an
	// agent looking into a specific prefix may need it.
	IncludeDowngraded bool `json:"include_downgraded,omitempty" jsonschema:"also return downgraded mail (unclaimed prefix or dmarc=fail), which is excluded by default"`
}

// SearchOutput is the structured result of search_messages.
type SearchOutput struct {
	Total  int    `json:"total"`
	Notice string `json:"notice"` // applies to every hit's content
	Hits   []Hit  `json:"hits"`
}

// Hit is one message found. Content is what the sender wrote.
type Hit struct {
	URI         string          `json:"uri"`
	ReceivedAt  time.Time       `json:"received_at"`
	Class       agentview.Class `json:"class"`
	Downgraded  bool            `json:"downgraded,omitempty"`
	Quarantined bool            `json:"quarantined,omitempty"`
	// Labels and Archived are annotations (annotate), not mail content.
	Labels   []string   `json:"labels,omitempty"`
	Archived bool       `json:"archived,omitempty"`
	Content  HitContent `json:"content"`
}

// HitContent is the untrusted part of a Hit.
type HitContent struct {
	From    agentview.Address `json:"from"`
	Subject string            `json:"subject"`
	Snippet string            `json:"snippet"`
}

const (
	defaultHits = 20
	maxHits     = 100
)

func (s *Server) searchMessages(ctx context.Context, v *auth.Viewer, in SearchInput) (result, error) {
	q := store.SearchQuery{Text: in.Q, IncludeDowngraded: in.IncludeDowngraded, Limit: defaultHits, Label: in.Label}
	if in.Limit > 0 {
		q.Limit = min(in.Limit, maxHits)
	}
	if in.Prefix != "" {
		local, zone, _ := strings.Cut(strings.ToLower(strings.TrimSpace(in.Prefix)), "@")
		q.Prefix, q.Zone = local, zone
	}
	if in.Class != "" {
		// The input schema lists the classes; the SDK refuses others.
		c := map[agentview.Class]classify.Class{
			agentview.ClassMachine: classify.Machine, agentview.ClassHuman: classify.Human, agentview.ClassUnknown: classify.Unknown,
		}[in.Class]
		q.Class = &c
	}
	if in.Since != "" {
		t, err := parseSince(in.Since)
		if err != nil {
			return result{}, err
		}
		q.Since = t
	}
	res, err := s.db.Search(ctx, v, q)
	if err != nil {
		return result{}, err
	}
	out := SearchOutput{Total: res.Total, Notice: agentview.Notice, Hits: make([]Hit, len(res.Hits))}
	for i, h := range res.Hits {
		out.Hits[i] = Hit{
			URI: agentview.MessageURI(h.ID), ReceivedAt: h.ReceivedAt.UTC(), Class: agentview.Class(h.Class.String()),
			Downgraded: h.Downgraded, Quarantined: h.Quarantined, Labels: h.Labels, Archived: h.Archived,
			Content: HitContent{From: agentview.Address{Name: h.FromName, Addr: h.FromAddr}, Subject: h.Subject, Snippet: h.Snippet},
		}
	}
	return result{
		structured: out,
		text:       searchText(out, in.IncludeDowngraded),
		audit:      map[string]any{"total": res.Total, "returned": len(res.Hits)},
	}, nil
}

func parseSince(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return time.Time{}, refuse("since must be RFC 3339 or YYYY-MM-DD")
	}
	return t, nil
}

// searchText lists yuuka's facts per hit, then all sender-written fields
// in one fence, numbered alike.
func searchText(out SearchOutput, downgraded bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d of %d matching messages, newest first", len(out.Hits), out.Total)
	if !downgraded {
		b.WriteString(" (downgraded mail excluded; set include_downgraded to see it)")
	}
	b.WriteString("\n")
	if len(out.Hits) == 0 {
		return b.String()
	}
	var content strings.Builder
	var seed strings.Builder
	for i, h := range out.Hits {
		fmt.Fprintf(&b, "%d. %s · received %s · %s", i+1, h.URI, h.ReceivedAt.Format(time.RFC3339), h.Class)
		if h.Downgraded {
			b.WriteString(" · downgraded")
		}
		if h.Quarantined {
			b.WriteString(" · quarantined")
		}
		if h.Archived {
			b.WriteString(" · archived")
		}
		for _, l := range h.Labels {
			fmt.Fprintf(&b, " · label %q", l)
		}
		b.WriteString("\n")
		c := h.Content
		fmt.Fprintf(&content, "%d. From: %s\n   Subject: %s\n   Snippet: %s\n", i+1, address(c.From), c.Subject, c.Snippet)
		seed.WriteString(h.URI)
	}
	b.WriteString("\n")
	b.WriteString(agentview.Fence(seed.String(), content.String()))
	return b.String()
}

func address(a agentview.Address) string {
	if a.Name == "" {
		return a.Addr
	}
	return a.Name + " <" + a.Addr + ">"
}

// GetInput is the input of get_message.
type GetInput struct {
	URI   string          `json:"uri" jsonschema:"yuuka://message/<id>, optionally with #part/<section> or #part/<section>/para/<n>"`
	Level agentview.Level `json:"level,omitempty" jsonschema:"snippet (default), sections or part"`
}

func (s *Server) getMessage(ctx context.Context, v *auth.Viewer, in GetInput) (result, error) {
	id, anchor, err := agentview.ParseURI(in.URI)
	if err != nil {
		return result{}, refuse("%v", err)
	}
	level, err := agentview.ParseLevel(string(in.Level))
	if err != nil {
		return result{}, refuse("%v", err)
	}
	m, err := s.db.MessageByID(ctx, v, id)
	if errors.Is(err, store.ErrNotFound) {
		return result{}, refuse("no message %s", agentview.MessageURI(id))
	}
	if err != nil {
		return result{}, err
	}
	raw, err := s.raw.ReadRaw(ctx, m.Raw)
	if err != nil {
		return result{}, err
	}
	full := agentview.Build(agentview.InputOf(v, m, raw))
	view, err := agentview.Select(full, level, anchor)
	if errors.Is(err, agentview.ErrNoAnchor) {
		return result{}, refuse("%s has no %s", full.URI, anchor)
	}
	if err != nil {
		return result{}, err
	}
	notes, err := s.ownNotes(ctx, v, id)
	if err != nil {
		return result{}, err
	}
	audit := map[string]any{"uri": view.URI, "level": view.Level, "notes": len(notes)}
	if view.Content.More != "" {
		audit["more"] = view.Content.More
	}
	return result{structured: MessageOutput{View: view, Notes: notes}, text: view.Text() + notesText(view.URI, notes), audit: audit}, nil
}

// MessageOutput is the structured result of get_message: the view of
// §8.1 and, beside its facts and content, the caller's own notes
// (§8.10). Notes are neither yuuka's facts nor the sender's content.
type MessageOutput struct {
	agentview.View
	Notes []Note `json:"notes,omitempty"`
}

// PrefixesInput is the input of list_prefixes.
type PrefixesInput struct {
	LeakSuspect *bool `json:"leak_suspect,omitempty" jsonschema:"only prefixes where mail leaked or was spoofed (true), or neither (false)"`
	Limit       int   `json:"limit,omitempty" jsonschema:"at most this many prefixes, default 200, at most 1000"`
}

// PrefixesOutput is the structured result of list_prefixes.
type PrefixesOutput struct {
	Total    int      `json:"total"`
	Prefixes []Prefix `json:"prefixes"`
}

// Prefix is one entry of the registry. Labels, notes and expected domains
// are the operator's; the local part of an unclaimed (discovered) prefix
// was chosen by whoever sent mail to it.
type Prefix struct {
	Address         string   `json:"address"`
	Claimed         bool     `json:"claimed"`
	Label           string   `json:"label,omitempty"`
	Note            string   `json:"note,omitempty"`
	ExpectedDomains []string `json:"expected_domains,omitempty"`
	// FirstSeen and LastSeen are absent for a prefix claimed before any
	// mail reached it.
	FirstSeen *time.Time `json:"first_seen,omitempty"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
	Inactive  bool       `json:"inactive,omitempty"`
	Messages  int        `json:"messages"`
	// The leak check of a claimed prefix (§3 principle 3). Observed are
	// the sender domains the trusted receiver's DMARC proved. Leaks
	// counts mail from senders the address was not given to, Unverified
	// those of them that proved nothing; Spoofs counts mail claiming an
	// expected domain without proving it.
	ObservedDomains []ObservedDomain `json:"observed_domains,omitempty"`
	LeakSuspect     bool             `json:"leak_suspect,omitempty"`
	Leaks           int              `json:"leaks,omitempty"`
	Unverified      int              `json:"unverified,omitempty"`
	Spoofs          int              `json:"spoofs,omitempty"`
}

// ObservedDomain is an organizational domain DMARC proved to have sent
// mail to a prefix.
type ObservedDomain struct {
	Domain   string `json:"domain"`
	Messages int    `json:"messages"`
	Expected bool   `json:"expected"`
}

const (
	defaultPrefixes = 200
	maxPrefixes     = 1000
)

func (s *Server) listPrefixes(ctx context.Context, v *auth.Viewer, in PrefixesInput) (result, error) {
	limit := defaultPrefixes
	if in.Limit > 0 {
		limit = min(in.Limit, maxPrefixes)
	}
	list, err := s.db.PrefixStats(ctx, v, store.PrefixQuery{LeakSuspect: in.LeakSuspect, Limit: limit})
	if err != nil {
		return result{}, err
	}
	out := PrefixesOutput{Total: list.Total, Prefixes: make([]Prefix, len(list.Prefixes))}
	for i, p := range list.Prefixes {
		out.Prefixes[i] = Prefix{
			Address: p.Prefix + "@" + p.Zone, Claimed: p.State == store.PrefixClaimed, Label: p.Label, Note: p.Note,
			ExpectedDomains: p.ExpectedDomains, FirstSeen: utc(p.FirstSeen), LastSeen: utc(p.LastSeen),
			Inactive: !p.Active, Messages: p.Messages,
			LeakSuspect: p.Suspect(), Leaks: p.Leaks, Unverified: p.Unverified, Spoofs: p.Spoofs,
		}
		for _, o := range p.Observed {
			out.Prefixes[i].ObservedDomains = append(out.Prefixes[i].ObservedDomains,
				ObservedDomain{Domain: o.Domain, Messages: o.Messages, Expected: o.Expected})
		}
	}
	return result{
		structured: out,
		text:       prefixesText(out),
		audit:      map[string]any{"total": list.Total, "returned": len(list.Prefixes)},
	}, nil
}

// prefixesText lists claimed prefixes as facts and fences discovered
// ones: their names come from senders, and a catch-all accepts any.
func prefixesText(out PrefixesOutput) string {
	var claimed, discovered strings.Builder
	for _, p := range out.Prefixes {
		line := fmt.Sprintf("%s · %d messages · no mail yet", p.Address, p.Messages)
		if p.LastSeen != nil {
			line = fmt.Sprintf("%s · %d messages · last seen %s", p.Address, p.Messages, p.LastSeen.Format(time.RFC3339))
		}
		if p.Inactive {
			line += " · inactive"
		}
		if !p.Claimed {
			discovered.WriteString(line + "\n")
			continue
		}
		if p.Label != "" {
			line += fmt.Sprintf(" · label %q", p.Label)
		}
		if len(p.ExpectedDomains) > 0 {
			line += " · expects " + strings.Join(p.ExpectedDomains, ", ")
		}
		if len(p.ObservedDomains) > 0 {
			var seen []string
			for _, o := range p.ObservedDomains {
				seen = append(seen, fmt.Sprintf("%s (%d)", o.Domain, o.Messages))
			}
			line += " · DMARC-verified senders " + strings.Join(seen, ", ")
		}
		if p.Leaks > 0 {
			line += fmt.Sprintf(" · leak suspect: %d from senders it was not given to, %d of them unverified", p.Leaks, p.Unverified)
		}
		if p.Spoofs > 0 {
			line += fmt.Sprintf(" · spoofed: %d claim an expected sender without passing DMARC", p.Spoofs)
		}
		if p.Note != "" {
			line += fmt.Sprintf(" · note %q", p.Note)
		}
		claimed.WriteString(line + "\n")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d of %d prefixes, most recently seen first\n", len(out.Prefixes), out.Total)
	if claimed.Len() > 0 {
		b.WriteString("\nclaimed by the operator:\n" + claimed.String())
	}
	if discovered.Len() > 0 {
		b.WriteString("\ndiscovered (unclaimed; the local parts were chosen by senders):\n")
		b.WriteString(agentview.Fence("prefixes", discovered.String()))
	}
	return b.String()
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
