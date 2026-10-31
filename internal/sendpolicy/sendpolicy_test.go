package sendpolicy

import (
	"testing"
)

const (
	operator = "01JHUMAN000000000000000000"
	other    = "01JHUMAN100000000000000000"
	arisu    = "01JAGENT000000000000000000"
	monitor  = "01JSERVC000000000000000000"
)

var (
	humanP   = Principal{ID: operator, Kind: KindHuman}
	otherP   = Principal{ID: other, Kind: KindHuman}
	agentP   = Principal{ID: arisu, Kind: KindAgent}
	serviceP = Principal{ID: monitor, Kind: KindService}

	operatorID = Identity{ID: "op", OwnerID: operator, OwnerKind: KindHuman}
	arisuID    = Identity{ID: "arisu", OwnerID: arisu, OwnerKind: KindAgent}
	alertsID   = Identity{ID: "alerts", OwnerID: operator, OwnerKind: KindHuman}
	serviceID  = Identity{ID: "svc", OwnerID: monitor, OwnerKind: KindService}
)

func grant(t Tier, s RecipientScope, allow ...string) *Grant {
	return &Grant{Policy: t, RecipientScope: s, Allowlist: allow}
}

func reply(correspondents ...string) *Reply { return &Reply{Correspondents: correspondents} }

type want struct {
	queue    bool
	approver string
	reason   HoldReason
	tier     Tier
	auto     bool
}

