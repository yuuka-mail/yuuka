package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authn"
	"github.com/yuuka-mail/yuuka/internal/leak"
	"github.com/yuuka-mail/yuuka/internal/openapi"
	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Management actions (§13.10). Decisions prompted by mail (claiming a
// prefix, its expected domains) are checked by scope like any write.
// Settings that change who may do what are served only to a person's
// session holding admin (route.authority). Such a session may read them
// and tighten them in one step, which the store checks against what
// applies now. Issuing, adding and raising — anything that lets someone
// do more — also takes a fresh passkey assertion (route.widening, or
// Viewer.Elevated where a write may go either way), and tells the
// operator. Every write goes through the store functions the host CLI
// calls, so the checks are written once.

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

// widening refuses a tightening that would let more through, saying why
// and naming the host command that makes it.
func widening(cmd, format string, args ...any) error {
	return openapi.Errorf(http.StatusForbidden, openapi.CodeWideningRequiresCLI,
		"%s This only tightens; widen with PUT /api/v1/admin/grants from an elevated session, or on the host: %s",
		fmt.Sprintf(format, args...), cmd)
}

// elevationRequired refuses a widening from a session that has not
// asserted its passkey in the last few minutes. With a host command, the
// detail ends with it as widening's does.
func elevationRequired(why string, cmd ...string) error {
	detail := why + " It takes asserting your passkey again (/auth/passkey/elevate), then retrying"
	if len(cmd) > 0 {
		return openapi.Errorf(http.StatusForbidden, openapi.CodeElevationRequired, "%s; or on the host: %s", detail, cmd[0])
	}
	return openapi.Errorf(http.StatusForbidden, openapi.CodeElevationRequired, "%s.", detail)
}

// invalid is a 400 naming what in the body was wrong.
func invalid(format string, args ...any) error {
	return openapi.Errorf(http.StatusBadRequest, openapi.CodeInvalidBody, format, args...)
}

// grantee is the principal named kind:name in a body (store.Grantee): an
// agent or service is created on first use.
func (a *API) grantee(ctx context.Context, v *auth.Viewer, s string) (store.Principal, error) {
	kind, name, err := store.ParsePrincipal(s)
	if err != nil {
		return store.Principal{}, invalid("%v", err)
	}
	p, _, err := a.db.Grantee(ctx, kind, name, action(v, "principal.create", nil))
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidPrincipal) || errors.Is(err, store.ErrPrincipalDisabled) {
		return p, invalid("%v", err)
	}
	return p, err
}

// widened tells the operator that v let someone do more (§13.10), as a
// new passkey is told: the alert is what makes a stolen elevated session
// visible. Failing to tell is logged and changes nothing.
func (a *API) widened(ctx context.Context, v *auth.Viewer, what string) {
	if a.alert == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	text := fmt.Sprintf("yuuka: %s %s in the WebUI (session %s). If this was not you, someone holds that session and one of your passkeys: undo it in the settings.", v, what, v.CredentialID)
	if err := a.alert.Notify(ctx, text); err != nil {
		a.log.Error("api: alert failed", "err", err)
	}
}

func (a *API) credentials(ctx context.Context, _ *auth.Viewer, _ struct{}) (CredentialList, error) {
	cs, err := a.db.Tokens(ctx, "")
	if err != nil {
		return CredentialList{}, err
	}
	out := CredentialList{Credentials: make([]ManagedCredential, len(cs))}
	for i, c := range cs {
		out.Credentials[i] = managedCredential(c)
	}
	return out, nil
}

func managedCredential(c store.CredentialPrincipal) ManagedCredential {
	scopes, _ := auth.ParseScopes(c.Scopes) // the column's CHECK lists the same
	return ManagedCredential{
		ID: c.ID, Kind: CredentialKind(c.Kind), Label: c.Label, Scopes: nonNilScopes(scopes),
		Principal:         Actor{ID: c.PrincipalID, Kind: c.PrincipalKind, Name: c.PrincipalName},
		PrincipalDisabled: c.PrincipalDisabled,
		CreatedAt:         c.CreatedAt, ExpiresAt: c.ExpiresAt, LastUsedAt: c.LastUsedAt, RevokedAt: c.RevokedAt,
	}
}

