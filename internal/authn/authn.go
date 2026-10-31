// Package authn turns credentials into an auth.Viewer (§13.7): browser
// sessions created by passkey login, bearer tokens for REST/MCP callers,
// one-time enrollment links and offline recovery codes. It also provides
// the net/http middleware that enforces scopes and the JSON endpoints of
// the WebUI login; the pages themselves are phase 2.
package authn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/clientip"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

const (
	// SessionTTL is the absolute lifetime of a browser session. There is
	// no sliding renewal: logging in again with a passkey is one touch.
	SessionTTL = 7 * 24 * time.Hour
	// DefaultEnrollTTL is how long a link from `yuuka admin enroll` stays
	// valid.
	DefaultEnrollTTL = 15 * time.Minute
	// RecoveryTokenTTL is how long the enrollment token bought with a
	// recovery code stays valid.
	RecoveryTokenTTL = 10 * time.Minute
	// RecoveryCodeCount is how many codes enroll prints.
	RecoveryCodeCount = 8
	// ceremonyTTL bounds a WebAuthn ceremony from begin to finish.
	ceremonyTTL = 5 * time.Minute
)

// ErrUnauthenticated: the request carries no valid credential.
var ErrUnauthenticated = errors.New("unauthenticated")

// rejection is a credential that exists but was refused; it is recorded.
type rejection struct {
	credentialID string
	reason       string
}

func (r *rejection) Error() string { return "credential " + r.credentialID + " rejected: " + r.reason }
func (r *rejection) Unwrap() error { return ErrUnauthenticated }

// Alerter pushes a security notice to the operator (Telegram in practice).
type Alerter interface {
	Notify(ctx context.Context, text string) error
}

// Config configures a Service.
type Config struct {
	DB *store.DB
	// Origin is the WebUI origin exactly as the browser sees it,
	// scheme://host[:port]. It is the WebAuthn origin, the trusted origin
	// for cross-origin checks and the base of enrollment links. Nothing
	// about the network in between (proxy, tunnel) is assumed (§2).
	Origin string
	// RPID is the WebAuthn relying party id; empty means Origin's host.
	RPID string
	// Alerter is optional.
	Alerter Alerter
	// Proxies are the reverse proxies whose X-Forwarded-For names the
	// client for rate limits and the audit log; the zero value trusts
	// none and uses the TCP peer.
	Proxies clientip.Proxies
	Log     *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Service authenticates requests and runs the login ceremonies.
type Service struct {
	db         *store.DB
	origin     string
	wa         *webauthn.WebAuthn
	alert      Alerter
	log        *slog.Logger
	now        func() time.Time
	csrf       *http.CrossOriginProtection
	ceremonies *ceremonies
	proxies    clientip.Proxies

	// Per-remote limits for login and enrollment; recovery is limited
	// globally (§13.7 "严格限速"): behind a proxy nobody configured as
	// trusted every caller shares one address anyway, and a locked
	// recovery door only sends the operator to the CLI. rejectLog keeps a revoked token that a client keeps
	// retrying from flooding the audit log.
	loginLimit    *limiter
	enrollLimit   *limiter
	recoveryLimit *limiter
	rejectLog     *limiter
}

// New validates cfg and returns a Service.
func New(cfg Config) (*Service, error) {
	origin, host, err := ParseOrigin(cfg.Origin)
	if err != nil {
		return nil, err
	}
	rpID := cfg.RPID
	if rpID == "" {
		rpID = host
	}
	if host != rpID && !strings.HasSuffix(host, "."+rpID) {
		return nil, fmt.Errorf("WebAuthn RP ID %q must be the origin host %q or a parent domain of it", rpID, host)
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPID:                  rpID,
		RPDisplayName:         "yuuka",
		RPOrigins:             []string{origin},
		AttestationPreference: protocol.PreferNoAttestation,
		// Discoverable credentials so login needs no username, and user
		// verification so a passkey is two factors in itself.
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		},
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Enforce: true, Timeout: ceremonyTTL, TimeoutUVD: ceremonyTTL},
			Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: ceremonyTTL, TimeoutUVD: ceremonyTTL},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("webauthn: %w", err)
	}
	csrf := http.NewCrossOriginProtection()
	if err := csrf.AddTrustedOrigin(origin); err != nil {
		return nil, fmt.Errorf("origin: %w", err)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		db:            cfg.DB,
		origin:        origin,
		wa:            wa,
		alert:         cfg.Alerter,
		log:           log,
		now:           now,
		csrf:          csrf,
		ceremonies:    newCeremonies(1000),
		proxies:       cfg.Proxies,
		loginLimit:    newLimiter(20, 3*time.Second, now),
		enrollLimit:   newLimiter(10, 6*time.Second, now),
		recoveryLimit: newLimiter(3, 5*time.Minute, now),
		rejectLog:     newLimiter(1, 10*time.Minute, now),
	}, nil
}