func TestDecide(t *testing.T) {
	t.Parallel()
	bob := []string{"Bob@Example.net"}
	for _, tc := range []struct {
		name string
		in   Input
		want want
	}{
		// Defaults without a grant row (§13.5).
		{
			"human, own identity, no grant: auto",
			Input{Submitter: humanP, CanSubmit: true, Identity: operatorID, Recipients: bob},
			want{queue: true, approver: operator, tier: Auto},
		},
		{
			"human, someone else's human identity: owner cap",
			Input{Submitter: otherP, CanSubmit: true, Identity: operatorID, Recipients: bob},
			want{reason: HoldOwner, tier: None},
		},
		{
			"human, agent identity, no grant: none",
			Input{Submitter: humanP, CanSubmit: true, Identity: arisuID, Recipients: bob},
			want{reason: HoldPolicy, tier: None},
		},
		{
			"agent, own identity, no grant: review without facts is a draft",
			Input{
				Submitter: agentP, CanSubmit: true, Identity: arisuID,
				Recipients: bob, Reply: reply("bob@example.net"),
			},
			want{reason: HoldReview, tier: Review},
		},
		{
			"agent, own identity, no grant, new thread: thread scope",
			Input{Submitter: agentP, CanSubmit: true, Identity: arisuID, Recipients: bob},
			want{reason: HoldRecipients, tier: Review},
		},
		{
			"agent, human identity, no grant: draft",
			Input{Submitter: agentP, CanSubmit: true, Identity: operatorID, Recipients: bob},
			want{reason: HoldOwner, tier: Draft},
		},
		{
			"service, human identity, no grant: none, the SMTP port rejects",
			Input{Submitter: serviceP, CanSubmit: true, Identity: operatorID, Recipients: bob},
			want{reason: HoldOwner, tier: None},
		},
		{
			"service, own identity, no grant: review",
			Input{
				Submitter: serviceP, CanSubmit: true, Identity: serviceID, Recipients: bob,
				Reply: reply("bob@example.net"),
			},
			want{reason: HoldReview, tier: Review},
		},

		// The hard cap: a grant cannot raise it.
		{
			"agent, human identity, grant auto: capped",
			Input{
				Submitter: agentP, CanSubmit: true, Identity: operatorID,
				Grant: grant(Auto, ScopeAny), Recipients: bob,
			},
			want{reason: HoldOwner, tier: Draft},
		},
		{
			"service, human-owned alerts@, grant auto: capped",
			Input{
				Submitter: serviceP, CanSubmit: true, Identity: alertsID,
				Grant: grant(Auto, ScopeAllowlist, "example.net"), Recipients: bob,
			},
			want{reason: HoldOwner, tier: Draft},
		},
		{
			"agent, human identity, grant none: none",
			Input{
				Submitter: agentP, CanSubmit: true, Identity: operatorID,
				Grant: grant(None, ScopeAny), Recipients: bob,
			},
			want{reason: HoldOwner, tier: None},
		},
		{
			"owner cap before the scope check",
			Input{
				Submitter: agentP, CanSubmit: false, Identity: operatorID,
				Grant: grant(Auto, ScopeAny), Recipients: bob,
			},
			want{reason: HoldOwner, tier: Draft},
		},

		// Tiers.
		{
			"no submission:send",
			Input{Submitter: agentP, CanSubmit: false, Identity: arisuID, Grant: grant(Auto, ScopeAny), Recipients: bob},
			want{reason: HoldPolicy, tier: Auto},
		},
		{
			"grant none",
			Input{Submitter: agentP, CanSubmit: true, Identity: arisuID, Grant: grant(None, ScopeAny), Recipients: bob},
			want{reason: HoldPolicy, tier: None},
		},
		{
			"grant draft",
			Input{Submitter: agentP, CanSubmit: true, Identity: arisuID, Grant: grant(Draft, ScopeAny), Recipients: bob},
			want{reason: HoldPolicy, tier: Draft},
		},
		{
			"human's own grant set to draft",
			Input{Submitter: humanP, CanSubmit: true, Identity: operatorID, Grant: grant(Draft, ScopeAny), Recipients: bob},
			want{reason: HoldPolicy, tier: Draft},
		},
		{
			"grant review",
			Input{Submitter: agentP, CanSubmit: true, Identity: arisuID, Grant: grant(Review, ScopeAny), Recipients: bob},
			want{reason: HoldReview, tier: Review},
		},
		{
			"agent auto: approved by system, Auto-Submitted",
			Input{
				Submitter: agentP, CanSubmit: true, Identity: arisuID,
				Grant: grant(Auto, ScopeAny), Recipients: bob,
			},
			want{queue: true, approver: SystemPrincipalID, tier: Auto, auto: true},
		},
		{
			"service auto on its own identity",
			Input{
				Submitter: serviceP, CanSubmit: true, Identity: serviceID,
				Grant: grant(Auto, ScopeAllowlist, "bob@example.net"), Recipients: bob,
			},
			want{queue: true, approver: SystemPrincipalID, tier: Auto, auto: true},
		},
		{
			"service auto on a service identity it does not own",
			Input{
				Submitter: serviceP, CanSubmit: true, Identity: arisuID,
				Grant: grant(Auto, ScopeAny), Recipients: bob,
			},
			want{queue: true, approver: SystemPrincipalID, tier: Auto, auto: true},
		},
		{
			"identity disabled",
			Input{
				Submitter: humanP, CanSubmit: true, Identity: Identity{ID: "x", OwnerID: operator, OwnerKind: KindHuman, Disabled: true},
				Recipients: bob,
			},
			want{reason: HoldPolicy, tier: Auto},
		},
		{
			"suspended",
			Input{
				Submitter: agentP, CanSubmit: true, Identity: arisuID,
				Grant: &Grant{Policy: Auto, RecipientScope: ScopeAny, Suspended: true}, Recipients: bob,
			},
			want{reason: HoldSuspended, tier: Auto},
		},

		// Recipient scope.
		{
			"no recipients",
			Input{Submitter: humanP, CanSubmit: true, Identity: operatorID, Recipients: []string{" "}},
			want{reason: HoldRecipients, tier: Auto},
		},
		{
			"thread: correspondent in another spelling",
			Input{
				Submitter: agentP, CanSubmit: true, Identity: arisuID,
				Grant: grant(Auto, ScopeThread), Recipients: bob, Reply: reply("bob@example.net", "carol@example.org"),
			},
			want{queue: true, approver: SystemPrincipalID, tier: Auto, auto: true},
		},
		{
			"thread: a stranger in Bcc",
			Input{
				Submitter: agentP, CanSubmit: true, Identity: arisuID,
				Grant: grant(Auto, ScopeThread), Recipients: []string{"bob@example.net", "eve@example.org"}, Reply: reply("bob@example.net"),
			},
			want{reason: HoldRecipients, tier: Auto},
		},
		{
			"thread: not a reply",
			Input{Submitter: agentP, CanSubmit: true, Identity: arisuID, Grant: grant(Auto, ScopeThread), Recipients: bob},
			want{reason: HoldRecipients, tier: Auto},
		},
		{
			"allowlist: domain",
			Input{
				Submitter: serviceP, CanSubmit: true, Identity: serviceID,
				Grant: grant(Auto, ScopeAllowlist, "EXAMPLE.NET"), Recipients: bob,
			},
			want{queue: true, approver: SystemPrincipalID, tier: Auto, auto: true},
		},
		{
			"allowlist: no subdomains",
			Input{
				Submitter: serviceP, CanSubmit: true, Identity: serviceID,
				Grant: grant(Auto, ScopeAllowlist, "example.net"), Recipients: []string{"bob@mail.example.net"},
			},
			want{reason: HoldRecipients, tier: Auto},
		},
		{
			"allowlist: address",
			Input{
				Submitter: serviceP, CanSubmit: true, Identity: serviceID,
				Grant: grant(Auto, ScopeAllowlist, "ops@example.org"), Recipients: []string{"ops@example.org", "bob@example.org"},
			},
			want{reason: HoldRecipients, tier: Auto},
		},
		{
			"unknown scope admits nobody",
			Input{
				Submitter: humanP, CanSubmit: true, Identity: operatorID,
				Grant: grant(Auto, RecipientScope("everyone")), Recipients: bob,
			},
			want{reason: HoldRecipients, tier: Auto},
		},

		// Mail loops.
		{
			"agent replying to Auto-Submitted",
			Input{
				Submitter: agentP, CanSubmit: true, Identity: arisuID, Grant: grant(Auto, ScopeAny),
				Recipients: bob, Reply: &Reply{AutoSubmitted: "Auto-Replied"},
			},
			want{reason: HoldLoop, tier: Auto},
		},
		{
			"Auto-Submitted: no is a person",
			Input{
				Submitter: agentP, CanSubmit: true, Identity: arisuID, Grant: grant(Auto, ScopeAny),
				Recipients: bob, Reply: &Reply{AutoSubmitted: "no"},
			},
			want{queue: true, approver: SystemPrincipalID, tier: Auto, auto: true},
		},
		{
			"agent replying to Precedence: list",
			Input{
				Submitter: agentP, CanSubmit: true, Identity: arisuID, Grant: grant(Auto, ScopeAny),
				Recipients: bob, Reply: &Reply{Precedence: "list"},
			},
			want{reason: HoldLoop, tier: Auto},
		},
		{
			"agent replying to class=machine",
			Input{
				Submitter: agentP, CanSubmit: true, Identity: arisuID, Grant: grant(Auto, ScopeAny),
				Recipients: bob, Reply: &Reply{Machine: true},
			},
			want{reason: HoldLoop, tier: Auto},
		},
		{
			"a human may answer a machine",
			Input{
				Submitter: humanP, CanSubmit: true, Identity: operatorID,
				Recipients: bob, Reply: &Reply{Machine: true, AutoSubmitted: "auto-generated"},
			},
			want{queue: true, approver: operator, tier: Auto},
		},
		{
			"per-thread automatic cap",
			Input{
				Submitter: agentP, CanSubmit: true, Identity: arisuID, Grant: grant(Auto, ScopeAny),
				Recipients: bob, Reply: &Reply{}, Counters: Counters{AutoInThread: map[string]int{"bob@example.net": MaxAutoPerThreadRecipient}},
			},
			want{reason: HoldLoop, tier: Auto},
		},
		{
			"per-thread automatic cap not yet",
			Input{
				Submitter: agentP, CanSubmit: true, Identity: arisuID, Grant: grant(Auto, ScopeAny),
				Recipients: bob, Reply: &Reply{}, Counters: Counters{AutoInThread: map[string]int{"bob@example.net": MaxAutoPerThreadRecipient - 1}},
			},
			want{queue: true, approver: SystemPrincipalID, tier: Auto, auto: true},
		},

		// Limits: defaults by kind, explicit values, the order.
		{
			"agent default per hour",
			Input{
				Submitter: agentP, CanSubmit: true, Identity: arisuID, Grant: grant(Auto, ScopeAny),
				Recipients: bob, Counters: Counters{SentLastHour: 10},
			},
			want{reason: HoldLimit, tier: Auto},
		},
		{
			"agent below default per hour",
			Input{
				Submitter: agentP, CanSubmit: true, Identity: arisuID, Grant: grant(Auto, ScopeAny),
				Recipients: bob, Counters: Counters{SentLastHour: 9, SentLastDay: 49},
			},
			want{queue: true, approver: SystemPrincipalID, tier: Auto, auto: true},
		},
		{
			"agent default per day",
			Input{
				Submitter: agentP, CanSubmit: true, Identity: arisuID, Grant: grant(Auto, ScopeAny),
				Recipients: bob, Counters: Counters{SentLastDay: 50},
			},
			want{reason: HoldLimit, tier: Auto},
		},
		{
			"service default per hour is higher",
			Input{
				Submitter: serviceP, CanSubmit: true, Identity: serviceID, Grant: grant(Auto, ScopeAny),
				Recipients: bob, Counters: Counters{SentLastHour: 29},
			},
			want{queue: true, approver: SystemPrincipalID, tier: Auto, auto: true},
		},
		{
			"explicit limit overrides the default",
			Input{
				Submitter: agentP, CanSubmit: true, Identity: arisuID,
				Grant:      &Grant{Policy: Auto, RecipientScope: ScopeAny, Limits: Limits{MaxPerHour: ptr(100)}},
				Recipients: bob, Counters: Counters{SentLastHour: 50},
			},
			want{queue: true, approver: SystemPrincipalID, tier: Auto, auto: true},
		},
		{
			"explicit zero",
			Input{
				Submitter: humanP, CanSubmit: true, Identity: operatorID,
				Grant: &Grant{Policy: Auto, RecipientScope: ScopeAny, Limits: Limits{MaxPerDay: ptr(0)}}, Recipients: bob,
			},
			want{reason: HoldLimit, tier: Auto},
		},
		{
			"human has no default limit",
			Input{
				Submitter: humanP, CanSubmit: true, Identity: operatorID,
				Recipients: bob, Counters: Counters{SentLastHour: 1000, SentLastDay: 10000, NewRecipientsLastDay: 1000, FirstContact: []string{"bob@example.net"}},
			},
			want{queue: true, approver: operator, tier: Auto},
		},
		{
			"new recipients",
			Input{
				Submitter: agentP, CanSubmit: true, Identity: arisuID, Grant: grant(Auto, ScopeAny),
				Recipients: []string{"a@example.net", "b@example.net"},
				Counters:   Counters{NewRecipientsLastDay: 4, FirstContact: []string{"a@example.net", "b@example.net"}},
			},
			want{reason: HoldLimit, tier: Auto},
		},
		{
			"known recipients do not count as new",
			Input{
				Submitter: agentP, CanSubmit: true, Identity: arisuID, Grant: grant(Auto, ScopeAny),
				Recipients: []string{"a@example.net", "b@example.net"},
				Counters:   Counters{NewRecipientsLastDay: 5, FirstContact: nil},
			},
			want{queue: true, approver: SystemPrincipalID, tier: Auto, auto: true},
		},
		{
			"recipients before limits",
			Input{
				Submitter: agentP, CanSubmit: true, Identity: arisuID, Grant: grant(Auto, ScopeThread),
				Recipients: bob, Counters: Counters{SentLastHour: 99},
			},
			want{reason: HoldRecipients, tier: Auto},
		},
		{
			"suspended before recipients",
			Input{
				Submitter: agentP, CanSubmit: true, Identity: arisuID,
				Grant: &Grant{Policy: Auto, RecipientScope: ScopeThread, Suspended: true}, Recipients: bob,
			},
			want{reason: HoldSuspended, tier: Auto},
		},
		{
			"limits before review",
			Input{
				Submitter: agentP, CanSubmit: true, Identity: arisuID, Grant: grant(Review, ScopeAny),
				Recipients: bob, Counters: Counters{SentLastHour: 10},
			},
			want{reason: HoldLimit, tier: Review},
		},

		// A human's confirmation of a held draft.
		{
			"operator confirms arisu's draft on the operator's identity",
			Input{
				Submitter: agentP, CanSubmit: true, ConfirmedBy: &humanP,
				Identity: operatorID, Recipients: bob,
			},
			want{queue: true, approver: operator, tier: Draft},
		},
		{
			"another human cannot confirm as the operator",
			Input{
				Submitter: agentP, CanSubmit: true, ConfirmedBy: &otherP,
				Identity: operatorID, Recipients: bob,
			},
			want{reason: HoldOwner, tier: Draft},
		},
		{
			"an agent cannot confirm",
			Input{
				Submitter: agentP, CanSubmit: true, ConfirmedBy: &agentP,
				Identity: arisuID, Recipients: bob,
			},
			want{reason: HoldPolicy, tier: Review},
		},
		{
			"confirmation needs submission:send",
			Input{
				Submitter: agentP, CanSubmit: false, ConfirmedBy: &humanP,
				Identity: operatorID, Recipients: bob,
			},
			want{reason: HoldPolicy, tier: Draft},
		},
		{
			"confirmation replaces limits, scope and suspension",
			Input{
				Submitter: agentP, CanSubmit: true, ConfirmedBy: &humanP,
				Identity: arisuID, Grant: &Grant{Policy: Draft, RecipientScope: ScopeThread, Suspended: true},
				Recipients: bob, Reply: &Reply{Machine: true}, Counters: Counters{SentLastHour: 1000},
			},
			want{queue: true, approver: operator, tier: Draft},
		},
		{
			"confirmation does not revive a disabled identity",
			Input{
				Submitter: agentP, CanSubmit: true, ConfirmedBy: &humanP,
				Identity: Identity{ID: "x", OwnerID: arisu, OwnerKind: KindAgent, Disabled: true}, Recipients: bob,
			},
			want{reason: HoldPolicy, tier: Review},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := Decide(tc.in)
			if d.Queue != tc.want.queue || d.ApprovedBy != tc.want.approver || d.HoldReason != tc.want.reason || d.Tier != tc.want.tier {
				t.Errorf("got queue=%v approver=%q reason=%q tier=%q (%s); want %+v", d.Queue, d.ApprovedBy, d.HoldReason, d.Tier, d.Detail, tc.want)
			}
			if got := d.AutoSubmitted != ""; got != tc.want.auto {
				t.Errorf("AutoSubmitted = %q, want set %v", d.AutoSubmitted, tc.want.auto)
			}
			if d.Queue && d.HoldReason != "" || !d.Queue && (d.HoldReason == "" || d.Detail == "" || d.ApprovedBy != "") {
				t.Errorf("inconsistent decision %+v", d)
			}
		})
	}
}