// issueCredential issues as `yuuka token issue` does (authn.IssueToken),
// in the person's name rather than the system's.
func (a *API) issueCredential(ctx context.Context, v *auth.Viewer, p IssueParams) (IssuedCredential, error) {
	b := p.Body
	kind, _, err := store.ParsePrincipal(b.Principal)
	if err != nil {
		return IssuedCredential{}, invalid("%v", err)
	}
	if kind != "agent" && kind != "service" {
		return IssuedCredential{}, invalid("%s: tokens and SMTP passwords are for agents and services; a person signs in with a passkey", b.Principal)
	}
	credKind, prefix, scopes := auth.KindToken, auth.PrefixToken, b.Scopes
	if b.Kind == "smtp_password" {
		credKind, prefix = auth.KindSMTPPassword, auth.PrefixSMTP
		if len(scopes) == 0 {
			scopes = []auth.Scope{auth.SubmissionSend}
		}
		if len(scopes) != 1 || scopes[0] != auth.SubmissionSend {
			return IssuedCredential{}, invalid("an SMTP password holds %s and nothing else", auth.SubmissionSend)
		}
	}
	if len(scopes) == 0 {
		return IssuedCredential{}, invalid("name every scope the token holds; there is no default")
	}
	// In the order of §13.4, without repeats, as the CLI stores them.
	var ordered []auth.Scope
	for _, sc := range auth.AllScopes {
		if slices.Contains(scopes, sc) {
			ordered = append(ordered, sc)
		}
	}
	pr, err := a.grantee(ctx, v, b.Principal)
	if err != nil {
		return IssuedCredential{}, err
	}
	tok, err := authn.IssueToken(ctx, a.db, time.Now(), authn.TokenRequest{
		Kind: credKind, PrincipalID: pr.ID, Label: b.Label, Scopes: ordered, TTL: time.Duration(b.TTLSeconds) * time.Second,
		IssuedBy: v.PrincipalID, IssuedWith: v.CredentialID, AuditArgs: map[string]any{"via": "api"},
	})
	if err != nil {
		return IssuedCredential{}, err
	}
	id, _, _ := auth.ParseSecret(prefix, tok)
	c, err := a.db.CredentialForAuth(ctx, id)
	if err != nil {
		return IssuedCredential{}, err
	}
	out := IssuedCredential{Credential: managedCredential(c), Secret: tok}
	what := "issued a token for " + b.Principal + " with " + strings.Join(auth.Strings(ordered), ",")
	if credKind == auth.KindSMTPPassword {
		out.SMTPUsername = id
		what = "issued an SMTP password for " + b.Principal
	}
	a.widened(ctx, v, what)
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
	accts, err := a.db.Accounts(ctx)
	if err != nil {
		return IdentityAdminList{}, err
	}
	trs, err := a.transportNames(ctx)
	if err != nil {
		return IdentityAdminList{}, err
	}
	var enabled []store.Identity
	for _, i := range all {
		if i.DisabledAt == nil {
			enabled = append(enabled, i)
		}
	}
	out := IdentityAdminList{
		Identities: make([]ManagedIdentity, len(all)), Transports: trs, Accounts: make([]string, len(accts)),
	}
	for k, acct := range accts {
		out.Accounts[k] = acct.Name
	}
	for k, i := range all {
		m := ManagedIdentity{
			ID: i.ID, Address: i.Address(), DisplayName: i.DisplayName, Transport: i.Transport, Transports: nonNil(i.Transports),
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

// transportNames are the names of the enabled transports, what a new
// identity may name: configured ones and placeholders, which serve makes
// for every name in YUUKA_TRANSPORTS (§17.7).
func (a *API) transportNames(ctx context.Context) ([]string, error) {
	trs, err := a.db.Transports(ctx, false)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(trs))
	for k, t := range trs {
		names[k] = t.Name
	}
	return names, nil
}

// addIdentity adds as `yuuka identity add` does, naming one transport.
func (a *API) addIdentity(ctx context.Context, v *auth.Viewer, p IdentityParams) (ManagedIdentity, error) {
	b := p.Body
	zone, prefix, err := store.SplitIdentityAddress(b.Address)
	if err != nil {
		return ManagedIdentity{}, invalid("%v", err)
	}
	trs, err := a.transportNames(ctx)
	if err != nil {
		return ManagedIdentity{}, err
	}
	if len(trs) == 0 {
		return ManagedIdentity{}, openapi.Errorf(http.StatusConflict, openapi.CodeConflict,
			"no transport exists (YUUKA_TRANSPORTS), so an identity would have nothing to send with")
	}
	if !slices.Contains(trs, b.Transport) {
		return ManagedIdentity{}, invalid("transport %q: want one of %s", b.Transport, strings.Join(trs, ", "))
	}
	accts, err := a.db.Accounts(ctx)
	if err != nil {
		return ManagedIdentity{}, err
	}
	var acct store.Account
	for _, x := range accts {
		if x.Name == b.Account || b.Account == "" && len(accts) == 1 {
			acct = x
		}
	}
	switch {
	case acct.ID != "":
	case len(accts) == 0:
		return ManagedIdentity{}, openapi.Errorf(http.StatusConflict, openapi.CodeConflict,
			"no account exists yet; it is created when yuuka serve first ingests mail")
	case b.Account == "":
		return ManagedIdentity{}, invalid("%d accounts exist; name one", len(accts))
	default:
		return ManagedIdentity{}, invalid("account %q: no such account", b.Account)
	}
	owner, err := a.grantee(ctx, v, b.Owner)
	if err != nil {
		return ManagedIdentity{}, err
	}
	now := time.Now()
	i := store.Identity{
		ID: ulid.New(now), AccountID: acct.ID, Zone: zone, Prefix: prefix, DisplayName: b.DisplayName,
		Signature: b.Signature, PlainStripMarkup: b.PlainStripMarkup, Transport: b.Transport, OwnerID: owner.ID, CreatedAt: now,
	}
	err = a.db.AddIdentity(ctx, i, action(v, "identity.add", map[string]any{
		"identity_id": i.ID, "address": i.Address(), "account": acct.Name, "owner": owner.Kind + ":" + owner.Name, "transport": i.Transport,
	}))
	if errors.Is(err, store.ErrDuplicate) {
		return ManagedIdentity{}, openapi.Errorf(http.StatusConflict, openapi.CodeConflict, "%s is already an identity", i.Address())
	}
	if errors.Is(err, store.ErrNoTransport) {
		// Removed between the check above and the insert.
		return ManagedIdentity{}, invalid("transport %q: %v", b.Transport, err)
	}
	if err != nil {
		return ManagedIdentity{}, err
	}
	a.widened(ctx, v, "added the identity "+i.Address()+", owned by "+owner.Kind+":"+owner.Name)
	list, err := a.adminIdentities(ctx, v, struct{}{})
	if err != nil {
		return ManagedIdentity{}, err
	}
	for _, m := range list.Identities {
		if m.ID == i.ID {
			return m, nil
		}
	}
	return ManagedIdentity{}, fmt.Errorf("identity %s vanished after it was added", i.ID)
}

// disableIdentity disables in one step when that only tightens. Where
// the zone's pattern would take the address over, it takes an elevated
// session, as `yuuka identity rm` on the host.
func (a *API) disableIdentity(ctx context.Context, v *auth.Viewer, p IDParams) (struct{}, error) {
	act := action(v, "identity.disable", map[string]any{"identity_id": p.ID})
	err := a.db.DisableIdentityIfTighter(ctx, p.ID, act)
	if !errors.Is(err, store.ErrPatternTakesOver) {
		return struct{}{}, err
	}
	i, err := a.db.IdentityByID(ctx, p.ID)
	if err != nil {
		return struct{}{}, err
	}
	if !v.Elevated(time.Now()) {
		return struct{}{}, elevationRequired(fmt.Sprintf(
			"Disabling %s hands its address to *@%s and its grants, which may let more through.", i.Address(), i.Zone),
			"yuuka identity rm "+shellWord(i.Address()))
	}
	if err := a.db.DisableIdentity(ctx, p.ID, act); err != nil {
		return struct{}{}, err
	}
	a.widened(ctx, v, "disabled "+i.Address()+", whose mail *@"+i.Zone+" now sends with its own owner and grants")
	return struct{}{}, nil
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
		Applied:       appliedGrant(g.Applied()),
		OwnerCapped:   sendpolicy.OwnerCapped(g.PrincipalID, sendpolicy.Identity{OwnerID: g.IdentityOwnerID, OwnerKind: g.IdentityOwnerKind}),
		Default:       appliedGrant(g.Default()),
		RemovalWidens: nonNil(g.RemovalWidenings()),
		CreatedAt:     g.CreatedAt,
		Host:          grantCommand("set", g),
	}
}

func appliedGrant(g sendpolicy.Grant) AppliedGrant {
	return AppliedGrant{
		SendPolicy: Tier(g.Policy), RecipientScope: RecipientScope(g.RecipientScope), Allowlist: nonNil(g.Allowlist),
		Limits: Limits{
			MaxPerHour: g.Limits.MaxPerHour, MaxPerDay: g.Limits.MaxPerDay, MaxNewRecipientsPerDay: g.Limits.MaxNewRecipientsPerDay,
		},
		HoldSeconds: sendpolicy.EffectiveHoldSeconds(g.HoldSeconds), Suspended: g.Suspended,
	}
}

func (a *API) tightenGrant(ctx context.Context, v *auth.Viewer, p TightenParams) (ManagedGrant, error) {
	b := p.Body
	t := store.GrantTightening{
		Allowlist:  b.Allowlist,
		MaxPerHour: b.MaxPerHour, MaxPerDay: b.MaxPerDay, MaxNewRecipientsPerDay: b.MaxNewRecipientsPerDay,
		HoldSeconds: b.HoldSeconds, Suspended: b.Suspended,
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

// setGrant writes a whole grant row as `yuuka grant set` does
// (store.SetGrant), whether it widens or not: the route takes an elevated
// session either way.
func (a *API) setGrant(ctx context.Context, v *auth.Viewer, p GrantSetParams) (ManagedGrant, error) {
	b := p.Body
	ident, err := a.db.IdentityByID(ctx, b.Identity)
	if err != nil {
		return ManagedGrant{}, err
	}
	if ident.DisabledAt != nil {
		return ManagedGrant{}, openapi.Errorf(http.StatusConflict, openapi.CodeConflict, "%s is disabled; nothing sends as it", ident.Address())
	}
	// Checked before the principal: a bad body creates no agent.
	allow, err := sendpolicy.ParseAllowlist(b.Allowlist)
	if err != nil {
		return ManagedGrant{}, invalid("%v", err)
	}
	pr, err := a.grantee(ctx, v, b.Principal)
	if err != nil {
		return ManagedGrant{}, err
	}
	before, err := a.db.GrantFor(ctx, ident.ID, pr.ID)
	existed := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return ManagedGrant{}, err
	}
	g := store.Grant{
		IdentityID: ident.ID, PrincipalID: pr.ID, Policy: sendpolicy.Tier(b.SendPolicy), RecipientScope: sendpolicy.RecipientScope(b.RecipientScope),
		Allowlist: allow, MaxPerHour: b.MaxPerHour, MaxPerDay: b.MaxPerDay, MaxNewRecipientsPerDay: b.MaxNewRecipientsPerDay,
		HoldSeconds: b.HoldSeconds,
	}
	if b.Suspended {
		// A suspension keeps the moment it began.
		g.SuspendedAt = before.SuspendedAt
		if g.SuspendedAt == nil {
			now := time.Now()
			g.SuspendedAt = &now
		}
	}
	err = a.db.SetGrant(ctx, g, action(v, "grant.set", map[string]any{
		"identity_id": ident.ID, "identity": ident.Address(), "principal_id": pr.ID, "principal": pr.Kind + ":" + pr.Name,
		"policy": g.Policy, "recipient_scope": g.RecipientScope, "allowlist": g.Allowlist,
		"max_per_hour": g.MaxPerHour, "max_per_day": g.MaxPerDay, "max_new_recipients_per_day": g.MaxNewRecipientsPerDay,
		"hold_seconds": g.HoldSeconds, "suspended": g.SuspendedAt != nil,
	}))
	if errors.Is(err, store.ErrOwnerCap) {
		return ManagedGrant{}, openapi.Errorf(http.StatusConflict, openapi.CodeConflict,
			"%s belongs to %s:%s; %s:%s can be granted draft at most, here and on the host (§8.9)",
			ident.Address(), ident.OwnerKind, ident.OwnerName, pr.Kind, pr.Name)
	}
	if err != nil {
		return ManagedGrant{}, err
	}
	after, err := a.db.GrantFor(ctx, ident.ID, pr.ID)
	if err != nil {
		return ManagedGrant{}, err
	}
	from := after.Default()
	if existed {
		from = before.Applied()
	}
	if fields := sendpolicy.Widenings(from, after.Applied()); len(fields) > 0 {
		a.widened(ctx, v, fmt.Sprintf("widened the grant of %s:%s on %s in %s", pr.Kind, pr.Name, ident.Address(), strings.Join(fields, ", ")))
	}
	return managedGrant(after), nil
}

// removeGrant answers 204 like every deletion; what applies afterwards is
// the default the list showed (ManagedGrant.Default). Where that default
// lets more through, it takes an elevated session.
func (a *API) removeGrant(ctx context.Context, v *auth.Viewer, p GrantParams) (struct{}, error) {
	act := action(v, "grant.remove", nil)
	_, err := a.db.RemoveGrantIfTighter(ctx, p.Identity, p.Principal, act)
	w, ok := errors.AsType[*store.WideningError](err)
	if !ok {
		return struct{}{}, err
	}
	why := fmt.Sprintf("Without the grant %s:%s gets the default, which lets more through in %s.",
		w.Current.PrincipalKind, w.Current.PrincipalName, strings.Join(w.Fields, ", "))
	if !v.Elevated(time.Now()) {
		return struct{}{}, elevationRequired(why, grantCommand("rm", w.Current))
	}
	if err := a.db.RemoveGrant(ctx, p.Identity, p.Principal, act); err != nil {
		return struct{}{}, err
	}
	a.widened(ctx, v, fmt.Sprintf("removed the grant of %s:%s on %s, whose default lets more through in %s",
		w.Current.PrincipalKind, w.Current.PrincipalName, w.Current.IdentityAddress, strings.Join(w.Fields, ", ")))
	return struct{}{}, nil
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
		case sendpolicy.FieldHoldSeconds:
			limit("--hold-seconds", b.HoldSeconds)
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