// ParseOrigin checks that s is a bare origin a browser will run WebAuthn
// on: https, or http on localhost (a secure context by definition). It
// returns the canonical form and the host without port.
func ParseOrigin(s string) (origin, host string, err error) {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("origin %q: want scheme://host[:port] and nothing else", s)
	}
	host = strings.ToLower(u.Hostname())
	scheme := strings.ToLower(u.Scheme)
	switch {
	case scheme == "https":
	case scheme == "http" && host == "localhost":
	default:
		return "", "", fmt.Errorf("origin %q: must be https (http only for localhost)", s)
	}
	if net.ParseIP(host) != nil {
		return "", "", fmt.Errorf("origin %q: WebAuthn needs a domain name, not an IP address", s)
	}
	origin = scheme + "://" + strings.ToLower(u.Host)
	return origin, host, nil
}

// Authenticate resolves a session cookie value (kind session) or a bearer
// token (kind token) to a Viewer. Errors wrap ErrUnauthenticated unless
// the database failed.
func (s *Service) Authenticate(ctx context.Context, kind, text string) (*auth.Viewer, error) {
	return authenticate(ctx, s.db, s.log, s.now(), kind, text)
}

func authenticate(ctx context.Context, db *store.DB, log *slog.Logger, now time.Time, kind, text string) (*auth.Viewer, error) {
	id, hash, ok := auth.ParseSecret(secretPrefix(kind), text)
	if !ok {
		return nil, ErrUnauthenticated
	}
	c, err := db.CredentialForAuth(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrUnauthenticated
	}
	if err != nil {
		return nil, err
	}
	// The secret is checked first so that a caller who only knows an id
	// learns nothing about the credential's state.
	switch {
	case !auth.HashEqual(c.SecretHash, hash):
		return nil, &rejection{c.ID, "bad secret"}
	case c.Kind != kind:
		return nil, &rejection{c.ID, "wrong kind " + c.Kind}
	case c.RevokedAt != nil:
		return nil, &rejection{c.ID, "revoked"}
	case c.ExpiresAt != nil && !now.Before(*c.ExpiresAt):
		return nil, &rejection{c.ID, "expired"}
	case c.PrincipalDisabled:
		return nil, &rejection{c.ID, "principal disabled"}
	}
	scopes, err := auth.ParseScopes(c.Scopes)
	if err != nil {
		return nil, fmt.Errorf("credential %s: %w", c.ID, err)
	}
	if err := db.TouchCredential(ctx, c.ID, now); err != nil {
		log.Warn("auth: cannot record credential use", "credential", c.ID, "err", err)
	}
	return &auth.Viewer{
		PrincipalID:    c.PrincipalID,
		PrincipalKind:  c.PrincipalKind,
		PrincipalName:  c.PrincipalName,
		CredentialID:   c.ID,
		CredentialKind: c.Kind,
		Scopes:         scopes,
	}, nil
}

// secretPrefix is the prefix of a credential kind's secrets.
func secretPrefix(kind string) string {
	switch kind {
	case auth.KindSession:
		return auth.PrefixSession
	case auth.KindSMTPPassword:
		return auth.PrefixSMTP
	}
	return auth.PrefixToken
}

// TokenRequest describes a bearer token or an SMTP password to issue.
type TokenRequest struct {
	// Kind is auth.KindToken (the default when empty) or
	// auth.KindSMTPPassword. An SMTP password does one thing, submit
	// mail, so its Scopes must be exactly submission:send: the scope is
	// what the kind means, not a default filled in.
	Kind        string
	PrincipalID string
	Label       string
	// Scopes are what the token may do, and there is no default: an empty
	// list is refused rather than read as "everything" or "read only".
	Scopes []auth.Scope
	// TTL zero means no expiry; tokens are revoked explicitly.
	TTL time.Duration
	// IssuedBy is the principal recorded as the actor.
	IssuedBy string
	// AuditArgs are added to the recorded action, e.g. how the CLI was
	// run. Never a secret.
	AuditArgs map[string]any
}