// Whatever the grant, the counters or the confirmation, nobody but its
// owner gets mail out as a human-owned identity.
func TestOwnerCapHolds(t *testing.T) {
	t.Parallel()
	tiers := []Tier{None, Draft, Review, Auto}
	scopes := []RecipientScope{ScopeThread, ScopeAllowlist, ScopeAny}
	submitters := []Principal{otherP, agentP, serviceP, {ID: "r", Kind: "rule"}, {ID: SystemPrincipalID, Kind: "system"}}
	for _, s := range submitters {
		for _, conf := range []*Principal{nil, &otherP, &agentP, &serviceP} {
			for _, g := range append([]*Grant{nil}, func() []*Grant {
				var gs []*Grant
				for _, tr := range tiers {
					for _, sc := range scopes {
						gs = append(gs, &Grant{Policy: tr, RecipientScope: sc, Allowlist: []string{"example.net"}})
					}
				}
				return gs
			}()...) {
				// The review dimension: no facts, a review that passes,
				// one that escalates. None of them lifts the cap.
				for _, rf := range []*ReviewFacts{nil, {}, {Unreadable: []string{"x"}}} {
					in := Input{
						Submitter: s, CanSubmit: true, ConfirmedBy: conf, Identity: operatorID, Grant: g,
						Recipients: []string{"bob@example.net"}, Reply: reply("bob@example.net"), Review: rf,
					}
					d := Decide(in)
					if d.Queue || d.Held || d.Tier.rank() > Draft.rank() {
						t.Fatalf("%+v confirmed by %+v with grant %+v, review %+v: %+v", s, conf, g, rf, d)
					}
				}
			}
		}
	}
}

func TestParse(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"none", "draft", "review", "auto"} {
		if _, err := ParseTier(s); err != nil {
			t.Error(err)
		}
	}
	if _, err := ParseTier("Auto"); err == nil {
		t.Error("ParseTier is case-sensitive like the column CHECK")
	}
	for _, s := range []string{"thread", "allowlist", "any"} {
		if _, err := ParseRecipientScope(s); err != nil {
			t.Error(err)
		}
	}
	if _, err := ParseRecipientScope(""); err == nil {
		t.Error("empty scope")
	}
}
