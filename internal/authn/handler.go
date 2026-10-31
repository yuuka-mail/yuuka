package authn

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// maxBody bounds every JSON request; an attestation is a few KiB.
const maxBody = 64 << 10

// Handler serves the JSON endpoints of the WebUI login under /auth/:
//
//	POST /auth/passkey/register/begin   {"enrollment_token"?, "label"?}  — token, or a session with admin
//	POST /auth/passkey/register/finish  PublicKeyCredential (attestation)
//	POST /auth/passkey/login/begin      {}
//	POST /auth/passkey/login/finish     PublicKeyCredential (assertion) → session cookie
//	POST /auth/recovery                 {"code"} → {"enrollment_token", "expires_at"}
//	POST /auth/logout                   session
//	GET  /auth/session                  session or bearer token → the viewer
//
// Only login/finish ever creates a session. Registration, enrollment and
// recovery never do: a recovery code buys one enrollment token, the token
// buys one passkey registration, and only that passkey logs in.
//
// CSRF: session cookies are SameSite=Strict, so a cross-site page cannot
// make the browser send them at all. As a second layer every unsafe
// request here, and every cookie-authenticated unsafe request through
// Require, passes net/http's CrossOriginProtection (Sec-Fetch-Site, then
// Origin against Host or the configured origin). As a third, POST bodies
// must be application/json, which an HTML form cannot send cross-origin
// without a CORS preflight that yuuka never answers. Bearer tokens are
// not ambient, so they need none of this.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /auth/passkey/register/begin", s.Optional(http.HandlerFunc(s.registerBegin)))
	mux.Handle("POST /auth/passkey/register/finish", s.Optional(http.HandlerFunc(s.registerFinish)))
	mux.HandleFunc("POST /auth/passkey/login/begin", s.loginBegin)
	mux.HandleFunc("POST /auth/passkey/login/finish", s.loginFinish)
	mux.HandleFunc("POST /auth/recovery", s.recover)
	mux.Handle("POST /auth/logout", s.Require(http.HandlerFunc(s.logout)))
	mux.Handle("GET /auth/session", s.Require(http.HandlerFunc(s.session)))
	return s.csrf.Handler(mux)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// readJSON reads a bounded application/json body. An empty body is
// allowed and leaves raw empty.
func readJSON(w http.ResponseWriter, r *http.Request) (raw []byte, ok bool) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "want application/json")
		return nil, false
	}
	raw, err = io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body too large")
		return nil, false
	}
	return raw, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	raw, ok := readJSON(w, r)
	if !ok {
		return false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return true
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad JSON")
		return false
	}
	return true
}

// user adapts a principal and its passkeys to the library.
type user struct {
	p    store.Principal
	keys []store.Passkey
}

func (u *user) WebAuthnID() []byte          { return []byte(u.p.ID) }
func (u *user) WebAuthnName() string        { return u.p.Name }
func (u *user) WebAuthnDisplayName() string { return u.p.DisplayName }
func (u *user) WebAuthnCredentials() []webauthn.Credential {
	out := make([]webauthn.Credential, len(u.keys))
	for i, k := range u.keys {
		flags := protocol.FlagUserPresent
		if k.UserVerified {
			flags |= protocol.FlagUserVerified
		}
		if k.BackupEligible {
			flags |= protocol.FlagBackupEligible
		}
		if k.BackupState {
			flags |= protocol.FlagBackupState
		}
		transports := make([]protocol.AuthenticatorTransport, len(k.Transports))
		for j, t := range k.Transports {
			transports[j] = protocol.AuthenticatorTransport(t)
		}
		out[i] = webauthn.Credential{
			ID: k.CredentialID, PublicKey: k.PublicKey,
			AttestationType: k.AttestationType, AttestationFormat: k.AttestationFormat,
			Transport: transports, Flags: webauthn.NewCredentialFlags(flags),
			Authenticator: webauthn.Authenticator{AAGUID: k.AAGUID, SignCount: k.SignCount},
		}
	}
	return out
}

func (s *Service) loadUser(r *http.Request, principalID string) (*user, error) {
	p, err := s.db.Principal(r.Context(), principalID)
	if err != nil {
		return nil, err
	}
	if p.Kind != "human" || p.DisabledAt != nil {
		return nil, store.ErrNotFound
	}
	keys, err := s.db.Passkeys(r.Context(), p.ID)
	if err != nil {
		return nil, err
	}
	return &user{p: p, keys: keys}, nil
}