// IssueToken creates a bearer token and returns its text, which is shown
// once and never stored.
func (s *Service) IssueToken(ctx context.Context, req TokenRequest) (string, error) {
	return IssueToken(ctx, s.db, s.now(), req)
}

// IssueToken is Service.IssueToken for callers without the WebUI login,
// such as `yuuka token issue`: bearer tokens work where no WebUI origin is
// configured. Only agent and service principals get tokens; a human signs
// in with a passkey and gets a session (§13.7).
//
// For an SMTP password the result is the password; the SMTP user name is
// the credential id inside it (`yuuka token issue --kind smtp` prints
// both), so the submission port finds the row by id and compares the hash
// in constant time, exactly like a token.
func IssueToken(ctx context.Context, db *store.DB, now time.Time, req TokenRequest) (string, error) {
	kind, tool := req.Kind, "auth.token.issue"
	switch kind {
	case "", auth.KindToken:
		kind = auth.KindToken
	case auth.KindSMTPPassword:
		tool = "auth.smtp_password.issue"
		if len(req.Scopes) != 1 || req.Scopes[0] != auth.SubmissionSend {
			return "", fmt.Errorf("issue SMTP password: scopes %v; an SMTP password holds exactly %s", req.Scopes, auth.SubmissionSend)
		}
	default:
		return "", fmt.Errorf("issue token: kind %q is not issued here", kind)
	}
	if len(req.Scopes) == 0 {
		return "", errors.New("issue token: no scopes; a token holds exactly the scopes it is given")
	}
	if _, err := auth.ParseScopes(auth.Strings(req.Scopes)); err != nil {
		return "", fmt.Errorf("issue token: %w", err)
	}
	if req.TTL < 0 {
		return "", fmt.Errorf("issue token: negative lifetime %v", req.TTL)
	}
	p, err := db.Principal(ctx, req.PrincipalID)
	if err != nil {
		return "", fmt.Errorf("issue token: %w", err)
	}
	if p.Kind != "agent" && p.Kind != "service" {
		return "", fmt.Errorf("issue token: %s:%s is a %s principal; tokens are for agent and service principals", p.Kind, p.Name, p.Kind)
	}
	if p.DisabledAt != nil {
		return "", fmt.Errorf("issue token: %s:%s is disabled", p.Kind, p.Name)
	}
	sec := auth.NewSecret(secretPrefix(kind), ulid.New(now))
	var expires *time.Time
	if req.TTL > 0 {
		t := now.Add(req.TTL)
		expires = &t
	}
	args := map[string]any{"credential_id": sec.ID, "principal_id": req.PrincipalID, "label": req.Label, "scopes": req.Scopes}
	if expires != nil {
		args["expires_at"] = expires.UTC()
	}
	for k, v := range req.AuditArgs {
		args[k] = v
	}
	err = db.CreateCredential(ctx, store.Credential{
		ID: sec.ID, PrincipalID: req.PrincipalID, Kind: kind, Label: req.Label,
		SecretHash: sec.Hash[:], Scopes: auth.Strings(req.Scopes), CreatedAt: now, ExpiresAt: expires,
	}, store.Action{ID: ulid.New(now), Actor: req.IssuedBy, Tool: tool, At: now, Args: args})
	if err != nil {
		return "", fmt.Errorf("issue token: %w", err)
	}
	return sec.Text, nil
}

// Revoke revokes a credential on behalf of v.
func (s *Service) Revoke(ctx context.Context, v *auth.Viewer, credentialID string) error {
	now := s.now()
	return s.db.RevokeCredential(ctx, credentialID, store.Action{
		ID: ulid.New(now), Actor: v.PrincipalID, CredentialID: v.CredentialID, Tool: "auth.credential.revoke", At: now,
		Args: map[string]any{"credential_id": credentialID},
	})
}

// RevokeToken revokes the bearer token or SMTP password credentialID for
// `yuuka token revoke`, recording actor and auditArgs. It stops working
// on the next request: REST, MCP over HTTP, every MCP stdio tool call and
// every SMTP session authenticate again. It returns store.ErrNotFound if
// no unrevoked one has that id; other credential kinds are refused, not
// revoked.
func RevokeToken(ctx context.Context, db *store.DB, now time.Time, credentialID, actor string, auditArgs map[string]any) error {
	c, err := db.CredentialForAuth(ctx, credentialID)
	if err != nil {
		return fmt.Errorf("revoke %s: %w", credentialID, err)
	}
	if c.Kind != auth.KindToken && c.Kind != auth.KindSMTPPassword {
		return fmt.Errorf("revoke %s: a %s credential, not a token or SMTP password", credentialID, c.Kind)
	}
	args := map[string]any{"credential_id": credentialID, "principal_id": c.PrincipalID}
	for k, v := range auditArgs {
		args[k] = v
	}
	err = db.RevokeCredential(ctx, credentialID, store.Action{
		ID: ulid.New(now), Actor: actor, Tool: "auth.credential.revoke", At: now, Args: args,
	})
	if err != nil {
		return fmt.Errorf("revoke %s: %w", credentialID, err)
	}
	return nil
}

