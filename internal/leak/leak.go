// Package leak judges mail to a claimed prefix against the domains the
// address was handed out to (§3 principle 3).
//
// Two questions are kept apart. Whether the address leaked needs no
// authentication: any sender outside the expected domains knows it. Who
// that sender is can only be told by the trusted receiver's DMARC result,
// the one check that binds the From header the reader sees (§10.1.1). So
// a leak is attributed to a domain only when DMARC passed for it; mail
// that proves nothing still counts as a leak, attributed to nobody; and
// mail whose From claims an expected domain without proving it is a
// spoof.
//
// Domains are compared by organizational domain (the registrable domain
// of the Public Suffix List, as DMARC's relaxed alignment does): mail
// from notifications.example.net matches an expectation of example.net,
// and expectations are stored that way (Expected).
//
// Everything here is a pure function of facts the store already holds
// (deliveries.auth, messages.from_addr, prefixes.expected_domains), so
// the store judges at read time and nothing is written: editing the
// expected domains, or a reparse that changes the auth facts, takes
// effect on the next read.
package leak

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/net/publicsuffix"

	"github.com/yuuka-mail/yuuka/internal/authres"
)

// Verdict is the judgement on one delivery.
type Verdict string

const (
	// None: not judged. The prefix is not claimed, no domains are
	// expected yet, or the trusted receiver reported dmarc=fail for a
	// From that claims no expected domain.
	None Verdict = ""
	// OK: DMARC passed for an expected domain.
	OK Verdict = "ok"
	// Leak: the sender is not one the address was given to. Domain is
	// the DMARC-verified sender, or empty when the mail proved nothing.
	Leak Verdict = "leak"
	// Spoof: the From header claims an expected domain, and DMARC did
	// not pass for it. Domain is the claimed one.
	Spoof Verdict = "spoof"
)

// Judgement is a Verdict and the organizational domain it is about.
type Judgement struct {
	Verdict Verdict
	// Domain is the verified sender for OK and an attributed Leak, the
	// claimed one for Spoof, and empty for a Leak by mail that proved
	// nothing about its sender.
	Domain string
}

// Sender is what one delivery says about who sent it, as organizational
// domains.
type Sender struct {
	// Verified is the From domain the trusted receiver's DMARC check
	// passed; empty when nothing was proven.
	Verified string
	// Claimed is the From domain as the sender wrote it.
	Claimed string
	// Failed is set when the trusted receiver reported dmarc=fail.
	Failed bool
}

// SenderOf is the Sender of a delivery with auth facts a (nil for a
// delivery from before they were extracted) of a message whose From
// address is fromAddr.
func SenderOf(a *authres.Auth, fromAddr string) Sender {
	var dmarc, headerFrom string
	if a != nil && a.Trusted {
		dmarc = a.DMARC
		for _, r := range a.Results {
			if r.Method == "dmarc" {
				headerFrom = r.Props["header.from"]
				break
			}
		}
	}
	return NewSender(dmarc, headerFrom, DomainOf(fromAddr))
}

// NewSender is the Sender of a delivery for which the trusted receiver
// reported the DMARC result dmarc ("" without a trusted result) about
// headerFrom, its header.from property ("" if it gave none), of a
// message whose From domain is fromDomain.
//
// DMARC proves the From domain only if it judged the From the reader
// sees. When the receiver names a header.from of another organization
// (several From fields, or a parser that read another one), the pass is
// about something else and proves nothing here.
func NewSender(dmarc, headerFrom, fromDomain string) Sender {
	s := Sender{Claimed: OrgDomain(fromDomain), Failed: dmarc == "fail"}
	if dmarc != "pass" || s.Claimed == "" {
		return s
	}
	if headerFrom == "" || OrgDomain(headerFrom) == s.Claimed {
		s.Verified = s.Claimed
	}
	return s
}

// Judge decides a delivery from s to a claimed prefix that expects mail
// from expected, which Expected need not have normalized. Without
// expected domains there is nothing to judge against.
//
// A DMARC failure is the receiver's own verdict that the From is forged,
// and such mail is downgraded (§4.3). It is judged only when it forges an
// expected sender: that is the typical spoof. Forging anyone else says
// nothing about who knows the address beyond what spam to any address
// says, and counting it as a leak would bury the real ones under spam
// campaigns; it stays in the downgraded view.
func Judge(expected []string, s Sender) Judgement {
	if len(expected) == 0 {
		return Judgement{}
	}
	if s.Failed && !IsExpected(expected, s.Claimed) {
		return Judgement{}
	}
	switch {
	case IsExpected(expected, s.Verified):
		return Judgement{Verdict: OK, Domain: s.Verified}
	case s.Verified != "":
		return Judgement{Verdict: Leak, Domain: s.Verified}
	case IsExpected(expected, s.Claimed):
		return Judgement{Verdict: Spoof, Domain: s.Claimed}
	}
	return Judgement{Verdict: Leak}
}

// IsExpected reports whether the organizational domain d is one of
// expected.
func IsExpected(expected []string, d string) bool {
	return d != "" && slices.ContainsFunc(expected, func(e string) bool { return OrgDomain(e) == d })
}

// DomainOf is the domain of an address, lowercased; "" without one.
func DomainOf(addr string) string {
	i := strings.LastIndexByte(addr, '@')
	if i < 0 {
		return ""
	}
	return strings.ToLower(addr[i+1:])
}

// OrgDomain is the organizational domain of name: its registrable domain
// by the Public Suffix List, lowercased. It is "" for a name that is not
// a plausible domain or is a public suffix itself (com, github.io), which
// no one sender owns.
func OrgDomain(name string) string {
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	if !plausible(name) {
		return ""
	}
	d, err := publicsuffix.EffectiveTLDPlusOne(name)
	if err != nil {
		return ""
	}
	return d
}

// plausible admits the characters of a domain name, Unicode ones
// included, and nothing else: the names end up in text for people and
// models (§8.3), so a value with spaces or punctuation is not one.
func plausible(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for label := range strings.SplitSeq(name, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if r != '-' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
				return false
			}
		}
	}
	return true
}

// Expected normalizes the expected domains of a prefix for storing:
// each one's organizational domain, without duplicates, sorted. What is
// stored is what is compared, so an operator who writes
// mail.example.net reads back example.net.
func Expected(domains []string) ([]string, error) {
	out := []string{}
	for _, d := range domains {
		d = strings.TrimSpace(d)
		if strings.Contains(d, "@") {
			return nil, fmt.Errorf("%q: want a domain, not an address", d)
		}
		org := OrgDomain(d)
		if org == "" {
			return nil, fmt.Errorf("%q: not a domain one sender can own (a public suffix, or not a domain name)", d)
		}
		if !slices.Contains(out, org) {
			out = append(out, org)
		}
	}
	slices.Sort(out)
	return out, nil
}
