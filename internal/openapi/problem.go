// Package openapi describes yuuka's HTTP API as an OpenAPI 3.1 document
// derived from Go types (§5.1), and holds what every endpoint in it
// shares: the problem+json error body and the constraint tags that are
// both written into the schema and enforced at runtime.
//
// Schemas come from github.com/google/jsonschema-go, the library the MCP
// tools' schemas come from, with the same options (Options), so REST and
// MCP cannot disagree about a type.
package openapi

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Code is the stable, machine-readable kind of a Problem. Clients branch
// on it, never on the detail text; once released a code is not renamed.
type Code string

const (
	CodeInvalidParam    Code = "invalid_param"    // a path or query parameter
	CodeInvalidBody     Code = "invalid_body"     // the JSON body
	CodeUnsupportedType Code = "unsupported_type" // the body is not application/json
	CodeBodyTooLarge    Code = "body_too_large"
	CodeNotFound        Code = "not_found"
	CodeUnauthenticated Code = "unauthenticated" // no credential, or an invalid one
	CodeScopeRequired   Code = "scope_required"  // the credential lacks a scope
	CodeForbidden       Code = "forbidden"       // refused for another reason, e.g. cross-origin
	CodeConflict        Code = "conflict"
	CodeNotVerified     Code = "not_verified"     // a passkey ceremony failed verification
	CodeCeremonyExpired Code = "ceremony_expired" // no ceremony in progress; begin again
	CodeRateLimited     Code = "rate_limited"
	CodeUndecodable     Code = "undecodable" // stored mail that cannot be decoded as asked
	CodeUnavailable     Code = "unavailable" // busy; retry later
	// CodeNoIdentity: no identity covers the From address asked for.
	CodeNoIdentity Code = "no_identity"
	// CodeSubmissionRejected: the submission core refused outright. A
	// submission beyond its grant is not this: it is held, a success.
	CodeSubmissionRejected Code = "submission_rejected"
	// CodeInvalidRule: a rule or a proposal does not validate (§8.5);
	// Problem.Path says where.
	CodeInvalidRule Code = "invalid_rule"
	// CodeCursorExpired: a change cursor this log did not issue, or no
	// longer can continue from (§8.6); start again without one.
	CodeCursorExpired Code = "cursor_expired"
	// CodeWideningRequiresCLI: a tightening would let someone do more
	// than now, so nothing changed. The detail names the host command;
	// a person's session can also make the change through the widening
	// endpoints once elevated (§13.10). The name predates those.
	CodeWideningRequiresCLI Code = "widening_requires_cli"
	// CodeUnsupportedFilter: a search query names a condition JMAP
	// defines that yuuka reserves but does not support yet (§16.2).
	CodeUnsupportedFilter Code = "unsupported_filter"
	// CodeElevationRequired: the change widens who may do what, which
	// takes a person's session that asserted its passkey again in the
	// last few minutes (/auth/passkey/elevate/begin and finish); then
	// retry (§13.10).
	CodeElevationRequired Code = "elevation_required"
	// CodeSecretsLocked: the change seals or uses a third-party secret,
	// and the server has no key to do it with (§17.3). Reads, receiving
	// and everything else still work.
	CodeSecretsLocked Code = "secrets_locked"
	CodeInternal      Code = "internal"
)

// Enum lists every code: the schema says which codes exist.
func (Code) Enum() []string {
	return []string{
		"invalid_param", "invalid_body", "unsupported_type", "body_too_large", "not_found",
		"unauthenticated", "scope_required", "forbidden", "conflict", "not_verified", "ceremony_expired", "rate_limited",
		"undecodable", "unavailable", "no_identity", "submission_rejected", "invalid_rule", "cursor_expired",
		"widening_requires_cli", "unsupported_filter", "elevation_required", "secrets_locked", "internal",
	}
}

// Problem is the body of every error response, an RFC 9457 problem
// details object with the extension member code. Its type is always the
// default about:blank, so Title is the status text and Code tells
// problems apart.
type Problem struct {
	Title  string `json:"title"`
	Status int    `json:"status"`
	Code   Code   `json:"code"`
	// Detail is for people: what was wrong and, where it helps, what to
	// send instead.
	Detail string `json:"detail,omitempty"`
	// Path is where in the request body an invalid_rule problem is,
	// such as match.all[1].value or actions[0].label.
	Path string `json:"path,omitempty"`
}

func (p *Problem) Error() string { return string(p.Code) + ": " + p.Detail }

// Errorf returns a Problem with status, code and a formatted detail.
func Errorf(status int, code Code, format string, args ...any) *Problem {
	return &Problem{Title: http.StatusText(status), Status: status, Code: code, Detail: fmt.Sprintf(format, args...)}
}

// Write sends p as application/problem+json.
func Write(w http.ResponseWriter, p *Problem) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}
