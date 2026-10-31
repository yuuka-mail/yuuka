package authn

import (
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
)

// The bodies of the /auth/ endpoints, described in openapi.json
// (internal/api) next to the REST API.

// RegisterBegin is the body of POST /auth/passkey/register/begin. Without
// an enrollment token, the request's session must be a human's with
// admin.
type RegisterBegin struct {
	EnrollmentToken string `json:"enrollment_token,omitempty"`
	Label           string `json:"label,omitempty"`
}

// RecoveryRequest is the body of POST /auth/recovery.
type RecoveryRequest struct {
	Code string `json:"code"`
}

// SessionInfo is GET /auth/session: who the request acts as.
type SessionInfo struct {
	Principal      string       `json:"principal"` // kind:name
	CredentialID   string       `json:"credential_id"`
	CredentialKind string       `json:"credential_kind"` // session | token
	Scopes         []auth.Scope `json:"scopes"`
	// ElevatedUntil is present while a person's session may widen who
	// may do what (§13.10): until then no passkey prompt is needed.
	ElevatedUntil *time.Time `json:"elevated_until,omitempty"`
}

// ElevateResult is POST /auth/passkey/elevate/finish.
type ElevateResult struct {
	ElevatedUntil time.Time `json:"elevated_until"`
}

// LoginResult is POST /auth/passkey/login/finish; the session itself is
// the cookie.
type LoginResult struct {
	Principal string    `json:"principal"`
	ExpiresAt time.Time `json:"expires_at"`
}

// RegisterResult is POST /auth/passkey/register/finish.
type RegisterResult struct {
	PasskeyID string `json:"passkey_id"`
}

// RecoveryGrant is POST /auth/recovery: an enrollment token, never a
// session.
type RecoveryGrant struct {
	EnrollmentToken string    `json:"enrollment_token"`
	ExpiresAt       time.Time `json:"expires_at"`
}
