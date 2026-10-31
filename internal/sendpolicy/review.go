package sendpolicy

import (
	"fmt"
	"slices"
	"strings"

	"github.com/yuuka-mail/yuuka/internal/leak"
)

// The review tier (§8.9 rule 10). Every check is deterministic and runs
// inside the decision, on facts the store gathered from what the
// submitter can see (ReviewFacts): the review is not an oracle that
// would answer "is this the code in a message I cannot read". Every
// check runs, none short-circuits, so a person reading the verdict sees
// every finding at once.
//
// Two kinds of check. A gate guards an outcome nobody can take back: a
// first message to a stranger, a code of incoming mail going out, and
// the readability that the code check depends on. A gate that does not
// pass escalates the submission to a draft; never to an outbox row,
// which would spend a slot of the limits and put a copy that went
// nowhere into the thread. An advisory check (another thread's mail, a
// link to a domain never seen) is context, not a guess at intent: its
// findings go into the verdict, the held card and the submitter's
// result, and the recall window is where they are acted on. The agent
// that wrote the mail is trusted to have meant it; what it may not have
// known is put in front of it and of the person.

// MaxRecallsPerDay is the circuit breaker of a grant (§8.9): when humans
// have withdrawn this many of its held messages submitted in the last 24
// hours, the grant is suspended and its other held messages withdrawn.
// Two, because one recall can be a typo noticed late; a second within a
// day says the grant's mail is not what its owner expects.
const MaxRecallsPerDay = 2

// Check names, as stored in the verdicts and shown by the UI.
const (
	CheckFirstContact = "first_contact"
	CheckExtracted    = "extracted_values"
	CheckCrossThread  = "cross_thread"
	CheckLinkDomains  = "link_domains"
	CheckReadable     = "readable"
)

// ReviewFacts are what the checks read, gathered by the store as the
// submitter sees the mailbox. First contact is read from
// Counters.FirstContact.
type ReviewFacts struct {
	// Unreadable names the parts of the outgoing message the checks
	// cannot read: an attachment that is not text, a message that does
	// not parse, more candidate values or links than a lookup vouches
	// for. Any of them escalates: what cannot be read cannot be passed.
	Unreadable []string
	// Extracted are the codes and sign-in links the extractor found in
	// mail that came in from outside, within the lookback, that appear
	// in the outgoing message.
	Extracted []Extracted
	// Refs are the messages the outgoing mail cites (a draft's cites, a
	// yuuka:// anchor in its text) or carries (an attached message).
	Refs []Ref
	// Links are the domains the outgoing message links to.
	Links []LinkDomain
}

// Extracted is one value of incoming mail found in the outgoing message.
// It says where, never what: the value is the secret.
type Extracted struct {
	Kind string `json:"kind"` // otp or link
	// MessageID is the message it was extracted from; Source where in it
	// (extractions.source).
	MessageID string `json:"message_id"`
	Source    string `json:"source"`
	// Where in the outgoing message: subject, body, attachment ...
	Where string `json:"where"`
}

// Ref is one message the outgoing mail refers to.
type Ref struct {
	// As is how it was referred to: a yuuka:// URI, or a Message-ID.
	As    string `json:"as"`
	Where string `json:"where"`
	// MessageID and ThreadID are empty when no message the submitter can
	// see answers to it.
	MessageID string `json:"message_id,omitempty"`
	ThreadID  string `json:"-"`
	// Correspondents of that thread, as Reply.Correspondents.
	Correspondents []string `json:"-"`
}

// LinkDomain is one domain linked to.
type LinkDomain struct {
	// Host as linked, lowercased, in ASCII (IDNA), without a final dot.
	Host  string `json:"host"`
	Where string `json:"where"`
	// Domain is its organizational domain, "" for an IP literal or a
	// name that is not one.
	Domain string `json:"domain,omitempty"`
	// Seen says why the domain is known, "" if it is not (the allowlist
	// is the grant's, and Decide adds it): one of our identities' zones
	// ("identity"), the From of mail that came in authenticated
	// ("inbound": visible, not downgraded, DMARC pass), an address we
	// sent to ("sent").
	Seen string `json:"seen,omitempty"`
}

// Verdict is the outcome of every check, in a fixed order.
type Verdict struct {
	Checks []Check `json:"checks"`
}

// Check is the outcome of one check. Findings say what tripped it; a
// check that passed has none. An advisory check that did not pass only
// reports; verdicts stored before a check became advisory lack the flag
// and read as the gate it was then.
type Check struct {
	Name     string `json:"name"`
	Pass     bool   `json:"pass"`
	Advisory bool   `json:"advisory,omitempty"`
	Detail   string `json:"detail"`
	// One of these lists is set, by the check.
	Recipients []string     `json:"recipients,omitempty"`
	Extracted  []Extracted  `json:"extracted,omitempty"`
	Refs       []Ref        `json:"refs,omitempty"`
	Links      []LinkDomain `json:"links,omitempty"`
	Unreadable []string     `json:"unreadable,omitempty"`
}

