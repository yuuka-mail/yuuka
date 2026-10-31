package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authn"
	"github.com/yuuka-mail/yuuka/internal/leak"
	"github.com/yuuka-mail/yuuka/internal/openapi"
	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// Management actions (§13.10). Decisions prompted by mail (claiming a
// prefix, its expected domains) are checked by scope like any write.
// Settings that change who may do what are served only to a person's
// session holding admin (route.authority), and only to read them and to
// tighten them: every write here can only make the system more
// conservative, which the store checks against what applies now. Nothing
// here issues, raises or adds; that stays on the host.

func (a *API) claimPrefix(ctx context.Context, v *auth.Viewer, p PrefixDecisionParams) (PrefixDecision, error) {
	return a.decidePrefix(ctx, v, p.Address, store.PrefixClaimed, "prefix.claim")
}

func (a *API) unclaimPrefix(ctx context.Context, v *auth.Viewer, p PrefixDecisionParams) (PrefixDecision, error) {
	return a.decidePrefix(ctx, v, p.Address, store.PrefixDiscovered, "prefix.unclaim")
}

var prefixOutcomes = map[store.PrefixChange]PrefixOutcome{
	store.PrefixUnchanged: "unchanged", store.PrefixUpdated: "updated", store.PrefixCreated: "created", store.PrefixRemoved: "removed",
}

func (a *API) decidePrefix(ctx context.Context, v *auth.Viewer, addr PrefixAddress, state int, tool string) (PrefixDecision, error) {
	got, err := a.db.SetPrefixState(ctx, addr.Zone, addr.Prefix, state, action(v, tool, nil))
	if err != nil {
		return PrefixDecision{}, err
	}
	out := PrefixDecision{Outcome: prefixOutcomes[got]}
	out.Prefix, err = a.prefixNow(ctx, v, addr)
	return out, err
}

// prefixNow is the prefix as v now sees its mail, nil when it is not in
// the registry.
func (a *API) prefixNow(ctx context.Context, v *auth.Viewer, addr PrefixAddress) (*Prefix, error) {
	sum, err := a.db.PrefixStats(ctx, v, store.PrefixQuery{Zone: addr.Zone, Prefix: addr.Prefix})
	if err != nil || len(sum.Prefixes) != 1 {
		return nil, err
	}
	p := prefixOut(sum.Prefixes[0])
	return &p, nil
}

func (a *API) expectDomains(ctx context.Context, v *auth.Viewer, p ExpectParams) (ExpectResult, error) {
	stored, changed, err := a.db.SetPrefixExpected(ctx, p.Address.Zone, p.Address.Prefix, p.Body.Domains, action(v, "prefix.expect", nil))
	switch {
	case errors.Is(err, store.ErrInvalidDomain):
		return ExpectResult{}, openapi.Errorf(http.StatusBadRequest, openapi.CodeInvalidBody, "%v", errors.Unwrap(err))
	case errors.Is(err, store.ErrNotClaimed):
		return ExpectResult{}, openapi.Errorf(http.StatusConflict, openapi.CodeConflict,
			"%s is not claimed; leak detection ignores unclaimed prefixes, so claim it first", p.Address.Prefix+"@"+p.Address.Zone)
	case err != nil:
		return ExpectResult{}, err
	}
	out := ExpectResult{Changed: changed, Expected: nonNil(stored), CountedAs: []CountedAs{}}
	for _, d := range p.Body.Domains {
		if org := leak.OrgDomain(d); org != strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d), ".")) {
			out.CountedAs = append(out.CountedAs, CountedAs{Domain: d, As: org})
		}
	}
	out.Prefix, err = a.prefixNow(ctx, v, p.Address)
	return out, err
}

// authorityDenied refuses everyone but a person's session (§13.10). The
// handler of a route marked authority() runs only after it, so a refused
// request never reaches the store.
func authorityDenied(v *auth.Viewer) error {
	if v.HumanSession() {
		return nil
	}
	return openapi.Errorf(http.StatusForbidden, openapi.CodeForbidden,
		"only a person signed in to the WebUI can change who may do what; a token cannot, whatever its scopes")
}

// widening refuses a change that would let more through, saying why and
// naming the host command that makes it.
func widening(cmd, format string, args ...any) error {
	return openapi.Errorf(http.StatusForbidden, openapi.CodeWideningRequiresCLI,
		"%s The WebUI only tightens; on the host: %s", fmt.Sprintf(format, args...), cmd)
}

