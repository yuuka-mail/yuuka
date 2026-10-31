// Package authres extracts the authentication facts of one delivery from
// its raw message: deliveries.auth (§10.1.1).
//
// Authentication-Results is an ordinary header field that any sender can
// pre-write, and a catch-all accepts mail from anyone. So only the topmost
// Authentication-Results counts, and only if its authserv-id is the one the
// account trusts; the receiver prepends its own fields above everything the
// sender wrote. Anything else yields an untrusted Auth with no results: no
// judgement is better than one based on forged input.
//
// It reads the header block with its own field splitter rather than the
// MIME parser, so a message the parser quarantines still gets its
// authentication facts.
package authres

import (
	"bytes"
	"strings"
)

// Auth is the JSON stored in deliveries.auth.
type Auth struct {
	// Trusted is true when the topmost Authentication-Results carries the
	// account's trusted authserv-id. When false, every other field except
	// Reason is empty and the delivery counts as unverified.
	Trusted    bool   `json:"trusted"`
	Reason     string `json:"reason,omitempty"`
	AuthservID string `json:"authserv_id,omitempty"`
	// DMARC is the result of the first dmarc method, lowercased; the only
	// result that says whether the visible From is genuine (§3.3).
	DMARC string `json:"dmarc,omitempty"`
	// Results lists every method result in order (dmarc, spf for helo and
	// mailfrom, dkim per signature, arc ...) with their properties.
	Results []Result `json:"results,omitempty"`
	// RemoteIP is the first smtp.remote-ip property.
	RemoteIP string `json:"remote_ip,omitempty"`
	// CFSpamHScore is X-CF-SpamH-Score verbatim. Its scale is undocumented
	// (§4.3), so it is kept as a signal and not interpreted yet.
	CFSpamHScore string `json:"cf_spamh_score,omitempty"`
}

// Result is one resinfo of RFC 8601.
type Result struct {
	Method string            `json:"method"`
	Result string            `json:"result"`
	Props  map[string]string `json:"props,omitempty"`
}

// Reasons for an untrusted Auth.
const (
	ReasonNoTrustedID = "no trusted authserv-id configured"
	ReasonMissing     = "no Authentication-Results"
	ReasonOtherID     = "topmost Authentication-Results is not from the trusted receiver"
)

// Extract returns the authentication facts of raw for an account whose
// trusted receiver uses authserv-id trustedID.
func Extract(raw []byte, trustedID string) Auth {
	if trustedID == "" {
		return Auth{Reason: ReasonNoTrustedID}
	}
	fs := headerFields(raw)
	top := -1
	for i, f := range fs {
		if strings.EqualFold(f.name, "Authentication-Results") {
			top = i
			break
		}
	}
	if top < 0 {
		return Auth{Reason: ReasonMissing}
	}
	id, results := parse(fs[top].value)
	if !strings.EqualFold(id, trustedID) {
		return Auth{Reason: ReasonOtherID}
	}
	a := Auth{Trusted: true, AuthservID: strings.ToLower(id), Results: results}
	for _, r := range results {
		if r.Method == "dmarc" && a.DMARC == "" {
			a.DMARC = r.Result
		}
		if ip := r.Props["smtp.remote-ip"]; ip != "" && a.RemoteIP == "" {
			a.RemoteIP = ip
		}
	}
	a.CFSpamHScore = receiverField(fs, "X-CF-SpamH-Score")
	return a
}

// receiverBlock names the fields a receiver prepends (§10.1.1 measured the
// Cloudflare set). The leading run of such fields is the receiver's own.
var receiverBlock = map[string]bool{
	"received":                   true,
	"received-spf":               true,
	"authentication-results":     true,
	"arc-seal":                   true,
	"arc-message-signature":      true,
	"arc-authentication-results": true,
	"x-cf-spamh-score":           true,
}

// receiverField returns the value of the first field called name within
// the leading run of receiver fields, and only there: the same name
// further down was written by the sender. A sender whose own header block
// starts with that field is indistinguishable when the receiver omitted
// it, which is acceptable for a signal that decides nothing on its own.
func receiverField(fs []field, name string) string {
	receivedSeen := false
	for _, f := range fs {
		n := strings.ToLower(f.name)
		if !receiverBlock[n] {
			return ""
		}
		if n == "received" {
			// The receiver adds exactly one Received; the next one is the
			// sender's.
			if receivedSeen {
				return ""
			}
			receivedSeen = true
		}
		if strings.EqualFold(f.name, name) {
			return strings.TrimSpace(f.value)
		}
	}
	return ""
}

