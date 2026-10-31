package rules

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/net/publicsuffix"

	"github.com/yuuka-mail/yuuka/internal/classify"
)

// Facts is what a rule sees of one (message, delivery): the unit of
// evaluation (§8.5). The store fills it from a snapshot taken before any
// rule runs, so rules never see each other's effects. Derived fields
// (from.domain, from.org, to.addr) and folding are computed here, not by
// the caller, so they have one definition.
type Facts struct {
	// FromAddr is the From header's address as parsed (messages.from_addr).
	// Self-asserted: only FromVerified makes it mean anything.
	FromAddr string
	// FromVerified is this delivery's trusted authentication result:
	// a trusted receiver said dmarc=pass (§10.1.1).
	FromVerified bool

	// ToPrefix and ToZone are this delivery's envelope recipient.
	ToPrefix string
	ToZone   string
	// PrefixClaimed is prefixes.state = 'claimed', read at evaluation time.
	PrefixClaimed bool

	// Leak and Spoof are the verdicts of the same function the read path
	// uses; there is only one definition of either.
	Leak  bool
	Spoof bool

	Subject string
	Body    string // messages.body_text
	// ListID is the List-Id header as parsed, raw or already reduced with
	// ListID; the evaluator reduces it either way.
	ListID string

	// Class is the effective class when this round starts
	// (class_override, else the classifier's).
	Class classify.Class
	// Extractions are the kinds extracted from the message (ExtractOTP, …).
	Extractions []string
	Attachment  bool
}

// FoldAddr folds an address or address part for comparison: Go's
// strings.ToLower, no Unicode normalization. It mirrors store.FoldAddr
// (the folding ingest applies to prefixes); it is duplicated rather than
// imported because the store imports this package. A test keeps the two
// equal.
func FoldAddr(s string) string { return strings.ToLower(s) }

// domainOf is the part of a folded address after its last "@", or "".
func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return addr[i+1:]
	}
	return ""
}

// OrgDomain is the organizational domain of a folded domain: its public
// suffix plus one label, from the Public Suffix List compiled into
// golang.org/x/net. A domain that has none (empty, an address literal, a
// public suffix itself) is its own organization. No IDNA conversion is
// done; Unicode labels are compared as written. The list ships with the
// binary, so an upgrade can move a domain's organization; rules on
// from.org follow the list in use.
func OrgDomain(domain string) string {
	if domain == "" || domain[0] == '[' {
		return domain
	}
	org, err := publicsuffix.EffectiveTLDPlusOne(domain)
	if err != nil {
		return domain
	}
	return org
}

// ListID reduces a List-Id header value to its identifier: the text
// between the last "<" and the ">" after it (RFC 2919), or the whole
// value trimmed when there are no brackets. It is idempotent, so the
// store may keep either form.
func ListID(v string) string {
	if i := strings.LastIndexByte(v, '<'); i >= 0 {
		if j := strings.IndexByte(v[i+1:], '>'); j >= 0 {
			return strings.TrimSpace(v[i+1 : i+1+j])
		}
	}
	return strings.TrimSpace(v)
}

// window is the part of s a regex sees: the longest prefix of at most
// RegexWindow bytes that does not split a UTF-8 sequence. Frozen in v1 for
// every text field; it only ever cuts the body in practice.
func window(s string) string {
	if len(s) <= RegexWindow {
		return s
	}
	cut := RegexWindow
	for i := 0; i < utf8.UTFMax && cut > 0 && !utf8.RuneStart(s[cut]); i++ {
		cut--
	}
	return s[:cut]
}

func className(c classify.Class) string {
	switch c {
	case classify.Machine:
		return ClassMachine
	case classify.Human:
		return ClassHuman
	}
	return ClassUnknown
}

// view is Facts prepared for evaluation: derived and folded once per
// message, lowercased lazily, shared by every rule in a round.
type view struct {
	f     *Facts
	text  map[string]string
	lower map[string]string
}

func newView(f *Facts) *view {
	from := FoldAddr(f.FromAddr)
	domain := domainOf(from)
	prefix, zone := FoldAddr(f.ToPrefix), FoldAddr(f.ToZone)
	return &view{
		f: f,
		text: map[string]string{
			FieldFromAddr:   from,
			FieldFromDomain: domain,
			FieldFromOrg:    OrgDomain(domain),
			FieldToAddr:     prefix + "@" + zone,
			FieldToPrefix:   prefix,
			FieldToZone:     zone,
			FieldSubject:    f.Subject,
			FieldBody:       f.Body,
			FieldListID:     ListID(f.ListID),
		},
		lower: map[string]string{},
	}
}

// lowered is the text field case-folded for eq, in and contains. The body
// can be large, so it is folded at most once per round.
func (v *view) lowered(field string) string {
	if s, ok := v.lower[field]; ok {
		return s
	}
	s := strings.ToLower(v.text[field])
	v.lower[field] = s
	return s
}

func (v *view) bool(field string) bool {
	switch field {
	case FieldFromVerified:
		return v.f.FromVerified
	case FieldPrefixClaimed:
		return v.f.PrefixClaimed
	case FieldAttachment:
		return v.f.Attachment
	}
	return false
}

func (v *view) enum(field string) string {
	if field == FieldClass {
		return className(v.f.Class)
	}
	return ""
}

func (v *view) has(field, value string) bool {
	switch field {
	case FieldVerdict:
		return value == VerdictLeak && v.f.Leak || value == VerdictSpoof && v.f.Spoof
	case FieldExtraction:
		for _, k := range v.f.Extractions {
			if k == value {
				return true
			}
		}
	}
	return false
}