// Enrollment is what `yuuka admin enroll` prints.
type Enrollment struct {
	Principal     store.Principal
	URL           string
	ExpiresAt     time.Time
	RecoveryCodes []string
}

// Enroll issues a one-time passkey enrollment link for a human principal
// and rotates its recovery codes. With name empty it uses the only human,
// or creates human:operator when there is none.
func (s *Service) Enroll(ctx context.Context, name string, ttl time.Duration) (Enrollment, error) {
	if ttl <= 0 || ttl > 24*time.Hour {
		return Enrollment{}, fmt.Errorf("enrollment link lifetime %v: want (0, 24h]", ttl)
	}
	p, err := s.resolveHuman(ctx, name)
	if err != nil {
		return Enrollment{}, err
	}
	now := s.now()
	tok := auth.NewSecret(auth.PrefixEnrollment, ulid.New(now))
	codes := make([]string, RecoveryCodeCount)
	rows := make([]store.RecoveryCode, RecoveryCodeCount)
	for i := range codes {
		codes[i] = auth.NewRecoveryCode()
		h, _ := auth.HashRecoveryCode(codes[i])
		rows[i] = store.RecoveryCode{ID: ulid.New(now), Hash: h}
	}
	expires := now.Add(ttl)
	err = s.db.IssueEnrollment(ctx, store.Enrollment{
		Token: store.EnrollmentToken{ID: tok.ID, PrincipalID: p.ID, SecretHash: tok.Hash[:], Reason: "enroll", ExpiresAt: expires},
		Codes: rows,
		// The CLI has no credential: running it on the host is the
		// highest authority there is (§13.7), so the system acts.
		Action: store.Action{
			ID: ulid.New(now), Actor: store.SystemPrincipalID, Tool: "auth.enroll.issue", At: now,
			Args: map[string]any{"principal_id": p.ID, "enrollment_token_id": tok.ID, "expires_at": expires.UTC(), "recovery_codes": len(rows)},
		},
	})
	if err != nil {
		return Enrollment{}, fmt.Errorf("enroll: %w", err)
	}
	// A fragment never reaches the server or a proxy log; the WebUI
	// page reads it and posts it to /auth/passkey/register/begin.
	return Enrollment{Principal: p, URL: s.origin + "/enroll#" + tok.Text, ExpiresAt: expires, RecoveryCodes: codes}, nil
}

func (s *Service) resolveHuman(ctx context.Context, name string) (store.Principal, error) {
	if name == "" {
		humans, err := s.db.PrincipalsOfKind(ctx, "human")
		if err != nil {
			return store.Principal{}, err
		}
		switch len(humans) {
		case 0:
			name = "operator"
		case 1:
			return humans[0], nil
		default:
			return store.Principal{}, fmt.Errorf("%d human principals exist; choose one with --name", len(humans))
		}
	}
	p, err := s.db.EnsurePrincipal(ctx, store.Principal{
		ID: ulid.New(s.now()), Kind: "human", Name: name, DisplayName: name, CreatedAt: s.now(),
	})
	if err != nil {
		return p, err
	}
	if p.DisabledAt != nil {
		return p, fmt.Errorf("principal human:%s is disabled", name)
	}
	return p, nil
}

// alertf sends a security notice; failure is logged, never fatal.
func (s *Service) alertf(ctx context.Context, format string, args ...any) {
	if s.alert == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := s.alert.Notify(ctx, "yuuka: "+fmt.Sprintf(format, args...)); err != nil {
		s.log.Error("auth: alert failed", "err", err)
	}
}

// record appends to the audit log; failure is logged. Security events
// are recorded before the response, so a lost write is visible in the log.
func (s *Service) record(ctx context.Context, a store.Action) {
	if a.ID == "" {
		a.At = s.now()
		a.ID = ulid.New(a.At)
	}
	if err := s.db.RecordAction(context.WithoutCancel(ctx), a); err != nil {
		s.log.Error("auth: cannot record action", "tool", a.Tool, "err", err)
	}
}