// Passed lists the names of the checks that passed.
func (v Verdict) Passed() []string {
	var out []string
	for _, c := range v.Checks {
		if c.Pass {
			out = append(out, c.Name)
		}
	}
	return out
}

// Failed lists the gates that did not pass: what escalated it.
func (v Verdict) Failed() []Check {
	return v.unpassed(false)
}

// Flagged lists the advisory checks that found something.
func (v Verdict) Flagged() []Check {
	return v.unpassed(true)
}

func (v Verdict) unpassed(advisory bool) []Check {
	var out []Check
	for _, c := range v.Checks {
		if !c.Pass && c.Advisory == advisory {
			out = append(out, c)
		}
	}
	return out
}

// Names are the names of cs.
func Names(cs []Check) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

// escalated is the summary of the gates that did not pass, "" if all
// did.
func (v Verdict) escalated() string {
	var why []string
	for _, c := range v.Failed() {
		why = append(why, c.Detail)
	}
	return strings.Join(why, "; ")
}

func review(g Grant, in Input, rcpts []string) Verdict {
	f := in.Review
	var v Verdict
	add := func(c Check, pass bool, failed string, passed string) {
		c.Pass, c.Detail = pass, passed
		if !pass {
			c.Detail = failed
		}
		v.Checks = append(v.Checks, c)
	}

	// First contact: nobody ever sent this address mail. That includes
	// a first answer to someone who wrote in, which is exactly the shape
	// of an injection; once a person lets one through, the address is
	// known.
	var fresh []string
	for _, r := range rcpts {
		if slices.Contains(in.Counters.FirstContact, r) {
			fresh = append(fresh, r)
		}
	}
	add(Check{Name: CheckFirstContact, Recipients: fresh}, len(fresh) == 0,
		fmt.Sprintf("first contact with %s", strings.Join(fresh, ", ")),
		"every recipient has been written to before")

	add(Check{Name: CheckExtracted, Extracted: f.Extracted}, len(f.Extracted) == 0,
		fmt.Sprintf("%d code(s) or sign-in link(s) from incoming mail in the message", len(f.Extracted)),
		"no code or sign-in link from incoming mail")

	// Cited mail the submitter cannot see, or mail of another thread
	// going to someone outside it. Advisory: the agent may well mean to
	// forward it, and the recipient of a reply is no proof it should.
	thread := ""
	if in.Reply != nil {
		thread = in.Reply.ThreadID
	}
	var refs []Ref
	for _, r := range f.Refs {
		switch {
		case r.MessageID == "":
			refs = append(refs, r)
		case r.ThreadID != thread:
			for _, rc := range rcpts {
				if !slices.Contains(fold(r.Correspondents), rc) {
					refs = append(refs, r)
					break
				}
			}
		}
	}
	add(Check{Name: CheckCrossThread, Advisory: true, Refs: refs}, len(refs) == 0,
		fmt.Sprintf("%d cited or attached message(s) unknown, or from another thread whose correspondents are not all the recipients: %s",
			len(refs), listed(refs, func(r Ref) string { return r.As })),
		"cites only this thread, or mail its recipients already had")

	var allowed []string
	for _, a := range g.Allowlist {
		if !strings.Contains(a, "@") {
			if d := leak.OrgDomain(a); d != "" {
				allowed = append(allowed, d)
			}
		}
	}
	var links []LinkDomain
	for _, l := range f.Links {
		if l.Seen == "" && (l.Domain == "" || !slices.Contains(allowed, l.Domain)) {
			links = append(links, l)
		}
	}
	// Advisory: a domain nobody authenticated wrote from is unfamiliar,
	// not wrong.
	add(Check{Name: CheckLinkDomains, Advisory: true, Links: links}, len(links) == 0,
		fmt.Sprintf("links to %d domain(s) never seen from an authenticated sender, a recipient, or the allowlist: %s",
			len(links), listed(links, func(l LinkDomain) string { return l.Host })),
		"links only to known domains")

	// A gate because the extracted check reads only text: a code inside
	// a PDF would pass it unseen.
	add(Check{Name: CheckReadable, Unreadable: f.Unreadable}, len(f.Unreadable) == 0,
		fmt.Sprintf("cannot read %s", strings.Join(f.Unreadable, ", ")),
		"every part is text the checks read")
	return v
}

// maxListed bounds the findings an advisory detail names: enough to act
// on, short enough for a card. The verdict keeps them all.
const maxListed = 5

// listed names the first maxListed of xs, and how many more there are.
func listed[T any](xs []T, name func(T) string) string {
	var out []string
	for _, x := range xs[:min(len(xs), maxListed)] {
		out = append(out, name(x))
	}
	if n := len(xs) - maxListed; n > 0 {
		out = append(out, fmt.Sprintf("and %d more", n))
	}
	return strings.Join(out, ", ")
}
