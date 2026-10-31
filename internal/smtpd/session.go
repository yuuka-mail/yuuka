package smtpd

import (
	"cmp"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authn"
	"github.com/yuuka-mail/yuuka/internal/mailnet"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Replies of this package's own. go-smtp's ErrAuthRequired is 502;
// RFC 4954 says 530.
var (
	errAuthRequired = &smtp.SMTPError{Code: 530, EnhancedCode: smtp.EnhancedCode{5, 7, 0}, Message: "Authentication required"}
	errThrottled    = &smtp.SMTPError{Code: 454, EnhancedCode: smtp.EnhancedCode{4, 7, 0}, Message: "Too many failed authentications, try again later"}
	errAuthTemp     = &smtp.SMTPError{Code: 454, EnhancedCode: smtp.EnhancedCode{4, 7, 0}, Message: "Temporary authentication failure"}
	// A credential revoked or expired while its session was open. 421
	// makes the client hang up and keep the message; reconnecting, it
	// gets 535 at AUTH, which is where a broken credential belongs.
	errNoLongerValid = &smtp.SMTPError{Code: 421, EnhancedCode: smtp.EnhancedCode{4, 7, 0}, Message: "Credential no longer valid, closing connection"}
	// RFC 6531 §3.7.4.1: without SMTPUTF8 a non-ASCII address is 553 5.6.7.
	errNeedsUTF8 = &smtp.SMTPError{Code: 553, EnhancedCode: smtp.EnhancedCode{5, 6, 7}, Message: "Non-ASCII address needs SMTPUTF8, which is not offered"}
	errTemporary = &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Temporary failure, try again later"}
)

// session is one connection from EHLO to its close.
type session struct {
	s      *Server
	remote string // client address, after any PROXY header
	proxy  string // the proxy it came through, "" if direct
	key    string // what limits count it under

	// mu guards the rest: with BDAT, go-smtp runs Data in a goroutine
	// of its own, next to the one that may call Reset.
	mu       sync.Mutex
	v        *auth.Viewer
	user     string
	password string // kept to recheck the credential at every MAIL
	revoked  bool
	from     string
	rcpts    []string
}

func (s *Server) newSession(c *smtp.Conn) (smtp.Session, error) {
	remote, key := clientOf(c.Conn().RemoteAddr())
	ss := &session{s: s, remote: remote, key: key}
	if p := mailnet.Proxy(c.Conn()); p != nil {
		ss.proxy, _ = clientOf(p)
	}
	return ss, nil
}

// args are the audit fields every entry of a session carries.
func (ss *session) args(extra map[string]any) map[string]any {
	a := map[string]any{"via": via, "remote": ss.remote}
	if ss.proxy != "" {
		a["proxy"] = ss.proxy
	}
	for k, v := range extra {
		a[k] = v
	}
	return a
}

// AuthMechanisms is PLAIN alone. LOGIN is obsolete (it never left draft
// status) and go-sasl has no server side for it; every client that would
// use this port speaks PLAIN, and the Cloudflare exit's own use of PLAIN
// shows the common libraries do. Add LOGIN when a real client needs it.
func (ss *session) AuthMechanisms() []string { return []string{sasl.Plain} }

func (ss *session) Auth(mech string) (sasl.Server, error) {
	if mech != sasl.Plain {
		return nil, smtp.ErrAuthUnknownMechanism
	}
	return sasl.NewPlainServer(ss.authenticate), nil
}

// authenticate checks a PLAIN exchange: the user name is the credential
// id, the password the SMTP password (§13.3).
func (ss *session) authenticate(identity, user, password string) error {
	s := ss.s
	if s.authFl.empty(ss.key) {
		if s.logFl.allow("auth " + ss.key) {
			s.record(store.Action{Tool: "smtp.auth.reject", Args: ss.args(map[string]any{"reason": "throttled"})})
		}
		return errThrottled
	}
	reject := func(reason string, v *auth.Viewer) error {
		s.authFl.allow(ss.key)
		extra := map[string]any{"reason": reason}
		// Only something shaped like a credential id: a client that
		// swapped its fields must not put its password in the log.
		if ulid.Valid(user) {
			extra["user"] = user
		}
		a := store.Action{Tool: "smtp.auth.reject", Args: ss.args(extra)}
		if v != nil {
			a.Actor, a.CredentialID = v.PrincipalID, v.CredentialID
		}
		s.record(a)
		return smtp.ErrAuthFailed
	}
	// No proxy authorization: an authorization identity, if given, must
	// be the user itself.
	if identity != "" && identity != user {
		return reject("authorization identity", nil)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	v, err := s.cfg.Auth.AuthenticateSMTP(ctx, user, password, via)
	switch {
	case errors.Is(err, authn.ErrUnauthenticated):
		return reject("bad credentials", nil)
	case err != nil:
		s.cfg.Log.Error("smtp: authenticate", "remote", ss.remote, "err", err)
		return errAuthTemp
	case !v.Has(auth.SubmissionSend):
		// An SMTP password is issued with exactly this scope (§13.3);
		// checked here too, so the port does not rest on that alone.
		return reject("missing scope "+string(auth.SubmissionSend), v)
	}
	ss.mu.Lock()
	ss.v, ss.user, ss.password = v, user, password
	ss.mu.Unlock()
	s.record(store.Action{Actor: v.PrincipalID, CredentialID: v.CredentialID, Tool: "smtp.auth", Args: ss.args(nil)})
	return nil
}

// Mail starts a transaction. The credential is checked again, so that a
// revocation reaches sessions already open (§13.3: revocation takes
// effect at once).
func (ss *session) Mail(from string, _ *smtp.MailOptions) error {
	ss.mu.Lock()
	user, password, revoked, had := ss.user, ss.password, ss.revoked, ss.v != nil
	ss.mu.Unlock()
	switch {
	case revoked:
		return errNoLongerValid
	case !had:
		return errAuthRequired
	case !ascii(from):
		return errNeedsUTF8
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	v, err := ss.s.cfg.Auth.AuthenticateSMTP(ctx, user, password, via)
	if err != nil && !errors.Is(err, authn.ErrUnauthenticated) {
		ss.s.cfg.Log.Error("smtp: recheck credential", "remote", ss.remote, "err", err)
		return errTemporary
	}
	ss.mu.Lock()
	old := ss.v
	if err == nil && v.Has(auth.SubmissionSend) {
		ss.v, ss.from, ss.rcpts = v, from, nil
		ss.mu.Unlock()
		return nil
	}
	ss.revoked = true
	ss.mu.Unlock()
	ss.s.record(store.Action{
		Actor: old.PrincipalID, CredentialID: old.CredentialID, Tool: "smtp.auth.reject",
		Args: ss.args(map[string]any{"reason": "no longer valid"}),
	})
	return errNoLongerValid
}

func (ss *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	if !ascii(to) {
		return errNeedsUTF8
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	// go-smtp enforces MaxRecipients before calling here (452 4.5.3,
	// which tells the client to send the rest in another transaction).
	ss.rcpts = append(ss.rcpts, to)
	return nil
}

// Data reads the message and hands it to the core. Only its answer
// decides the reply: 250 means the core has the message, held or not.
func (ss *session) Data(r io.Reader) error {
	raw, err := io.ReadAll(r)
	ss.mu.Lock()
	v, from, rcpts := ss.v, ss.from, slices.Clone(ss.rcpts)
	ss.mu.Unlock()
	audit := store.Action{
		Actor: v.PrincipalID, CredentialID: v.CredentialID, Tool: "smtp.submit",
		Args: ss.args(map[string]any{"mail_from": from, "rcpts": len(rcpts), "bytes": len(raw)}),
	}
	if err != nil {
		// Too large (552), or the client went away or reset mid-BDAT:
		// nothing reached the core either way.
		if errors.Is(err, smtp.ErrDataTooLarge) {
			audit.Result = map[string]any{"code": smtp.ErrDataTooLarge.Code}
			ss.s.record(audit)
		}
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := ss.s.cfg.Submitter.SubmitRaw(ctx, v, from, rcpts, raw)
	rep := reply(res, err)
	result := map[string]any{"code": rep.Code}
	for k, val := range map[string]string{"outbox_id": res.OutboxID, "draft_id": res.DraftID, "hold_reason": res.HoldReason, "detail": res.Detail} {
		if val != "" {
			result[k] = val
		}
	}
	if err != nil {
		result["error"] = err.Error()
		if rep.Code != 550 {
			ss.s.cfg.Log.Error("smtp: submit", "remote", ss.remote, "principal", v.String(), "err", err)
		}
	}
	audit.Result = result
	ss.s.record(audit)
	return rep
}

// reply maps the core's answer to the SMTP reply. Mail kept as a draft or
// held for review is a 250 like queued mail: holding it is the policy
// working (§13.5), and a client that saw an error would retry and pile up
// copies. The text names the id so the operator can find it.
func reply(res Result, err error) *smtp.SMTPError {
	switch {
	case errors.Is(err, ErrRefused):
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: printable(err.Error())}
	case err != nil:
		return errTemporary
	}
	var msg string
	switch {
	case res.Queued:
		msg = "Queued as " + res.OutboxID
	case res.OutboxID != "":
		msg = "Held for review as " + res.OutboxID
	case res.DraftID != "":
		msg = "Kept as draft " + res.DraftID
	default:
		msg = "Accepted"
	}
	if why := cmp.Or(res.HoldReason, res.Detail); why != "" && !res.Queued {
		msg += ": " + why
	}
	// go-smtp writes whatever code Data returns; this is how a 250
	// carries its own text.
	return &smtp.SMTPError{Code: 250, EnhancedCode: smtp.EnhancedCode{2, 0, 0}, Message: printable(msg)}
}

// printable makes text safe for one reply line: printable ASCII only,
// at most 300 bytes. The core's error text names addresses and reasons;
// nothing in it may break the line or the protocol.
func printable(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= 300 {
			break
		}
		if r < 0x20 || r > 0x7e {
			r = '?'
		}
		b.WriteRune(r)
	}
	return b.String()
}

func ascii(s string) bool {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func (ss *session) Reset() {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.from, ss.rcpts = "", nil
}

func (ss *session) Logout() error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.password = ""
	return nil
}
