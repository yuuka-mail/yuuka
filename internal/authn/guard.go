package authn

import (
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// Cookie names use the __Host- prefix: the browser then refuses them
// unless Secure, Path=/ and without Domain, so no sibling host can plant
// or overwrite them.
const (
	SessionCookie  = "__Host-yuuka-session"
	ceremonyCookie = "__Host-yuuka-ceremony"
)

// Require wraps h so it only runs for a request authenticated by a bearer
// token or a session cookie whose credential holds every scope in scopes.
// No credential, or an invalid one: 401. Missing scope: 403, recorded.
//
// A request with an Authorization header is judged by that header alone;
// a cookie next to it is ignored, so the two never mix. Cookie-borne
// requests with unsafe methods must also pass the cross-origin check
// (see Handler for the full CSRF story).
func (s *Service) Require(h http.Handler, scopes ...auth.Scope) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, ok := s.resolve(w, r, false)
		if !ok {
			return
		}
		if v == nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="yuuka"`)
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if missing := v.Missing(scopes...); len(missing) > 0 {
			s.record(r.Context(), store.Action{
				Actor: v.PrincipalID, CredentialID: v.CredentialID, Tool: "auth.deny",
				Args: map[string]any{"method": r.Method, "path": r.URL.Path, "missing": missing},
			})
			writeError(w, http.StatusForbidden, "missing scope")
			return
		}
		h.ServeHTTP(w, r.WithContext(auth.WithViewer(r.Context(), v)))
	})
}

// Optional wraps h so that it sees the Viewer if the request carries a
// valid credential and runs anonymously if it carries none. A bad bearer
// token is still a 401: a caller that presents one meant to be someone.
// A stale session cookie is cleared and ignored instead, so that an
// operator whose session expired can still use an enrollment link.
func (s *Service) Optional(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, ok := s.resolve(w, r, true)
		if !ok {
			return
		}
		if v != nil {
			r = r.WithContext(auth.WithViewer(r.Context(), v))
		}
		h.ServeHTTP(w, r)
	})
}

// resolve finds the request's viewer. It returns (nil, true) when the
// request carries no credential (or, with ignoreStaleCookie, an invalid
// session cookie), and ok=false after writing an error.
func (s *Service) resolve(w http.ResponseWriter, r *http.Request, ignoreStaleCookie bool) (*auth.Viewer, bool) {
	var kind, text string
	if h := r.Header.Get("Authorization"); h != "" {
		scheme, tok, found := strings.Cut(h, " ")
		if !found || !strings.EqualFold(scheme, "Bearer") {
			w.Header().Set("WWW-Authenticate", `Bearer realm="yuuka"`)
			writeError(w, http.StatusUnauthorized, "want a Bearer token")
			return nil, false
		}
		kind, text = auth.KindToken, strings.TrimSpace(tok)
	} else if c, err := r.Cookie(SessionCookie); err == nil {
		if err := s.csrf.Check(r); err != nil {
			writeError(w, http.StatusForbidden, "cross-origin request refused")
			return nil, false
		}
		kind, text = auth.KindSession, c.Value
	} else {
		return nil, true
	}
	v, err := s.Authenticate(r.Context(), kind, text)
	if err == nil {
		return v, true
	}
	if rej, isRej := errors.AsType[*rejection](err); isRej && s.rejectLog.allow(rej.credentialID) {
		s.record(r.Context(), store.Action{
			CredentialID: rej.credentialID, Tool: "auth.reject",
			Args: map[string]any{"reason": rej.reason, "method": r.Method, "path": r.URL.Path, "remote": remoteHost(r)},
		})
	}
	if !errors.Is(err, ErrUnauthenticated) {
		s.log.Error("auth: authenticate", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return nil, false
	}
	if kind == auth.KindSession {
		clearCookie(w, SessionCookie)
		if ignoreStaleCookie {
			return nil, true
		}
	} else {
		w.Header().Set("WWW-Authenticate", `Bearer realm="yuuka", error="invalid_token"`)
	}
	writeError(w, http.StatusUnauthorized, "invalid credential")
	return nil, false
}

// remoteHost is the peer address as the socket sees it. Forwarding
// headers are not trusted: nothing about the proxy in front is assumed.
func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: maxAge,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func clearCookie(w http.ResponseWriter, name string) { setCookie(w, name, "", -1) }
