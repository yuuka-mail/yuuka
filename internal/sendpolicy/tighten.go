package sendpolicy

import "slices"

// Applied is the grant that acts on in: Effective, with the owner cap
// lowering its tier as Decide does. It is what a person reading a grant
// is told, and what a tightening is compared against (§13.10).
func Applied(in Input) Grant {
	g := Effective(in)
	if OwnerCapped(in.Submitter.ID, in.Identity) && g.Policy.rank() > Draft.rank() {
		g.Policy = Draft
	}
	return g
}

// Sends reports whether a grant at tier t lets mail out without a
// person's confirmation: review and auto. At none and draft Decide holds
// every submission before it looks at the rest of the grant.
func (t Tier) Sends() bool { return t.rank() > Draft.rank() }

// within reports whether scope r admits no one that from does not. The
// scopes are a partial order: any admits everyone; thread (the thread's
// correspondents, whoever joins later) and allowlist (a fixed list) are
// not nested, so neither is within the other. An unknown scope admits
// nobody.
func (r RecipientScope) within(from RecipientScope) bool {
	switch {
	case r == from, from == ScopeAny:
		return true
	case r == ScopeThread, r == ScopeAllowlist, r == ScopeAny:
		return false
	}
	return true
}

// The columns of identity_grants that Widenings names.
const (
	FieldPolicy                 = "send_policy"
	FieldRecipientScope         = "recipient_scope"
	FieldAllowlist              = "recipient_allowlist"
	FieldMaxPerHour             = "max_per_hour"
	FieldMaxPerDay              = "max_per_day"
	FieldMaxNewRecipientsPerDay = "max_new_recipients_per_day"
	FieldHoldSeconds            = "hold_seconds"
	FieldSuspended              = "suspended"
)

// Widenings lists the columns in which to lets more through than from,
// none when to is at least as tight in every one. Both are applied
// grants (Applied): limits resolved from DefaultLimits, the tier after
// the owner cap. The orders, tightest first:
//
//   - send_policy: none < draft < review < auto
//   - recipient_scope: thread < any and allowlist < any; thread and
//     allowlist are not comparable, so changing one into the other is a
//     widening either way: thread reaches whoever joins a conversation,
//     an allowlist reaches people in no conversation yet
//   - recipient_allowlist: to's entries are a subset of from's (folded),
//     whatever the scope, so a later raise of the scope cannot bring
//     back an entry nobody chose
//   - each limit: a smaller number; no limit (nil) is above every number
//   - hold_seconds: a longer recall window (EffectiveHoldSeconds); a
//     shorter one gives a person less time to recall held mail, 0 none
//   - suspended: suspended < not suspended
//
// Every column is compared, even one that does not act at to's tier: a
// value stored now acts once someone raises the tier on the host, and
// that person reads the grant as it is then, not as it was.
func Widenings(from, to Grant) []string {
	var out []string
	if to.Policy.rank() > from.Policy.rank() {
		out = append(out, FieldPolicy)
	}
	if !to.RecipientScope.within(from.RecipientScope) {
		out = append(out, FieldRecipientScope)
	}
	allow := fold(from.Allowlist)
	for _, e := range fold(to.Allowlist) {
		if !slices.Contains(allow, e) {
			out = append(out, FieldAllowlist)
			break
		}
	}
	for _, l := range []struct {
		field    string
		from, to *int
	}{
		{FieldMaxPerHour, from.Limits.MaxPerHour, to.Limits.MaxPerHour},
		{FieldMaxPerDay, from.Limits.MaxPerDay, to.Limits.MaxPerDay},
		{FieldMaxNewRecipientsPerDay, from.Limits.MaxNewRecipientsPerDay, to.Limits.MaxNewRecipientsPerDay},
	} {
		if l.from != nil && (l.to == nil || *l.to > *l.from) {
			out = append(out, l.field)
		}
	}
	if EffectiveHoldSeconds(to.HoldSeconds) < EffectiveHoldSeconds(from.HoldSeconds) {
		out = append(out, FieldHoldSeconds)
	}
	if from.Suspended && !to.Suspended {
		out = append(out, FieldSuspended)
	}
	return out
}

// RemovalWidenings is Widenings for removing a grant: from is the row
// applied, def what applies without it (Applied with no Grant). A
// default that does not send makes the rest of it moot (Decide stops at
// the tier), and nothing of it is stored to act later, so only the tier
// is compared then. For the same reason the recall windows compared are
// the ones that act: a grant's hold at review, none at auto.
func RemovalWidenings(from, def Grant) []string {
	if !def.Policy.Sends() {
		if def.Policy.rank() > from.Policy.rank() {
			return []string{FieldPolicy}
		}
		return nil
	}
	return Widenings(actingHold(from), actingHold(def))
}

// actingHold is g with the recall window that acts at its tier: at auto
// mail goes out at once, whatever hold_seconds says.
func actingHold(g Grant) Grant {
	if g.Policy != Review {
		g.HoldSeconds = ptr(0)
	}
	return g
}
