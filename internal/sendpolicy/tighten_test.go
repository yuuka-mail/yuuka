package sendpolicy

import (
	"slices"
	"testing"
)

func TestApplied(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   Input
		want Tier
	}{
		{"agent on the operator's identity, auto stored: capped", Input{Submitter: agentP, Identity: operatorID, Grant: grant(Auto, ScopeAny)}, Draft},
		{"agent on the operator's identity, review stored: capped", Input{Submitter: agentP, Identity: operatorID, Grant: grant(Review, ScopeAny)}, Draft},
		{"agent on the operator's identity, none stored: none", Input{Submitter: agentP, Identity: operatorID, Grant: grant(None, ScopeAny)}, None},
		{"agent on the operator's identity, no row: draft", Input{Submitter: agentP, Identity: operatorID}, Draft},
		{"another human on the operator's identity, auto stored: capped", Input{Submitter: otherP, Identity: operatorID, Grant: grant(Auto, ScopeAny)}, Draft},
		{"the owner, no row: auto", Input{Submitter: humanP, Identity: operatorID}, Auto},
		{"agent on its own identity, auto stored", Input{Submitter: agentP, Identity: arisuID, Grant: grant(Auto, ScopeAny)}, Auto},
		{"service on its own identity, no row: review", Input{Submitter: serviceP, Identity: serviceID}, Review},
	} {
		if got := Applied(tc.in).Policy; got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
	// Limits are resolved like Effective resolves them.
	g := Applied(Input{Submitter: agentP, Identity: arisuID, Grant: grant(Auto, ScopeAny)})
	if g.Limits.MaxPerHour == nil || *g.Limits.MaxPerHour != 10 {
		t.Errorf("limits not resolved: %+v", g.Limits)
	}
}

func n(v int) *int { return &v }

func TestWidenings(t *testing.T) {
	t.Parallel()
	tiers := []Tier{None, Draft, Review, Auto}
	scopes := []RecipientScope{ScopeThread, ScopeAllowlist, ScopeAny}
	base := Grant{
		Policy: Review, RecipientScope: ScopeAllowlist, Allowlist: []string{"a@example.net", "example.org"},
		Limits: Limits{MaxPerHour: n(10), MaxPerDay: n(50), MaxNewRecipientsPerDay: n(5)},
	}

	// Every pair of tiers and of scopes: wider exactly when ranked above.
	for i, from := range tiers {
		for j, to := range tiers {
			a, b := base, base
			a.Policy, b.Policy = from, to
			got := slices.Contains(Widenings(a, b), FieldPolicy)
			if got != (j > i) {
				t.Errorf("tier %s -> %s: widening %v", from, to, got)
			}
		}
	}
	// Scopes are a partial order: any is widest; thread and allowlist
	// are not nested, so either into the other widens.
	for _, tc := range []struct {
		from, to RecipientScope
		widens   bool
	}{
		{ScopeThread, ScopeThread, false},
		{ScopeThread, ScopeAllowlist, true},
		{ScopeThread, ScopeAny, true},
		{ScopeAllowlist, ScopeThread, true},
		{ScopeAllowlist, ScopeAllowlist, false},
		{ScopeAllowlist, ScopeAny, true},
		{ScopeAny, ScopeThread, false},
		{ScopeAny, ScopeAllowlist, false},
		{ScopeAny, ScopeAny, false},
	} {
		a, b := base, base
		a.RecipientScope, b.RecipientScope = tc.from, tc.to
		if got := slices.Contains(Widenings(a, b), FieldRecipientScope); got != tc.widens {
			t.Errorf("scope %s -> %s: widening %v", tc.from, tc.to, got)
		}
	}
	if len(scopes) != 3 {
		t.Fatal("a scope the table above does not cover")
	}

	for _, tc := range []struct {
		name string
		edit func(*Grant)
		want []string
	}{
		{"unchanged", func(*Grant) {}, nil},
		{"allowlist: an entry removed", func(g *Grant) { g.Allowlist = []string{"example.org"} }, nil},
		{"allowlist: emptied", func(g *Grant) { g.Allowlist = nil }, nil},
		{"allowlist: same entries, other case and order", func(g *Grant) { g.Allowlist = []string{"EXAMPLE.org", "A@example.net"} }, nil},
		{"allowlist: an entry added", func(g *Grant) { g.Allowlist = append(slices.Clone(g.Allowlist), "b@example.net") }, []string{FieldAllowlist}},
		{"allowlist: an address swapped for its domain", func(g *Grant) { g.Allowlist = []string{"example.net"} }, []string{FieldAllowlist}},
		{"limit: one lower", func(g *Grant) { g.Limits.MaxPerHour = n(9) }, nil},
		{"limit: zero", func(g *Grant) {
			g.Limits.MaxPerHour, g.Limits.MaxPerDay, g.Limits.MaxNewRecipientsPerDay = n(0), n(0), n(0)
		}, nil},
		{"limit: equal", func(g *Grant) { g.Limits.MaxPerDay = n(50) }, nil},
		{"limit: one higher", func(g *Grant) { g.Limits.MaxPerHour = n(11) }, []string{FieldMaxPerHour}},
		{"limit: none", func(g *Grant) { g.Limits.MaxPerDay = nil }, []string{FieldMaxPerDay}},
		{"limit: new recipients higher", func(g *Grant) { g.Limits.MaxNewRecipientsPerDay = n(6) }, []string{FieldMaxNewRecipientsPerDay}},
		{"suspended", func(g *Grant) { g.Suspended = true }, nil},
		{
			"everything at once",
			func(g *Grant) {
				g.Policy, g.RecipientScope, g.Allowlist = Auto, ScopeAny, []string{"c@example.com"}
				g.Limits = Limits{}
			},
			[]string{FieldPolicy, FieldRecipientScope, FieldAllowlist, FieldMaxPerHour, FieldMaxPerDay, FieldMaxNewRecipientsPerDay},
		},
	} {
		to := base
		to.Allowlist = slices.Clone(base.Allowlist)
		tc.edit(&to)
		if got := Widenings(base, to); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}

	// From no limit (a human's default) every number is tighter; from a
	// suspended grant, lifting it is the one widening.
	open := Grant{Policy: Auto, RecipientScope: ScopeAny}
	if w := Widenings(open, Grant{Policy: Auto, RecipientScope: ScopeAny, Limits: Limits{MaxPerHour: n(1000)}}); len(w) != 0 {
		t.Errorf("a number below no limit: %v", w)
	}
	susp := base
	susp.Suspended = true
	if w := Widenings(susp, base); !slices.Equal(w, []string{FieldSuspended}) {
		t.Errorf("lifting a suspension: %v", w)
	}
	// A column that does not act at the new tier still counts: it would
	// once the tier is raised on the host.
	low := base
	low.Policy, low.RecipientScope = Draft, ScopeAny
	if w := Widenings(base, low); !slices.Equal(w, []string{FieldRecipientScope}) {
		t.Errorf("draft with a wider scope: %v", w)
	}
}

