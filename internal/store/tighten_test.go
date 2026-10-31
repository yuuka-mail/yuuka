package store_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

func (e *sendEnv) act(tool string) store.Action {
	at := e.at()
	return store.Action{ID: ulid.New(at), Actor: e.op.PrincipalID, Tool: tool, At: at, Args: map[string]any{"via": "test"}}
}

func (e *sendEnv) setGrant(g store.Grant) {
	e.t.Helper()
	if g.Allowlist == nil {
		g.Allowlist = []string{}
	}
	if err := e.db.SetGrant(e.t.Context(), g, e.act("grant.set")); err != nil {
		e.t.Fatal(err)
	}
}

func widened(err error) []string {
	if w, ok := errors.AsType[*store.WideningError](err); ok {
		return w.Fields
	}
	return nil
}

func TestTightenGrant(t *testing.T) {
	t.Parallel()
	e := newSendEnv(t)
	ctx := t.Context()
	tier := func(t sendpolicy.Tier) *sendpolicy.Tier { return &t }
	scope := func(s sendpolicy.RecipientScope) *sendpolicy.RecipientScope { return &s }
	num := func(n int) *int { return &n }
	yes, no := new(true), new(false)

	// arisu on its own identity: auto, an allowlist, the agent's default
	// limits (10/hour, 50/day, 5 new/day) except its own 20 a day.
	e.setGrant(store.Grant{
		IdentityID: e.arisuID.ID, PrincipalID: e.arisu.PrincipalID, Policy: sendpolicy.Auto,
		RecipientScope: sendpolicy.ScopeAllowlist, Allowlist: []string{"a@example.net", "example.org"}, MaxPerDay: num(20),
	})
	tighten := func(tt store.GrantTightening) (store.Grant, error) {
		return e.db.TightenGrant(ctx, e.arisuID.ID, e.arisu.PrincipalID, tt, e.act("grant.tighten"))
	}
	for _, tc := range []struct {
		name string
		t    store.GrantTightening
		want []string // the widened columns; nil: applied
	}{
		{"tier up", store.GrantTightening{Policy: tier("auto")}, nil}, // equal is not wider
		{"scope any", store.GrantTightening{RecipientScope: scope("any")}, []string{"recipient_scope"}},
		{"allowlist to thread", store.GrantTightening{RecipientScope: scope("thread")}, []string{"recipient_scope"}},
		{"an allowlist entry added", store.GrantTightening{Allowlist: []string{"a@example.net", "example.org", "b@example.net"}}, []string{"recipient_allowlist"}},
		{"limit above the default", store.GrantTightening{MaxPerHour: num(11)}, []string{"max_per_hour"}},
		{"limit above its own", store.GrantTightening{MaxPerDay: num(21)}, []string{"max_per_day"}},
		{"new recipients above the default", store.GrantTightening{MaxNewRecipientsPerDay: num(6)}, []string{"max_new_recipients_per_day"}},
		{"a recall window below the default", store.GrantTightening{HoldSeconds: num(599)}, []string{"hold_seconds"}},
		{"no recall window", store.GrantTightening{HoldSeconds: num(0)}, []string{"hold_seconds"}},
		{"tighter tier, wider scope", store.GrantTightening{Policy: tier("draft"), RecipientScope: scope("any")}, []string{"recipient_scope"}},
		{"not suspended, already not", store.GrantTightening{Suspended: no}, nil},
	} {
		_, err := tighten(tc.t)
		if got := widened(err); !slices.Equal(got, tc.want) || tc.want == nil && err != nil {
			t.Errorf("%s: %v, want widening %v", tc.name, err, tc.want)
		}
	}
	if n := e.actions("grant.tighten"); n != 0 {
		t.Errorf("refused or empty tightenings recorded %d actions", n)
	}

	g, err := tighten(store.GrantTightening{
		Policy: tier("review"), Allowlist: []string{"EXAMPLE.org"}, MaxPerHour: num(10), MaxPerDay: num(3), Suspended: yes,
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := e.db.GrantFor(ctx, e.arisuID.ID, e.arisu.PrincipalID)
	if stored.Policy != sendpolicy.Review || stored.RecipientScope != sendpolicy.ScopeAllowlist ||
		!slices.Equal(stored.Allowlist, []string{"example.org"}) || *stored.MaxPerHour != 10 || *stored.MaxPerDay != 3 ||
		stored.MaxNewRecipientsPerDay != nil || stored.SuspendedAt == nil || g.Policy != stored.Policy {
		t.Errorf("tightened = %+v", stored)
	}
	if n := e.actions("grant.tighten"); n != 1 {
		t.Errorf("%d grant.tighten actions", n)
	}
	var actor, via string
	if err := e.pool.QueryRow(ctx, `SELECT actor, args->>'via' FROM actions WHERE tool = 'grant.tighten'`).Scan(&actor, &via); err != nil ||
		actor != e.op.PrincipalID || via != "test" {
		t.Errorf("recorded as %s via %s: %v", actor, via, err)
	}
	// Now suspended: lifting it, or going back up, is the host's.
	for _, tc := range []struct {
		t    store.GrantTightening
		want []string
	}{
		{store.GrantTightening{Suspended: no}, []string{"suspended"}},
		{store.GrantTightening{Policy: tier("auto")}, []string{"send_policy"}},
		{store.GrantTightening{Allowlist: []string{"a@example.net"}}, []string{"recipient_allowlist"}},
		{store.GrantTightening{MaxPerDay: num(4)}, []string{"max_per_day"}},
	} {
		if _, err := tighten(tc.t); !slices.Equal(widened(err), tc.want) {
			t.Errorf("%+v: %v, want %v", tc.t, err, tc.want)
		}
	}
	// Nothing to change: nothing recorded.
	if _, err := tighten(store.GrantTightening{Suspended: yes, Policy: tier("review")}); err != nil || e.actions("grant.tighten") != 1 {
		t.Errorf("no-op: %v, %d actions", err, e.actions("grant.tighten"))
	}
	if _, err := tighten(store.GrantTightening{MaxPerHour: num(-1)}); err == nil {
		t.Error("a negative limit")
	}
	if _, err := e.db.TightenGrant(ctx, e.star.ID, e.arisu.PrincipalID, store.GrantTightening{Policy: tier("none")}, e.act("grant.tighten")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("no row: %v", err)
	}

	// A human's own grant without limits: any number is tighter.
	e.setGrant(store.Grant{IdentityID: e.star.ID, PrincipalID: e.op.PrincipalID, Policy: sendpolicy.Auto, RecipientScope: sendpolicy.ScopeAny})
	if _, err := e.db.TightenGrant(ctx, e.star.ID, e.op.PrincipalID, store.GrantTightening{MaxPerHour: num(1000)}, e.act("grant.tighten")); err != nil {
		t.Errorf("a limit where there was none: %v", err)
	}

	// The owner cap: monitoring on the operator's alerts@ is draft at
	// most. Review and auto would apply as draft, so they widen nothing,
	// and the write refuses them as it refuses them from the host: the
	// cap holds in the one place a row is written, not only in Decide.
	e.setGrant(store.Grant{IdentityID: e.alertsID.ID, PrincipalID: e.monitor.PrincipalID, Policy: sendpolicy.Draft, RecipientScope: sendpolicy.ScopeAny})
	capTighten := func(tt store.GrantTightening) error {
		_, err := e.db.TightenGrant(ctx, e.alertsID.ID, e.monitor.PrincipalID, tt, e.act("grant.tighten"))
		return err
	}
	for _, tt := range []sendpolicy.Tier{"review", "auto"} {
		if err := capTighten(store.GrantTightening{Policy: tier(tt)}); !errors.Is(err, store.ErrOwnerCap) {
			t.Errorf("%s under the cap: %v", tt, err)
		}
	}
	// A row edited by hand to auto applies as draft; it is never written
	// back as it stands, but lowering it is a tightening like any other.
	if _, err := e.pool.Exec(ctx, `UPDATE identity_grants SET send_policy = 'auto' WHERE identity_id = $1`, e.alertsID.ID); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []store.GrantTightening{{Policy: tier("review")}, {Suspended: yes}} {
		if err := capTighten(tt); !errors.Is(err, store.ErrOwnerCap) {
			t.Errorf("%+v over a hand-edited auto: %v", tt, err)
		}
	}
	if err := capTighten(store.GrantTightening{Policy: tier("none")}); err != nil {
		t.Errorf("none under the cap: %v", err)
	}
	if g, _ := e.db.GrantFor(ctx, e.alertsID.ID, e.monitor.PrincipalID); g.Policy != sendpolicy.None {
		t.Errorf("after none: %s", g.Policy)
	}
	if err := capTighten(store.GrantTightening{Policy: tier("draft")}); !slices.Equal(widened(err), []string{"send_policy"}) {
		t.Errorf("none back to draft: %v", err)
	}
}

func TestRemoveGrantIfTighter(t *testing.T) {
	t.Parallel()
	e := newSendEnv(t)
	ctx := t.Context()
	remove := func(identity store.Identity, principal string) (sendpolicy.Grant, error) {
		return e.db.RemoveGrantIfTighter(ctx, identity.ID, principal, e.act("grant.remove"))
	}
	arisu, monitor := e.arisu.PrincipalID, e.monitor.PrincipalID
	num := func(n int) *int { return &n }

	for _, tc := range []struct {
		name     string
		identity store.Identity
		g        store.Grant
		want     []string
		def      sendpolicy.Tier
	}{
		// arisu's own identity defaults to review + thread.
		{"own identity, draft row", e.arisuID, store.Grant{PrincipalID: arisu, Policy: "draft", RecipientScope: "thread"}, []string{"send_policy"}, ""},
		{"own identity, review row with a longer recall window", e.arisuID, store.Grant{PrincipalID: arisu, Policy: "review", RecipientScope: "thread", HoldSeconds: num(1200)}, []string{"hold_seconds"}, ""},
		{"own identity, review row with a shorter recall window", e.arisuID, store.Grant{PrincipalID: arisu, Policy: "review", RecipientScope: "thread", HoldSeconds: num(60)}, nil, "review"},
		{"own identity, auto row with a long window that never acts", e.arisuID, store.Grant{PrincipalID: arisu, Policy: "auto", RecipientScope: "any", HoldSeconds: num(3600)}, nil, "review"},
		{"own identity, suspended auto row", e.arisuID, store.Grant{PrincipalID: arisu, Policy: "auto", RecipientScope: "any", SuspendedAt: &base}, []string{"suspended"}, ""},
		{"own identity, auto row", e.arisuID, store.Grant{PrincipalID: arisu, Policy: "auto", RecipientScope: "any"}, nil, "review"},
		// On the operator's identity arisu defaults to draft.
		{"operator's identity, none row", e.star, store.Grant{PrincipalID: arisu, Policy: "none", RecipientScope: "thread"}, []string{"send_policy"}, ""},
		{"operator's identity, suspended draft row", e.star, store.Grant{PrincipalID: arisu, Policy: "draft", RecipientScope: "any", SuspendedAt: &base}, nil, "draft"},
		// A service on the operator's identity defaults to none.
		{"service on the operator's identity", e.alertsID, store.Grant{PrincipalID: monitor, Policy: "draft", RecipientScope: "any"}, nil, "none"},
	} {
		tc.g.IdentityID = tc.identity.ID
		e.setGrant(tc.g)
		def, err := remove(tc.identity, tc.g.PrincipalID)
		if got := widened(err); !slices.Equal(got, tc.want) || tc.want == nil && (err != nil || def.Policy != tc.def) {
			t.Errorf("%s: %v (default %s), want widening %v", tc.name, err, def.Policy, tc.want)
		}
		_, err = e.db.GrantFor(ctx, tc.identity.ID, tc.g.PrincipalID)
		if gone := errors.Is(err, store.ErrNotFound); gone != (tc.want == nil) {
			t.Errorf("%s: removed %v", tc.name, gone)
		}
		if !errors.Is(err, store.ErrNotFound) {
			if err := e.db.RemoveGrant(ctx, tc.identity.ID, tc.g.PrincipalID, e.act("grant.remove")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := remove(e.arisuID, arisu); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("no row: %v", err)
	}

	// Nothing sends as a disabled identity, with or without its grants.
	e.setGrant(store.Grant{IdentityID: e.arisuID.ID, PrincipalID: arisu, Policy: "draft", RecipientScope: "thread"})
	if err := e.db.DisableIdentity(ctx, e.arisuID.ID, e.act("identity.disable")); err != nil {
		t.Fatal(err)
	}
	if _, err := remove(e.arisuID, arisu); err != nil {
		t.Errorf("a grant on a disabled identity: %v", err)
	}
}

func TestDisableIdentityIfTighter(t *testing.T) {
	t.Parallel()
	e := newSendEnv(t)
	ctx := t.Context()
	disable := func(i store.Identity) error {
		return e.db.DisableIdentityIfTighter(ctx, i.ID, e.act("identity.disable"))
	}
	// *@example.com would send as arisu@ once arisu@ is gone.
	if err := disable(e.arisuID); !errors.Is(err, store.ErrPatternTakesOver) {
		t.Errorf("an address under an enabled pattern: %v", err)
	}
	all, _ := e.db.Identities(ctx, false)
	if p, ok := e.arisuID.TakenOverBy(all); !ok || p.ID != e.star.ID {
		t.Errorf("TakenOverBy = %v, %v", p.Address(), ok)
	}
	if _, ok := e.star.TakenOverBy(all); ok {
		t.Error("a pattern taken over")
	}
	if err := disable(e.star); err != nil {
		t.Fatal(err)
	}
	if err := disable(e.arisuID); err != nil {
		t.Errorf("with the pattern gone: %v", err)
	}
	if err := disable(e.arisuID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("twice: %v", err)
	}
	if n := e.actions("identity.disable"); n != 2 {
		t.Errorf("%d identity.disable actions", n)
	}
}
