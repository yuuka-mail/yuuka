package rules

import (
	"cmp"
	"context"
	"slices"
)

// Rule is an enabled rule as Combine needs it.
type Rule struct {
	ID       string // rules.id, a ULID
	Priority int    // ascending; ties broken by ID
	Program  *Program
}

// Effect is one action that took effect, attributed to the rule that
// won it. The store writes one actions row per rule with effects, as
// actor rule:<id>.
type Effect struct {
	Rule   string
	Action Action
}

// Hit is one rule that matched (Err nil) or failed closed (Err wraps
// ErrUnsupported or the context's error). Rules that evaluated to false
// are not listed. A matching rule whose actions all lost to earlier rules
// is a hit with no effects.
type Hit struct {
	Rule string
	Err  error
}

// Intent is what a round of rules wants done to one (message, delivery).
type Intent struct {
	Effects []Effect // in rule order, then action order
	Hits    []Hit    // in rule order
}

// Combine evaluates rules on f and resolves their actions. Frozen v1
// semantics:
//
//   - Rules run in (Priority, ID) order, ID compared bytewise; Combine
//     sorts, so callers cannot get the order wrong.
//   - Every rule sees the same facts: no rule sees another's effects,
//     and there is no stop.
//   - Single-valued outcomes (classify, archive, the notify level) go to
//     the first matching rule that sets them.
//   - Labels are the union, each attributed to the first rule adding it.
//
// When ctx is done, the rules not yet evaluated fail closed; the effects
// of rules already decided stand.
func Combine(ctx context.Context, rules []Rule, f *Facts) Intent {
	sorted := slices.Clone(rules)
	slices.SortStableFunc(sorted, func(a, b Rule) int {
		return cmp.Or(cmp.Compare(a.Priority, b.Priority), cmp.Compare(a.ID, b.ID))
	})
	v := newView(f)
	var in Intent
	single := map[string]bool{}
	labels := map[string]bool{}
	for _, r := range sorted {
		ok, err := r.Program.eval(ctx, v)
		if err != nil {
			in.Hits = append(in.Hits, Hit{Rule: r.ID, Err: err})
			continue
		}
		if !ok {
			continue
		}
		in.Hits = append(in.Hits, Hit{Rule: r.ID})
		for _, a := range r.Program.actions {
			if a.Kind == KindLabel {
				if labels[a.Label] {
					continue
				}
				labels[a.Label] = true
			} else {
				if single[a.Kind] {
					continue
				}
				single[a.Kind] = true
			}
			in.Effects = append(in.Effects, Effect{Rule: r.ID, Action: a})
		}
	}
	return in
}
