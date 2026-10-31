package api

import (
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/ingest"
)

// M3 management actions (§13.10): decisions prompted by mail, and the
// settings that say who may do what, which the WebUI can read and
// tighten but not widen.

// PrefixAddress is a prefix@zone named for a decision, read as `yuuka
// prefix` reads it (ingest.ParsePrefixAddress), so a claim keys the mail
// the way ingest does.
type PrefixAddress struct{ Prefix, Zone string }

func (m *PrefixAddress) UnmarshalText(b []byte) error {
	zone, prefix, err := ingest.ParsePrefixAddress(string(b))
	m.Zone, m.Prefix = zone, prefix
	return err
}

// PrefixDecisionParams name the prefix a decision is about.
type PrefixDecisionParams struct {
	Address PrefixAddress `path:"address" jsonschema:"prefix@zone"`
}

// PrefixOutcome is what a claim or an unclaim did: created (claimed
// before any mail reached it), updated, unchanged, or removed (an
// unclaimed prefix that never received mail leaves the registry).
type PrefixOutcome string

func (PrefixOutcome) Enum() []string { return []string{"created", "updated", "unchanged", "removed"} }

// PrefixDecision answers claim and unclaim. Nothing about the mail is
// rewritten: downgrades and the leak check are computed on read, so the
// prefix's counts are what every view shows from now on.
type PrefixDecision struct {
	Outcome PrefixOutcome `json:"outcome"`
	// Prefix is absent once the prefix has left the registry.
	Prefix *Prefix `json:"prefix,omitempty"`
}

// ExpectBody replaces a prefix's expected domains; empty expects none.
type ExpectBody struct {
	Domains []string `json:"domains"`
}

// ExpectParams are the parameters of PUT /api/v1/prefixes/{address}/expected-domains.
type ExpectParams struct {
	Address PrefixAddress `path:"address" jsonschema:"prefix@zone"`
	Body    ExpectBody    `body:"json"`
}

// CountedAs is a domain given that is stored as its organizational
// domain: senders are compared by it.
type CountedAs struct {
	Domain string `json:"domain"`
	As     string `json:"as"`
}

// ExpectResult answers a change of expected domains: what was stored,
// and what the leak check now makes of the mail already received (the
// prefix's observed senders, leaks and spoofs), which `yuuka prefix
// expect` prints too.
type ExpectResult struct {
	// Changed is false when the domains were those already.
	Changed   bool        `json:"changed"`
	Expected  []string    `json:"expected"`
	CountedAs []CountedAs `json:"counted_as"`
	// Prefix is absent only if the prefix left the registry meanwhile.
	Prefix *Prefix `json:"prefix,omitempty"`
}

// CredentialKind is the kind of a credential the settings list.
type CredentialKind string

func (CredentialKind) Enum() []string { return []string{"token", "smtp_password"} }