// parse splits an Authentication-Results value into its authserv-id and
// results (RFC 8601 §2.2). It is lenient: unparseable pieces are skipped,
// never guessed at.
//
// The grammar allows CFWS around "=" and around a method's "/" version,
// so a result is not one whitespace-separated word; a quoted value may
// hold spaces; and an unquoted value ends only at whitespace, because
// receivers write header.b and similar base64 values unquoted, "=" and
// "/" included. go-msgauth's authres splits on whitespace and keeps only
// a few properties per method, which is why this parser stays.
func parse(v string) (string, []Result) {
	segs := split(stripComments(v), ';')
	// authserv-id [ CFWS authres-version ]
	id, _ := value(segs[0], skipSpace(segs[0], 0))
	var out []Result
	for _, seg := range segs[1:] {
		method, result, i, ok := pair(seg, skipSpace(seg, 0))
		if !ok {
			continue // "none" or garbage
		}
		method, _, _ = strings.Cut(method, "/") // method version
		r := Result{Method: strings.ToLower(method), Result: strings.ToLower(result)}
		for i < len(seg) {
			k, val, next, ok := pair(seg, i)
			i = next
			if !ok {
				continue
			}
			if r.Props == nil {
				r.Props = map[string]string{}
			}
			if _, dup := r.Props[strings.ToLower(k)]; !dup {
				r.Props[strings.ToLower(k)] = val
			}
		}
		out = append(out, r)
	}
	return id, out
}

// pair reads key [CFWS] ["/" [CFWS] version [CFWS]] "=" [CFWS] value at
// s[i:], comments already blanked, and returns the index after it and any
// trailing space. A key without "=" is a stray word: ok is false and next
// skips just that word.
func pair(s string, i int) (key, val string, next int, ok bool) {
	start := i
	for i < len(s) && !isSpace(s[i]) && s[i] != '=' && s[i] != '/' {
		i++
	}
	end := i
	i = skipSpace(s, i)
	if i < len(s) && s[i] == '/' {
		i = skipSpace(s, i+1)
		for i < len(s) && !isSpace(s[i]) && s[i] != '=' {
			i++
		}
		i = skipSpace(s, i)
	}
	if end == start || i >= len(s) || s[i] != '=' {
		return "", "", skipSpace(s, max(end, start+1)), false
	}
	val, i = value(s, skipSpace(s, i+1))
	return s[start:end], val, skipSpace(s, i), true
}

// value reads a quoted-string, unescaping quoted-pairs, or else everything
// up to the next whitespace.
func value(s string, i int) (string, int) {
	if i < len(s) && s[i] == '"' {
		var b strings.Builder
		for i++; i < len(s) && s[i] != '"'; i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
			}
			b.WriteByte(s[i])
		}
		return b.String(), min(i+1, len(s))
	}
	start := i
	for i < len(s) && !isSpace(s[i]) {
		i++
	}
	return s[start:i], i
}

func skipSpace(s string, i int) int {
	for i < len(s) && isSpace(s[i]) {
		i++
	}
	return i
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

// stripComments removes RFC 5322 comments (nested, with quoted-pairs),
// leaving quoted strings intact.
func stripComments(s string) string {
	var b strings.Builder
	depth := 0
	quoted := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && (quoted || depth > 0):
			if depth == 0 {
				b.WriteByte(c)
				b.WriteByte(s[i+1])
			}
			i++
		case quoted:
			b.WriteByte(c)
			if c == '"' {
				quoted = false
			}
		case c == '(':
			depth++
		case c == ')' && depth > 0:
			depth--
			if depth == 0 {
				b.WriteByte(' ')
			}
		case depth > 0:
		case c == '"':
			quoted = true
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// split splits s at sep outside quoted strings.
func split(s string, sep byte) []string {
	var out []string
	quoted := false
	start := 0
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\' && quoted:
			i++
		case s[i] == '"':
			quoted = !quoted
		case s[i] == sep && !quoted:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

type field struct{ name, value string }

// headerFields splits the header block into unfolded fields in order. Like
// contenthash, it only knows field boundaries: a line starting with SP or
// HTAB continues the field above; lines without a colon are skipped.
func headerFields(raw []byte) []field {
	var fs []field
	rest := raw
	for len(rest) > 0 {
		var line []byte
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i+1], rest[i+1:]
		} else {
			line, rest = rest, nil
		}
		line = bytes.TrimRight(line, "\r\n")
		if len(line) == 0 {
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			if len(fs) > 0 {
				fs[len(fs)-1].value += " " + strings.TrimSpace(string(line))
			}
			continue
		}
		name, value, ok := bytes.Cut(line, []byte(":"))
		if !ok {
			// Not a field; it must not extend the previous one either.
			fs = append(fs, field{})
			continue
		}
		fs = append(fs, field{
			name:  strings.TrimRight(string(name), " \t"),
			value: strings.TrimSpace(string(value)),
		})
	}
	return fs
}
