package rules

// Candidate is one condition offered when turning an example message into
// a rule, with whether it is ticked by default.
type Candidate struct {
	Condition Match // a leaf
	Selected  bool
}

// Generalize offers conditions that describe f's sender and recipient,
// for "handle mail like this automatically" (§8.5). The defaults:
//
//   - from.addr eq, plus from.verified = true when the example passed
//     DMARC, so the rule cannot be triggered by forging that From;
//   - list.id eq instead of from.addr when the message has a List-Id,
//     since that is what identifies a discussion list's mail;
//   - offered unticked: from.domain, from.org (when it differs from the
//     domain) and to.addr, to widen or narrow by hand;
//   - to.addr ticked instead when there is neither a List-Id nor a From
//     address (from.verified is then not ticked either: alone it would
//     match all authenticated mail).
//
// It never draws a condition from the subject or the body: the sender
// writes those, and a rule learned from them is a rule the sender can
// steer. from.verified = true is not offered for an unverified example,
// because the draft would not match its own example.
func Generalize(f *Facts) []Candidate {
	v := newView(f)
	leaf := func(field string, value any) Match {
		return Match{Field: field, Op: OpEq, Value: value}
	}
	var cs []Candidate
	listID := v.text[FieldListID]
	if listID != "" && len(listID) <= MaxValueBytes {
		cs = append(cs, Candidate{leaf(FieldListID, listID), true})
	}
	from, domain, org := v.text[FieldFromAddr], v.text[FieldFromDomain], v.text[FieldFromOrg]
	if from != "" && len(from) <= MaxValueBytes {
		cs = append(cs, Candidate{leaf(FieldFromAddr, from), len(cs) == 0})
	}
	if domain != "" && len(domain) <= MaxValueBytes {
		cs = append(cs, Candidate{leaf(FieldFromDomain, domain), false})
		if org != domain {
			cs = append(cs, Candidate{leaf(FieldFromOrg, org), false})
		}
	}
	// Whether a sender condition is ticked; from.verified alone would
	// match all authenticated mail.
	identified := len(cs) > 0 && cs[0].Selected
	if f.FromVerified {
		cs = append(cs, Candidate{leaf(FieldFromVerified, true), identified})
	}
	to := v.text[FieldToAddr]
	if f.ToPrefix != "" && f.ToZone != "" && len(to) <= MaxValueBytes {
		cs = append(cs, Candidate{leaf(FieldToAddr, to), !identified})
	}
	return cs
}

// Draft joins the selected candidates into a match tree: an all of them,
// or false when none is selected.
func Draft(cs []Candidate) (Match, bool) {
	var all []Match
	for _, c := range cs {
		if c.Selected {
			all = append(all, c.Condition)
		}
	}
	if len(all) == 0 {
		return Match{}, false
	}
	return Match{All: all}, true
}