// ManagedCredential is a bearer token or an SMTP password, never its
// secret. Sessions are not listed: they end by signing out.
type ManagedCredential struct {
	ID                string         `json:"id"`
	Kind              CredentialKind `json:"kind"`
	Principal         Actor          `json:"principal"`
	PrincipalDisabled bool           `json:"principal_disabled"`
	Label             string         `json:"label"`
	Scopes            []auth.Scope   `json:"scopes"`
	CreatedAt         time.Time      `json:"created_at"`
	// ExpiresAt is absent for one that does not expire.
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// CredentialList is GET /api/v1/admin/credentials: every principal's,
// oldest first, revoked and expired ones included. Issuing one is done
// on the host (`yuuka token issue`).
type CredentialList struct {
	Credentials []ManagedCredential `json:"credentials"`
}

// ManagedIdentity is an identity as the settings list it.
type ManagedIdentity struct {
	ID string `json:"id"`
	// Address is local@zone, or *@zone for the pattern.
	Address     string     `json:"address"`
	DisplayName string     `json:"display_name"`
	Owner       Actor      `json:"owner"`
	Transport   string     `json:"transport"`
	CreatedAt   time.Time  `json:"created_at"`
	DisabledAt  *time.Time `json:"disabled_at,omitempty"`
	// TakenOverBy is the enabled *@zone identity that would send this
	// address once it is disabled, with its own owner and grants.
	// Disabling it may then let more through, so it is done on the host
	// (Host names the command).
	TakenOverBy string `json:"taken_over_by,omitempty"`
	// Host is the host command that disables it.
	Host string `json:"host"`
}

// IdentityAdminList is GET /api/v1/admin/identities, disabled ones
// included. Adding one is done on the host (`yuuka identity add`).
type IdentityAdminList struct {
	Identities []ManagedIdentity `json:"identities"`
}

// GrantIdentity is the identity a grant is on.
type GrantIdentity struct {
	ID       string `json:"id"`
	Address  string `json:"address"`
	Owner    Actor  `json:"owner"`
	Disabled bool   `json:"disabled"`
}

// StoredGrant is a grant row as written. An absent limit is NULL: the
// default of the principal's kind applies (applied says which).
type StoredGrant struct {
	SendPolicy     Tier           `json:"send_policy"`
	RecipientScope RecipientScope `json:"recipient_scope"`
	Allowlist      []string       `json:"allowlist"`
	Limits         Limits         `json:"limits"`
	HoldSeconds    int            `json:"hold_seconds"`
	SuspendedAt    *time.Time     `json:"suspended_at,omitempty"`
}

// AppliedGrant is what a grant lets its principal do: limits resolved
// (an absent one is unlimited), the tier after the owner cap.
type AppliedGrant struct {
	SendPolicy     Tier           `json:"send_policy"`
	RecipientScope RecipientScope `json:"recipient_scope"`
	Allowlist      []string       `json:"allowlist"`
	Limits         Limits         `json:"limits"`
	HoldSeconds    int            `json:"hold_seconds"`
	Suspended      bool           `json:"suspended"`
}

// ManagedGrant is a grant row as the settings list it.
type ManagedGrant struct {
	Identity  GrantIdentity `json:"identity"`
	Principal Actor         `json:"principal"`
	Stored    StoredGrant   `json:"stored"`
	Applied   AppliedGrant  `json:"applied"`
	// OwnerCapped: the identity belongs to a human who is not the
	// principal, so the tier is draft at most whatever is stored (§8.9).
	OwnerCapped bool `json:"owner_capped"`
	// Default is what applies once the row is removed.
	Default AppliedGrant `json:"default"`
	// RemovalWidens names what Default lets through that the row does
	// not; removing the row is then done on the host.
	RemovalWidens []string  `json:"removal_widens"`
	CreatedAt     time.Time `json:"created_at"`
	// Host is the host command that changes this grant; widening is
	// done there, with flags as `yuuka grant set` takes them: --policy,
	// --recipients, --allow, --max-per-hour, --max-per-day,
	// --max-new-recipients-per-day, --hold-seconds, --resume.
	Host string `json:"host"`
}

// GrantAdminList is GET /api/v1/admin/grants, by identity, then
// principal. A pair without a row is at its default (§13.5), which only
// the host changes.
type GrantAdminList struct {
	Grants []ManagedGrant `json:"grants"`
}

// GrantParams name one grant row.
type GrantParams struct {
	Identity  string `path:"identity" jsonschema:"the identity's id"`
	Principal string `path:"principal" jsonschema:"the principal's id"`
}

// TightenBody is what to tighten; an absent field is left as it is.
// Every field must let no more through than the grant does now, applied
// (limits resolved, the owner cap taken), or nothing is changed and the
// answer is 403 widening_requires_cli. The orders, tightest first:
// send_policy none < draft < review < auto; recipient_scope thread <
// any and allowlist < any, thread and allowlist not comparable (either
// into the other widens); allowlist a subset of the current one; a
// smaller limit; suspended.
type TightenBody struct {
	SendPolicy     *Tier           `json:"send_policy,omitempty"`
	RecipientScope *RecipientScope `json:"recipient_scope,omitempty"`
	// Allowlist is the list to keep: entries of the current one. An
	// empty list empties it.
	Allowlist              []string `json:"allowlist,omitempty"`
	MaxPerHour             *int     `json:"max_per_hour,omitempty" minimum:"0"`
	MaxPerDay              *int     `json:"max_per_day,omitempty" minimum:"0"`
	MaxNewRecipientsPerDay *int     `json:"max_new_recipients_per_day,omitempty" minimum:"0"`
	// Suspended true suspends the grant: everything under it is held
	// as a draft. Lifting a suspension is done on the host.
	Suspended *bool `json:"suspended,omitempty"`
}

// TightenParams are the parameters of POST .../tighten.
type TightenParams struct {
	GrantParams
	Body TightenBody `body:"json"`
}