func (a *API) credentials(ctx context.Context, _ *auth.Viewer, _ struct{}) (CredentialList, error) {
	cs, err := a.db.Tokens(ctx, "")
	if err != nil {
		return CredentialList{}, err
	}
	out := CredentialList{Credentials: make([]ManagedCredential, len(cs))}
	for i, c := range cs {
		scopes, _ := auth.ParseScopes(c.Scopes) // the column's CHECK lists the same
		out.Credentials[i] = ManagedCredential{
			ID: c.ID, Kind: CredentialKind(c.Kind), Label: c.Label, Scopes: nonNilScopes(scopes),
			Principal:         Actor{ID: c.PrincipalID, Kind: c.PrincipalKind, Name: c.PrincipalName},
			PrincipalDisabled: c.PrincipalDisabled,
			CreatedAt:         c.CreatedAt, ExpiresAt: c.ExpiresAt, LastUsedAt: c.LastUsedAt, RevokedAt: c.RevokedAt,
		}
	}
	return out, nil
}

func nonNilScopes(s []auth.Scope) []auth.Scope {
	if s == nil {
		return []auth.Scope{}
	}
	return s
}

func (a *API) revokeCredential(ctx context.Context, v *auth.Viewer, p IDParams) (struct{}, error) {
	err := authn.RevokeToken(ctx, a.db, p.ID, action(v, "auth.credential.revoke", nil))
	if errors.Is(err, authn.ErrNotToken) || errors.Is(err, store.ErrNotFound) {
		return struct{}{}, openapi.Errorf(http.StatusNotFound, openapi.CodeNotFound, "no unrevoked token or SMTP password %s", p.ID)
	}
	return struct{}{}, err
}

func (a *API) adminIdentities(ctx context.Context, _ *auth.Viewer, _ struct{}) (IdentityAdminList, error) {
	all, err := a.db.Identities(ctx, true)
	if err != nil {
		return IdentityAdminList{}, err
	}
	var enabled []store.Identity
	for _, i := range all {
		if i.DisabledAt == nil {
			enabled = append(enabled, i)
		}
	}
	out := IdentityAdminList{Identities: make([]ManagedIdentity, len(all))}
	for k, i := range all {
		m := ManagedIdentity{
			ID: i.ID, Address: i.Address(), DisplayName: i.DisplayName, Transport: i.Transport,
			Owner:     Actor{ID: i.OwnerID, Kind: i.OwnerKind, Name: i.OwnerName},
			CreatedAt: i.CreatedAt, DisabledAt: i.DisabledAt, Host: "yuuka identity rm " + shellWord(i.Address()),
		}
		if i.DisabledAt == nil {
			if p, ok := i.TakenOverBy(enabled); ok {
				m.TakenOverBy = p.Address()
			}
		}
		out.Identities[k] = m
	}
	return out, nil
}

func (a *API) disableIdentity(ctx context.Context, v *auth.Viewer, p IDParams) (struct{}, error) {
	err := a.db.DisableIdentityIfTighter(ctx, p.ID, action(v, "identity.disable", map[string]any{"identity_id": p.ID}))
	if errors.Is(err, store.ErrPatternTakesOver) {
		i, ierr := a.db.IdentityByID(ctx, p.ID)
		if ierr != nil {
			return struct{}{}, ierr
		}
		return struct{}{}, widening("yuuka identity rm "+shellWord(i.Address()),
			"Disabling %s hands its address to *@%s and its grants, which may let more through.", i.Address(), i.Zone)
	}
	return struct{}{}, err
}

func (a *API) adminGrants(ctx context.Context, _ *auth.Viewer, _ struct{}) (GrantAdminList, error) {
	gs, err := a.db.Grants(ctx, "", "")
	if err != nil {
		return GrantAdminList{}, err
	}
	out := GrantAdminList{Grants: make([]ManagedGrant, len(gs))}
	for i, g := range gs {
		out.Grants[i] = managedGrant(g)
	}
	return out, nil
}

func managedGrant(g store.Grant) ManagedGrant {
	return ManagedGrant{
		Identity: GrantIdentity{
			ID: g.IdentityID, Address: g.IdentityAddress, Disabled: g.IdentityDisabled,
			Owner: Actor{ID: g.IdentityOwnerID, Kind: g.IdentityOwnerKind, Name: g.IdentityOwnerName},
		},
		Principal: Actor{ID: g.PrincipalID, Kind: g.PrincipalKind, Name: g.PrincipalName},
		Stored: StoredGrant{
			SendPolicy: Tier(g.Policy), RecipientScope: RecipientScope(g.RecipientScope), Allowlist: nonNil(g.Allowlist),
			Limits:      Limits{MaxPerHour: g.MaxPerHour, MaxPerDay: g.MaxPerDay, MaxNewRecipientsPerDay: g.MaxNewRecipientsPerDay},
			HoldSeconds: g.HoldSeconds, SuspendedAt: g.SuspendedAt,
		},
		Applied:     appliedGrant(g.Applied(), g.Hold()),
		OwnerCapped: sendpolicy.OwnerCapped(g.PrincipalID, sendpolicy.Identity{OwnerID: g.IdentityOwnerID, OwnerKind: g.IdentityOwnerKind}),
		// Without the row the column default's recall window applies.
		Default:       appliedGrant(g.Default(), store.Grant{}.Hold()),
		RemovalWidens: nonNil(g.RemovalWidenings()),
		CreatedAt:     g.CreatedAt,
		Host:          grantCommand("set", g),
	}
}

