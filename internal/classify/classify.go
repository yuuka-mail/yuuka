// Package classify computes messages.class, the machine / human split of
// §3 principle 4, from content-derived signals. It is the built-in seed
// classifier; the rule engine (M3) and human overrides
// (message_state.class_override) sit on top of it and never write here.
package classify

import (
	"regexp"
	"strings"

	"github.com/yuuka-mail/yuuka/internal/mimeparse"
)

// Class is messages.class. The values are stored; do not renumber.
type Class int16

const (
	Unknown Class = 0
	Machine Class = 1
	Human   Class = 2
)

func (c Class) String() string {
	switch c {
	case Machine:
		return "machine"
	case Human:
		return "human"
	}
	return "unknown"
}

// Of classifies a parsed message delivered to prefixes (the local parts of
// all its deliveries). Any machine signal wins. Human needs positive
// evidence of a conversation; everything else is unknown, which is the
// honest answer for a first mail from a stranger.
func Of(m mimeparse.Message, prefixes []string) Class {
	if machine(m, prefixes) {
		return Machine
	}
	if m.InReplyTo != "" || len(m.References) > 0 || replyPrefix.MatchString(m.Subject) {
		return Human
	}
	return Unknown
}

// noreplyLocal matches the local parts of senders nobody answers.
var noreplyLocal = regexp.MustCompile(`^(no-?reply|do-?not-?reply|donotreply|mailer-daemon|postmaster|bounces?|notifications?|alerts?|newsletters?)([-+._].*)?$`)

// machinePrefix is the receiving-prefix naming convention of §3 principle
// 4: addresses handed out to services.
var machinePrefix = regexp.MustCompile(`^(svc|gh)-`)

// otpKeyword and otpCode together are the "verification code pattern".
var (
	otpKeyword = regexp.MustCompile(`(?i)verification code|security code|one-time|passcode|\bOTP\b|\bcode\b|验证码|校验码|动态码|确认码`)
	otpCode    = regexp.MustCompile(`(^|[^0-9])[0-9]{4,8}([^0-9]|$)`)
)

var replyPrefix = regexp.MustCompile(`(?i)^\s*(re|fwd?|aw|wg|sv|vs)\s*:|^\s*(回复|答复|转发)\s*[:：]`)

func machine(m mimeparse.Message, prefixes []string) bool {
	if m.ListUnsubscribe || m.ListID != "" {
		return true
	}
	if m.AutoSubmitted != "" && m.AutoSubmitted != "no" {
		return true
	}
	switch m.Precedence {
	case "bulk", "list", "junk", "auto_reply":
		return true
	}
	if m.MediaType == "multipart/report" {
		return true // DSN / MDN
	}
	if local, _, ok := strings.Cut(m.FromAddr, "@"); ok && noreplyLocal.MatchString(local) {
		return true
	}
	if m.InReplyTo == "" {
		text := m.Subject + "\n" + m.Snippet
		if otpKeyword.MatchString(text) && otpCode.MatchString(text) {
			return true
		}
	}
	for _, p := range prefixes {
		if machinePrefix.MatchString(p) {
			return true
		}
	}
	return false
}