func (s *Service) startCeremony(w http.ResponseWriter, cer ceremony) bool {
	cer.expires = s.now().Add(ceremonyTTL)
	id, err := s.ceremonies.put(cer, s.now())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "busy, retry later")
		return false
	}
	setCookie(w, ceremonyCookie, id, int(ceremonyTTL/time.Second))
	return true
}

func (s *Service) takeCeremony(w http.ResponseWriter, r *http.Request, register bool) (ceremony, bool) {
	clearCookie(w, ceremonyCookie)
	c, err := r.Cookie(ceremonyCookie)
	if err != nil {
		writeError(w, http.StatusBadRequest, "no ceremony in progress")
		return ceremony{}, false
	}
	cer, ok := s.ceremonies.take(c.Value, s.now())
	if !ok || cer.register != register {
		writeError(w, http.StatusBadRequest, "ceremony expired or unknown; start again")
		return ceremony{}, false
	}
	return cer, true
}

func (s *Service) registerBegin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		EnrollmentToken string `json:"enrollment_token"`
		Label           string `json:"label"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Label) > 100 {
		writeError(w, http.StatusBadRequest, "label too long")
		return
	}
	cer := ceremony{register: true, label: req.Label}
	if req.EnrollmentToken != "" {
		if !s.enrollLimit.allow(remoteHost(r)) {
			writeError(w, http.StatusTooManyRequests, "too many attempts")
			return
		}
		t, ok := s.checkEnrollmentToken(r, req.EnrollmentToken)
		if !ok {
			writeError(w, http.StatusUnauthorized, "enrollment link invalid, used or expired")
			return
		}
		cer.principalID, cer.tokenID, cer.tokenReason = t.PrincipalID, t.ID, t.Reason
	} else {
		v, ok := auth.ViewerFrom(r.Context())
		switch {
		case !ok:
			writeError(w, http.StatusUnauthorized, "enrollment token or session required")
			return
		case v.CredentialKind != auth.KindSession || v.PrincipalKind != "human" || !v.Has(auth.Admin):
			writeError(w, http.StatusForbidden, "only a human session with admin can add a passkey")
			return
		}
		cer.principalID, cer.credentialID = v.PrincipalID, v.CredentialID
	}
	u, err := s.loadUser(r, cer.principalID)
	if err != nil {
		s.log.Error("auth: register begin", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	creation, data, err := s.wa.BeginRegistration(u,
		webauthn.WithExclusions(webauthn.Credentials(u.WebAuthnCredentials()).CredentialDescriptors()))
	if err != nil {
		s.log.Error("auth: register begin", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	cer.data = *data
	if s.startCeremony(w, cer) {
		writeJSON(w, http.StatusOK, creation)
	}
}

// checkEnrollmentToken validates without consuming; registerFinish
// consumes atomically with storing the passkey.
func (s *Service) checkEnrollmentToken(r *http.Request, text string) (store.EnrollmentToken, bool) {
	id, hash, ok := auth.ParseSecret(auth.PrefixEnrollment, text)
	if !ok {
		s.record(r.Context(), store.Action{Tool: "auth.enroll.reject", Args: map[string]any{"reason": "malformed", "remote": remoteHost(r)}})
		return store.EnrollmentToken{}, false
	}
	t, err := s.db.EnrollmentTokenByID(r.Context(), id)
	reason := ""
	switch {
	case errors.Is(err, store.ErrNotFound):
		reason = "unknown"
	case err != nil:
		s.log.Error("auth: enrollment token", "err", err)
		reason = "lookup failed"
	case !auth.HashEqual(t.SecretHash, hash):
		reason = "bad secret"
	case t.UsedAt != nil:
		reason = "used"
	case !s.now().Before(t.ExpiresAt):
		reason = "expired"
	}
	if reason != "" {
		s.record(r.Context(), store.Action{Tool: "auth.enroll.reject", Args: map[string]any{
			"reason": reason, "enrollment_token_id": id, "remote": remoteHost(r),
		}})
		return t, false
	}
	return t, true
}

func (s *Service) registerFinish(w http.ResponseWriter, r *http.Request) {
	cer, ok := s.takeCeremony(w, r, true)
	if !ok {
		return
	}
	if cer.credentialID != "" {
		// Started by a session: it must still be that live session.
		v, ok := auth.ViewerFrom(r.Context())
		if !ok || v.CredentialID != cer.credentialID {
			writeError(w, http.StatusUnauthorized, "session ended during registration")
			return
		}
	}
	raw, ok := readJSON(w, r)
	if !ok {
		return
	}
	u, err := s.loadUser(r, cer.principalID)
	if err != nil {
		s.log.Error("auth: register finish", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(raw)
	var cred *webauthn.Credential
	if err == nil {
		cred, err = s.wa.CreateCredential(u, cer.data, parsed)
	}
	if err != nil {
		s.record(r.Context(), store.Action{
			Actor: cer.principalID, CredentialID: cer.credentialID, Tool: "auth.passkey.reject",
			Args: map[string]any{"reason": protocolReason(err), "remote": remoteHost(r)},
		})
		writeError(w, http.StatusBadRequest, "registration not verified")
		return
	}
	now := s.now()
	transports := make([]string, len(cred.Transport))
	for i, t := range cred.Transport {
		transports[i] = string(t)
	}
	pk := store.Passkey{
		ID: ulid.New(now), PrincipalID: cer.principalID, CredentialID: cred.ID, PublicKey: cred.PublicKey,
		SignCount: cred.Authenticator.SignCount, AAGUID: cred.Authenticator.AAGUID, Transports: transports,
		UserVerified: cred.Flags.UserVerified, BackupEligible: cred.Flags.BackupEligible, BackupState: cred.Flags.BackupState,
		AttestationType: cred.AttestationType, AttestationFormat: cred.AttestationFormat, Label: cer.label,
	}
	via := "session"
	if cer.tokenID != "" {
		via = cer.tokenReason
	}
	err = s.db.AddPasskey(r.Context(), store.NewPasskey{
		Passkey: pk, EnrollmentTokenID: cer.tokenID,
		Action: store.Action{
			ID: ulid.New(now), Actor: cer.principalID, CredentialID: cer.credentialID, Tool: "auth.passkey.register", At: now,
			Args: map[string]any{"passkey_id": pk.ID, "via": via, "enrollment_token_id": cer.tokenID, "label": cer.label, "remote": remoteHost(r)},
		},
	})
	switch {
	case errors.Is(err, store.ErrSpent):
		s.record(r.Context(), store.Action{Tool: "auth.enroll.reject", Args: map[string]any{
			"reason": "used or expired at finish", "enrollment_token_id": cer.tokenID, "remote": remoteHost(r),
		}})
		writeError(w, http.StatusUnauthorized, "enrollment link invalid, used or expired")
		return
	case errors.Is(err, store.ErrDuplicate):
		writeError(w, http.StatusConflict, "passkey already registered")
		return
	case err != nil:
		s.log.Error("auth: store passkey", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.alertf(r.Context(), "a new passkey was registered for human:%s (via %s). If this was not you, revoke it and run `yuuka admin enroll`.", u.p.Name, via)
	writeJSON(w, http.StatusCreated, RegisterResult{PasskeyID: pk.ID})
}

func (s *Service) loginBegin(w http.ResponseWriter, r *http.Request) {
	if !s.loginLimit.allow(remoteHost(r)) {
		writeError(w, http.StatusTooManyRequests, "too many attempts")
		return
	}
	if _, ok := readJSON(w, r); !ok {
		return
	}
	assertion, data, err := s.wa.BeginDiscoverableLogin()
	if err != nil {
		s.log.Error("auth: login begin", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if s.startCeremony(w, ceremony{data: *data}) {
		writeJSON(w, http.StatusOK, assertion)
	}
}

func (s *Service) loginFinish(w http.ResponseWriter, r *http.Request) {
	if !s.loginLimit.allow(remoteHost(r)) {
		writeError(w, http.StatusTooManyRequests, "too many attempts")
		return
	}
	cer, ok := s.takeCeremony(w, r, false)
	if !ok {
		return
	}
	raw, ok := readJSON(w, r)
	if !ok {
		return
	}
	var (
		u  *user
		pk store.Passkey
	)
	lookup := func(rawID, userHandle []byte) (webauthn.User, error) {
		var err error
		if pk, err = s.db.PasskeyByCredentialID(r.Context(), rawID); err != nil {
			return nil, err
		}
		if pk.PrincipalID != string(userHandle) {
			return nil, errors.New("user handle does not own the passkey")
		}
		u, err = s.loadUser(r, pk.PrincipalID)
		return u, err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(raw)
	var cred *webauthn.Credential
	if err == nil {
		_, cred, err = s.wa.ValidatePasskeyLogin(lookup, cer.data, parsed)
	}
	if err == nil && cred.Authenticator.CloneWarning {
		err = errors.New("signature counter went backwards: possible cloned authenticator")
		s.alertf(r.Context(), "a passkey of human:%s presented a stale signature counter; login refused. Consider revoking it.", u.p.Name)
	}
	if err != nil {
		s.record(r.Context(), store.Action{Tool: "auth.login.reject", Args: map[string]any{
			"reason": protocolReason(err), "passkey_id": pk.ID, "remote": remoteHost(r),
		}})
		writeError(w, http.StatusUnauthorized, "login not verified")
		return
	}

	now := s.now()
	sec := auth.NewSecret(auth.PrefixSession, ulid.New(now))
	expires := now.Add(SessionTTL)
	err = s.db.CompleteLogin(r.Context(), store.Login{
		PasskeyID: pk.ID, SignCount: cred.Authenticator.SignCount,
		BackupState: cred.Flags.BackupState, UserVerified: cred.Flags.UserVerified,
		Session: store.Credential{
			ID: sec.ID, PrincipalID: u.p.ID, Kind: auth.KindSession, Label: pk.Label,
			SecretHash: sec.Hash[:], Scopes: auth.Strings(auth.AllScopes), CreatedAt: now, ExpiresAt: &expires,
		},
		Action: store.Action{
			ID: ulid.New(now), Actor: u.p.ID, CredentialID: sec.ID, Tool: "auth.login", At: now,
			Args: map[string]any{"passkey_id": pk.ID, "remote": remoteHost(r)},
		},
	})
	if err != nil {
		s.log.Error("auth: complete login", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	setCookie(w, SessionCookie, sec.Text, int(SessionTTL/time.Second))
	writeJSON(w, http.StatusOK, LoginResult{Principal: "human:" + u.p.Name, ExpiresAt: expires})
}

// protocolReason keeps the audit log readable: the library's details say
// what failed without echoing anything secret.
func protocolReason(err error) string {
	if pe, ok := errors.AsType[*protocol.Error](err); ok {
		return pe.Type + ": " + pe.Details
	}
	return err.Error()
}

func (s *Service) recover(w http.ResponseWriter, r *http.Request) {
	if !s.recoveryLimit.allow("") {
		writeError(w, http.StatusTooManyRequests, "too many recovery attempts; wait, or use `yuuka admin enroll` on the host")
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	now := s.now()
	reject := func(reason string) {
		s.record(r.Context(), store.Action{Tool: "auth.recovery.reject", Args: map[string]any{"reason": reason, "remote": remoteHost(r)}})
		writeError(w, http.StatusUnauthorized, "recovery code invalid or already used")
	}
	hash, ok := auth.HashRecoveryCode(req.Code)
	if !ok {
		reject("malformed")
		return
	}
	// Found by hash, not compared in Go: a code has no id to look up by.
	// The index lookup's timing depends only on the SHA-256 of the input,
	// which an attacker cannot steer towards a stored hash.
	tok := auth.NewSecret(auth.PrefixEnrollment, ulid.New(now))
	expires := now.Add(RecoveryTokenTTL)
	principal, err := s.db.UseRecoveryCode(r.Context(), store.RecoveryUse{
		CodeHash: hash,
		Token:    store.EnrollmentToken{ID: tok.ID, SecretHash: tok.Hash[:], Reason: "recovery", ExpiresAt: expires},
		Action: store.Action{
			ID: ulid.New(now), Tool: "auth.recovery.use", At: now,
			Args: map[string]any{"remote": remoteHost(r)},
		},
	})
	if errors.Is(err, store.ErrSpent) {
		reject("no unused code matches")
		return
	}
	if err != nil {
		s.log.Error("auth: recovery", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	left, _ := s.db.UnusedRecoveryCodes(r.Context(), principal)
	s.alertf(r.Context(), "a recovery code was used from %s; %d left. If this was not you, run `yuuka admin enroll` on the host to rotate them.", remoteHost(r), left)
	writeJSON(w, http.StatusOK, RecoveryGrant{EnrollmentToken: tok.Text, ExpiresAt: expires})
}

func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	v, _ := auth.ViewerFrom(r.Context())
	if v.CredentialKind != auth.KindSession {
		writeError(w, http.StatusBadRequest, "logout ends a browser session; revoke tokens instead")
		return
	}
	now := s.now()
	err := s.db.RevokeCredential(r.Context(), v.CredentialID, store.Action{
		ID: ulid.New(now), Actor: v.PrincipalID, CredentialID: v.CredentialID, Tool: "auth.logout", At: now,
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.Error("auth: logout", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	clearCookie(w, SessionCookie)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) session(w http.ResponseWriter, r *http.Request) {
	v, _ := auth.ViewerFrom(r.Context())
	writeJSON(w, http.StatusOK, SessionInfo{
		Principal: v.String(), CredentialID: v.CredentialID, CredentialKind: v.CredentialKind,
		Scopes: auth.Strings(v.Scopes),
	})
}