func appliedGrant(g sendpolicy.Grant, hold int) AppliedGrant {
	return AppliedGrant{
		SendPolicy: Tier(g.Policy), RecipientScope: RecipientScope(g.RecipientScope), Allowlist: nonNil(g.Allowlist),
		Limits: Limits{
			MaxPerHour: g.Limits.MaxPerHour, MaxPerDay: g.Limits.MaxPerDay, MaxNewRecipientsPerDay: g.Limits.MaxNewRecipientsPerDay,
		},
		HoldSeconds: hold, Suspended: g.Suspended,
	}
}

func (a *API) tightenGrant(ctx context.Context, v *auth.Viewer, p TightenParams) (ManagedGrant, error) {
	b := p.Body
	t := store.GrantTightening{
		Allowlist:  b.Allowlist,
		MaxPerHour: b.MaxPerHour, MaxPerDay: b.MaxPerDay, MaxNewRecipientsPerDay: b.MaxNewRecipientsPerDay,
		Suspended: b.Suspended,
	}
	if b.SendPolicy != nil {
		t.Policy = new(sendpolicy.Tier(*b.SendPolicy))
	}
	if b.RecipientScope != nil {
		t.RecipientScope = new(sendpolicy.RecipientScope(*b.RecipientScope))
	}
	g, err := a.db.TightenGrant(ctx, p.Identity, p.Principal, t, action(v, "grant.tighten", nil))
	if w, ok := errors.AsType[*store.WideningError](err); ok {
		return ManagedGrant{}, widening(grantCommand("set", w.Current)+hostFlags(w.Fields, b),
			"This lets more through than the grant does now in %s.", strings.Join(w.Fields, ", "))
	}
	if errors.Is(err, store.ErrOwnerCap) {
		return ManagedGrant{}, openapi.Errorf(http.StatusConflict, openapi.CodeConflict,
			"the identity belongs to a human who is not the grantee: draft at most, here and on the host (§8.9)")
	}
	if err != nil {
		return ManagedGrant{}, err
	}
	return managedGrant(g), nil
}

// removeGrant answers 204 like every deletion; what applies afterwards is
// the default the list showed (ManagedGrant.Default).
func (a *API) removeGrant(ctx context.Context, v *auth.Viewer, p GrantParams) (struct{}, error) {
	_, err := a.db.RemoveGrantIfTighter(ctx, p.Identity, p.Principal, action(v, "grant.remove", nil))
	if w, ok := errors.AsType[*store.WideningError](err); ok {
		return struct{}{}, widening(grantCommand("rm", w.Current),
			"Without the grant %s:%s gets the default, which lets more through in %s.",
			w.Current.PrincipalKind, w.Current.PrincipalName, strings.Join(w.Fields, ", "))
	}
	return struct{}{}, err
}

// grantCommand is `yuuka grant <verb>` naming g's row.
func grantCommand(verb string, g store.Grant) string {
	return "yuuka grant " + verb + " --identity " + shellWord(g.IdentityAddress) + " --principal " + shellWord(g.PrincipalKind+":"+g.PrincipalName)
}

// hostFlags are the `yuuka grant set` flags for the widened fields, with
// the values asked for.
func hostFlags(fields []string, b TightenBody) string {
	var out []string
	limit := func(flag string, n *int) {
		if n != nil {
			out = append(out, flag+" "+strconv.Itoa(*n))
		}
	}
	for _, f := range fields {
		switch f {
		case sendpolicy.FieldPolicy:
			out = append(out, "--policy "+string(*b.SendPolicy))
		case sendpolicy.FieldRecipientScope:
			out = append(out, "--recipients "+string(*b.RecipientScope))
		case sendpolicy.FieldAllowlist:
			out = append(out, "--allow "+shellWord(strings.Join(b.Allowlist, ",")))
		case sendpolicy.FieldMaxPerHour:
			limit("--max-per-hour", b.MaxPerHour)
		case sendpolicy.FieldMaxPerDay:
			limit("--max-per-day", b.MaxPerDay)
		case sendpolicy.FieldMaxNewRecipientsPerDay:
			limit("--max-new-recipients-per-day", b.MaxNewRecipientsPerDay)
		case sendpolicy.FieldSuspended:
			out = append(out, "--resume")
		}
	}
	if len(out) == 0 {
		return ""
	}
	return " " + strings.Join(out, " ")
}

// shellWord quotes s for a POSIX shell when it needs it: *@zone does.
func shellWord(s string) string {
	plain := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@._+:,-/=", r)
	}
	if s != "" && !strings.ContainsFunc(s, func(r rune) bool { return !plain(r) }) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
