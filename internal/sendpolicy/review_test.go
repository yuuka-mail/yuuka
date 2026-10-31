package sendpolicy

import (
	"slices"
	"testing"
)

// reviewIn is an agent replying as itself under review: every check
// passes until a case breaks one.
func reviewIn(f ReviewFacts) Input {
	return Input{
		Submitter: agentP, CanSubmit: true, Identity: arisuID, Grant: grant(Review, ScopeAny, "example.org", "carol@example.com"),
		Recipients: []string{"bob@example.net"}, Reply: &Reply{ThreadID: "T1", Correspondents: []string{"bob@example.net"}},
		Review: &f,
	}
}

func failed(d Decision) []string {
	if d.Review == nil {
		return nil
	}
	var out []string
	for _, c := range d.Review.Checks {
		if !c.Pass {
			out = append(out, c.Name)
		}
	}
	return out
}

func TestReview(t *testing.T) {
	t.Parallel()
	seen := LinkDomain{Host: "example.net", Domain: "example.net", Seen: "inbound"}
	for _, c := range []struct {
		name   string
		in     func() Input
		failed []string // nil: held
	}{
		{"all pass", func() Input { return reviewIn(ReviewFacts{Links: []LinkDomain{seen}}) }, nil},
		{"first contact", func() Input {
			in := reviewIn(ReviewFacts{})
			in.Counters.FirstContact = []string{"bob@example.net"}
			return in
		}, []string{CheckFirstContact}},
		{"extracted value", func() Input {
			return reviewIn(ReviewFacts{Extracted: []Extracted{{Kind: "otp", MessageID: "M", Source: "body", Where: "body"}}})
		}, []string{CheckExtracted}},
		{"unresolved cite", func() Input { return reviewIn(ReviewFacts{Refs: []Ref{{As: "yuuka://message/X"}}}) }, []string{CheckCrossThread}},
		{"cite in this thread", func() Input {
			return reviewIn(ReviewFacts{Refs: []Ref{{As: "x", MessageID: "M", ThreadID: "T1"}}})
		}, nil},
		{"cite of another thread its correspondents had", func() Input {
			return reviewIn(ReviewFacts{Refs: []Ref{{As: "x", MessageID: "M", ThreadID: "T2", Correspondents: []string{"Bob@example.net"}}}})
		}, nil},
		{"cite of another thread, recipient outside it", func() Input {
			return reviewIn(ReviewFacts{Refs: []Ref{{As: "x", MessageID: "M", ThreadID: "T2", Correspondents: []string{"carol@example.net"}}}})
		}, []string{CheckCrossThread}},
		{"cite into a new thread", func() Input {
			in := reviewIn(ReviewFacts{Refs: []Ref{{As: "x", MessageID: "M", ThreadID: "T1", Correspondents: []string{"carol@example.net"}}}})
			in.Reply = nil
			return in
		}, []string{CheckCrossThread}},
		{"unknown link domain", func() Input {
			return reviewIn(ReviewFacts{Links: []LinkDomain{{Host: "evil.example", Domain: "evil.example"}}})
		}, []string{CheckLinkDomains}},
		{"IP literal", func() Input { return reviewIn(ReviewFacts{Links: []LinkDomain{{Host: "192.0.2.1"}}}) }, []string{CheckLinkDomains}},
		{"allowlisted domain, subdomain linked", func() Input {
			return reviewIn(ReviewFacts{Links: []LinkDomain{{Host: "docs.example.org", Domain: "example.org"}}})
		}, nil},
		{"an allowlisted address does not admit its domain", func() Input {
			return reviewIn(ReviewFacts{Links: []LinkDomain{{Host: "example.com", Domain: "example.com"}}})
		}, []string{CheckLinkDomains}},
		{"unreadable attachment", func() Input {
			return reviewIn(ReviewFacts{Unreadable: []string{"attachment 2 (a.pdf, application/pdf)"}})
		}, []string{CheckReadable}},
		// No short circuit: every reason at once.
		{"everything", func() Input {
			in := reviewIn(ReviewFacts{
				Unreadable: []string{"x"}, Extracted: []Extracted{{Kind: "link"}},
				Refs: []Ref{{As: "y"}}, Links: []LinkDomain{{Host: "evil.example"}},
			})
			in.Counters.FirstContact = []string{"bob@example.net"}
			return in
		}, []string{CheckFirstContact, CheckExtracted, CheckCrossThread, CheckLinkDomains, CheckReadable}},
	} {
		d := Decide(c.in())
		if d.Review == nil || len(d.Review.Checks) != 5 {
			t.Errorf("%s: verdict %+v", c.name, d.Review)
			continue
		}
		if got := failed(d); !slices.Equal(got, c.failed) {
			t.Errorf("%s: failed %q, want %q", c.name, got, c.failed)
		}
		if c.failed == nil {
			if !d.Queue || !d.Held || d.HoldSeconds != DefaultHoldSeconds || d.ApprovedBy != "" || d.AutoSubmitted != AutoSubmittedValue {
				t.Errorf("%s: %+v, want held for the default window, approved by nobody yet, automatic", c.name, d)
			}
		} else if d.Queue || d.Held || d.HoldReason != HoldReview || d.Detail == "" {
			t.Errorf("%s: %+v, want a draft held for review", c.name, d)
		}
	}
}

func TestReviewDecision(t *testing.T) {
	t.Parallel()
	// Without facts the review fails closed.
	in := reviewIn(ReviewFacts{})
	in.Review = nil
	if d := Decide(in); d.Queue || d.HoldReason != HoldReview || d.Review != nil {
		t.Errorf("no facts: %+v", d)
	}

	// The grant's window, 0 included.
	in = reviewIn(ReviewFacts{})
	in.Grant.HoldSeconds = ptr(0)
	if d := Decide(in); !d.Held || d.HoldSeconds != 0 {
		t.Errorf("hold 0: %+v", d)
	}
	in.Grant.HoldSeconds = ptr(90)
	if d := Decide(in); !d.Held || d.HoldSeconds != 90 {
		t.Errorf("hold 90: %+v", d)
	}

	// A human under a review grant: held, not automatic, released as
	// the human.
	h := reviewIn(ReviewFacts{})
	h.Submitter, h.Identity = otherP, serviceID
	if d := Decide(h); !d.Held || d.AutoSubmitted != "" || ReleasedBy(otherP) != other || ReleasedBy(agentP) != SystemPrincipalID {
		t.Errorf("human under review: %+v", d)
	}

	// Rules 1-9 come first and keep their reasons.
	lim := reviewIn(ReviewFacts{})
	lim.Grant.Limits.MaxPerHour = ptr(1)
	lim.Counters.SentLastHour = 1
	if d := Decide(lim); d.HoldReason != HoldLimit || d.Review != nil {
		t.Errorf("limit before review: %+v", d)
	}
	sus := reviewIn(ReviewFacts{})
	sus.Grant.Suspended = true
	if d := Decide(sus); d.HoldReason != HoldSuspended {
		t.Errorf("suspended before review: %+v", d)
	}

	// A human's confirmation replaces the review: queued, approved by
	// the human, no verdict.
	conf := reviewIn(ReviewFacts{Extracted: []Extracted{{Kind: "otp"}}})
	conf.ConfirmedBy = &humanP
	if d := Decide(conf); !d.Queue || d.Held || d.ApprovedBy != operator || d.Review != nil {
		t.Errorf("confirmation: %+v", d)
	}
}
