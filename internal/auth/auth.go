// Package auth holds the vocabulary of authorization (§13): flat scopes,
// the Viewer every request acts as, and the formats of the secrets that
// identify a credential. It depends on nothing but the standard library so
// that the store can require a Viewer on read queries (§13.8) without an
// import cycle; authentication itself lives in internal/authn.
package auth

import (
	"context"
	"fmt"
	"slices"
)

// Scope is one flat permission (§13.4). There are no roles and no
// implication between scopes: admin does not include message:read.
type Scope string

// The closed list of §13.4. Adding one also needs a migration that
// replaces credentials_scopes_known.
const (
	MessageRead     Scope = "message:read"
	DeliveryRead    Scope = "delivery:read"
	PrefixRead      Scope = "prefix:read"
	MessageAnnotate Scope = "message:annotate"
	DraftCreate     Scope = "draft:create"
	SubmissionSend  Scope = "submission:send"
	PrefixManage    Scope = "prefix:manage"
	AccountManage   Scope = "account:manage"
	RuleManage      Scope = "rule:manage"
	Admin           Scope = "admin"
)

// AllScopes is every scope, in the order of §13.4. A human browser session
// holds all of them.
var AllScopes = []Scope{
	MessageRead, DeliveryRead, PrefixRead,
	MessageAnnotate, DraftCreate, SubmissionSend,
	PrefixManage, AccountManage, RuleManage, Admin,
}

// ParseScopes validates scope strings as stored in credentials.scopes.
func ParseScopes(ss []string) ([]Scope, error) {
	out := make([]Scope, 0, len(ss))
	for _, s := range ss {
		if !slices.Contains(AllScopes, Scope(s)) {
			return nil, fmt.Errorf("unknown scope %q", s)
		}
		out = append(out, Scope(s))
	}
	return out, nil
}

// Enum lists every scope: the API schema says which exist.
func (Scope) Enum() []string { return Strings(AllScopes) }

// Strings converts scopes for storage.
func Strings(scopes []Scope) []string {
	out := make([]string, len(scopes))
	for i, s := range scopes {
		out[i] = string(s)
	}
	return out
}

// Credential kinds that can authenticate a request.
const (
	KindSession = "session"
	KindToken   = "token"
	// KindSMTPPassword authenticates the SMTP submission port (§13.6).
	KindSMTPPassword = "smtp_password"
)

// Viewer is who a request acts as: a principal, through one credential,
// with that credential's scopes. Every authenticated handler gets one from
// the context; phase 2 store reads take it as a parameter so visibility
// (§13.8) is enforced in one place. Treat it as immutable.
type Viewer struct {
	PrincipalID    string
	PrincipalKind  string // human | agent | service | rule | system
	PrincipalName  string
	CredentialID   string
	CredentialKind string // session | token | smtp_password
	Scopes         []Scope
}

// String is kind:name, the form used in logs and the audit trail.
func (v *Viewer) String() string { return v.PrincipalKind + ":" + v.PrincipalName }

// Has reports whether the viewer holds scope s.
func (v *Viewer) Has(s Scope) bool { return v != nil && slices.Contains(v.Scopes, s) }

// Missing returns the scopes of required that the viewer lacks.
func (v *Viewer) Missing(required ...Scope) []Scope {
	var out []Scope
	for _, s := range required {
		if !v.Has(s) {
			out = append(out, s)
		}
	}
	return out
}

type viewerKey struct{}

// WithViewer returns ctx carrying v.
func WithViewer(ctx context.Context, v *Viewer) context.Context {
	return context.WithValue(ctx, viewerKey{}, v)
}

// ViewerFrom returns the viewer of an authenticated request.
func ViewerFrom(ctx context.Context) (*Viewer, bool) {
	v, ok := ctx.Value(viewerKey{}).(*Viewer)
	return v, ok && v != nil
}
