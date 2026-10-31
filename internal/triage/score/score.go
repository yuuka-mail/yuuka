// Package score holds the types the §15 triage shares between the store,
// which keeps scores, and the scorer, which produces them. Types only:
// the store validates what it is given, the scorer decides what to ask.
// Neither imports the other.
package score

import "time"

// Score is one scoring run of one message: every question of one
// question set, answered by one scorer at one moment (§15.2). It is an
// observed fact, like deliveries.spf_recheck: the same mail scored again
// later, or by another model, may score differently. A new run is a new
// Score; nothing updates or deletes one.
type Score struct {
	// ID is a ULID of ScoredAt.
	ID        string
	MessageID string
	Profile
	// Model is the backend's model and version, as it reported them.
	Model string
	// Answers are by question key (the scorer's question set, e.g. kind,
	// phishing, injection, needs_reply).
	Answers  map[string]Answer
	Input    Input
	ScoredAt time.Time
}

// Profile names what produced a score: the scorer as configured and the
// version of its question set. Only scores of the active profile are
// current; a new model gets a new scorer name, a reworded question a new
// version, and mail scored under the old profile shows no current score
// until it is scored again.
type Profile struct {
	// Scorer is the configured name, ^[a-z0-9][a-z0-9.-]{0,31}$.
	Scorer string
	// Questions is the version of the question set, > 0.
	Questions int16
}

// Answer is the scorer's answer to one question: the probability of each
// option, or an abstention, never both. Question and option keys are
// ^[a-z][a-z0-9_]{0,31}$.
type Answer struct {
	P       map[string]float64 `json:"p,omitempty"`
	Abstain bool               `json:"abstain,omitempty"`
}

// Input records what the scorer was shown, never the content itself: the
// facts it was given, how much text, a hash of the text, and where it was
// sent. Facts change (a prefix claimed later), so a score keeps the ones
// it was computed from.
type Input struct {
	// Endpoint is EndpointLocal or EndpointHosted: whether the content
	// left the deployment (§15.6).
	Endpoint string `json:"endpoint"`
	// View and Parser are the agent view and parser versions the text
	// was rendered with (§8.1).
	View   int `json:"view"`
	Parser int `json:"parser"`
	// Chars is how many characters of text were sent; Truncated whether
	// the text was cut to fit.
	Chars     int  `json:"chars"`
	Truncated bool `json:"truncated"`
	// SHA256 is the lowercase hex SHA-256 of the text sent.
	SHA256 string `json:"sha256"`
	Facts  Facts  `json:"facts"`
}

// Facts are the system's facts about the message the scorer was given
// (the facts half of §8.1). Verdicts and booleans only: nothing here is
// text the sender wrote.
type Facts struct {
	// Auth is the agent view's authentication line (§8.1): the trusted
	// receiver's verdicts, or why there are none. At most 300 bytes.
	Auth string `json:"auth"`
	// Claimed: delivered to a claimed prefix.
	Claimed bool `json:"claimed"`
	// KnownSender: mail from the same sender reached this prefix before.
	KnownSender bool `json:"known_sender"`
	// ReplyToOther: Reply-To is in another domain than From.
	ReplyToOther bool `json:"reply_to_other"`
	// ListID: the message has a List-Id.
	ListID bool `json:"list_id"`
	// Class is the classifier's: unknown, machine or human.
	Class string `json:"class"`
	// HasCode and HasLink: the extractor found a code or a login link;
	// whether, never what.
	HasCode bool `json:"has_code"`
	HasLink bool `json:"has_link"`
}

// Input endpoints.
const (
	EndpointLocal  = "local"
	EndpointHosted = "hosted"
)