func TestRemovalWidenings(t *testing.T) {
	t.Parallel()
	applied := func(in Input) Grant { return Applied(in) }
	for _, tc := range []struct {
		name string
		row  Input // with the grant row
		want []string
	}{
		// The default is draft (agent on a human's identity): only the
		// tier compares, whatever else the row says.
		{"agent on the operator's identity, row none", Input{Submitter: agentP, Identity: operatorID, Grant: grant(None, ScopeThread)}, []string{FieldPolicy}},
		{"agent on the operator's identity, row draft", Input{Submitter: agentP, Identity: operatorID, Grant: grant(Draft, ScopeThread)}, nil},
		{
			"agent on the operator's identity, row draft and suspended",
			Input{Submitter: agentP, Identity: operatorID, Grant: &Grant{Policy: Draft, RecipientScope: ScopeThread, Suspended: true}},
			nil,
		},
		// The default is none: removing never widens.
		{"service on the operator's identity, row draft", Input{Submitter: serviceP, Identity: operatorID, Grant: grant(Draft, ScopeAny)}, nil},
		{"human on an agent's identity, row auto", Input{Submitter: humanP, Identity: arisuID, Grant: grant(Auto, ScopeAny)}, nil},
		// The default is review + thread with default limits.
		{"agent on its own identity, row auto any", Input{Submitter: agentP, Identity: arisuID, Grant: grant(Auto, ScopeAny)}, nil},
		{"agent on its own identity, row review thread", Input{Submitter: agentP, Identity: arisuID, Grant: grant(Review, ScopeThread)}, nil},
		{
			"agent on its own identity, row auto allowlist: the default's thread is not within it",
			Input{Submitter: agentP, Identity: arisuID, Grant: grant(Auto, ScopeAllowlist, "a@example.net")},
			[]string{FieldRecipientScope},
		},
		{"agent on its own identity, row draft", Input{Submitter: agentP, Identity: arisuID, Grant: grant(Draft, ScopeThread)}, []string{FieldPolicy}},
		{"agent on its own identity, row none", Input{Submitter: agentP, Identity: arisuID, Grant: grant(None, ScopeAny)}, []string{FieldPolicy}},
		{
			"agent on its own identity, row with a lower limit",
			Input{Submitter: agentP, Identity: arisuID, Grant: &Grant{Policy: Auto, RecipientScope: ScopeAny, Limits: Limits{MaxPerHour: n(3)}}},
			[]string{FieldMaxPerHour},
		},
		{
			"agent on its own identity, row with a higher limit",
			Input{Submitter: agentP, Identity: arisuID, Grant: &Grant{Policy: Auto, RecipientScope: ScopeAny, Limits: Limits{MaxPerHour: n(30)}}},
			nil,
		},
		{
			"agent on its own identity, row suspended",
			Input{Submitter: agentP, Identity: arisuID, Grant: &Grant{Policy: Auto, RecipientScope: ScopeAny, Suspended: true}},
			[]string{FieldSuspended},
		},
		// The owner's default is auto + any without limits.
		{"the owner, row auto any", Input{Submitter: humanP, Identity: operatorID, Grant: grant(Auto, ScopeAny)}, nil},
		{"the owner, row review", Input{Submitter: humanP, Identity: operatorID, Grant: grant(Review, ScopeAny)}, []string{FieldPolicy}},
		{"the owner, row auto thread", Input{Submitter: humanP, Identity: operatorID, Grant: grant(Auto, ScopeThread)}, []string{FieldRecipientScope}},
		{
			"the owner, row with a limit",
			Input{Submitter: humanP, Identity: operatorID, Grant: &Grant{Policy: Auto, RecipientScope: ScopeAny, Limits: Limits{MaxPerDay: n(100)}}},
			[]string{FieldMaxPerDay},
		},
	} {
		def := tc.row
		def.Grant = nil
		if got := RemovalWidenings(applied(tc.row), applied(def)); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
