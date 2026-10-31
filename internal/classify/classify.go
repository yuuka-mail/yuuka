// Package classify computes messages.class, the machine / human split of
// §3 principle 4, from content-derived signals. It is the built-in seed
// classifier; the rule engine (M3) and human overrides
// (message_state.class_override) sit on top of it and never write here.
//
// One signal is the extractor's (internal/extract): mail that is not a
// reply and carries a code or sign-in link the extractor is sure of is a
// notification. So the class depends on extract.Version too, and the
// backlog pass that re-extracts mail below it recomputes the class with
// it (store.Reextract); messages.extract_version says which version a
// row's class was computed with.
package classify

import (
	"regexp"
	"strings"
	"time"

	"github.com/yuuka-mail/yuuka/internal/extract"
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
	if reply(m) {
		return Human
	}
	return Unknown
}

// noreplyLocal matches the local parts of senders nobody answers.
var noreplyLocal = regexp.MustCompile(`^(no-?reply|do-?not-?reply|donotreply|mailer-daemon|postmaster|bounces?|notifications?|alerts?|newsletters?)([-+._].*)?$`)

// machinePrefix is the receiving-prefix naming convention of §3 principle
// 4: addresses handed out to services.
var machinePrefix = regexp.MustCompile(`^(svc|gh)-`)

// machineConfidence is how sure the extractor must be of a code or a
// sign-in link for the mail to count as a notification: a cue word right
// at the value (§3 principle 4's "verification code pattern").
const machineConfidence = 85

// reply reports the signs of a conversation. A person who answers or
// forwards mail with a code in it is still a person: the code signal is
// only read in mail that is none of these.
func reply(m mimeparse.Message) bool {
	return m.InReplyTo != "" || len(m.References) > 0 || replyPrefix.MatchString(m.Subject)
}

// deliversCode reports a one-time code or sign-in link the extractor is
// sure of. Its expiry is not needed, so no arrival time is passed.
func deliversCode(m mimeparse.Message) bool {
	for _, x := range extract.Message(m, time.Time{}) {
		if (x.Kind == extract.OTP || x.Kind == extract.Link) && x.Confidence >= machineConfidence {
			return true
		}
	}
	return false
}

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
	if !reply(m) && deliversCode(m) {
		return true
	}
	for _, p := range prefixes {
		if machinePrefix.MatchString(p) {
			return true
		}
	}
	return false
}
