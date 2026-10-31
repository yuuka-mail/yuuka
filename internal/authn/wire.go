package authn

import "time"

// Response bodies of the /auth/ endpoints that the WebUI reads. tygo
// generates web/src/api/auth.gen.ts from this file (see
// internal/api/tygo.yaml).

// SessionInfo is GET /auth/session: who the request acts as.
type SessionInfo struct {
	Principal      string   `json:"principal"` // kind:name
	CredentialID   string   `json:"credential_id"`
	CredentialKind string   `json:"credential_kind"` // session | token
	Scopes         []string `json:"scopes"`
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
