// Package triage scores incoming mail with a decision model (§15): it
// builds what the model sees from the agent view, asks the question set
// over the System One protocol (POST /v1/systemone), validates the
// answers and writes them through the store as observations. It runs
// in serve, after ingest and never inside it, and nothing waits on it.
// v1 observes only (§15.4): a score is shown and acts on nothing.
package triage

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// QuestionsVersion is the version of Questions. Any change of wording,
// options or type is a new version: scores of different versions are
// never compared, and the scores of the old one stop being current.
const QuestionsVersion = 2

// The question keys of set v2 (§15.3). v1 also asked "injection"
// (instructions addressed to an AI agent); v2 drops it, since the score
// moved nothing and told an agent less than whose words it is reading.
const (
	Kind       = "kind"
	Phishing   = "phishing"
	NeedsReply = "needs_reply"
)

// The options of v2. A yes/no answer is stored as Yes and No.
const (
	Yes = "yes"
	No  = "no"

	KindNotification = "notification"
	KindConversation = "conversation"
	KindMarketing    = "marketing"
	KindSpam         = "spam"
)

// QuestionType is the System One type of a question.
type QuestionType string

const (
	// Choice picks one of the options.
	Choice QuestionType = "choice"
	// YesNo is System One's "noul": the probability of true.
	YesNo QuestionType = "noul"
)

// Question is one question of the set, as the model is asked it.
type Question struct {
	Key          string
	Type         QuestionType
	Instructions string
	// Criteria describe each option of a Choice, or for YesNo what true
	// and false mean.
	Criteria map[string]string
}

// Questions is set v2 (§15.3). That the facts come from yuuka and the
// content from the sender is said in the state itself (State), so that
// the questions stay about the mail.
var Questions = []Question{
	{
		Key:          Kind,
		Type:         Choice,
		Instructions: "What kind of email is this?",
		Criteria: map[string]string{
			KindNotification: "Sent by a system about the recipient's own account or activity: sign-in codes, receipts, alerts, shipping, security notices.",
			KindConversation: "Written by a person to the recipient, to be read and possibly answered by a person.",
			KindMarketing:    "Promotion or newsletter from an organisation the recipient deals with.",
			KindSpam:         "Unsolicited bulk email the recipient never asked for, or a scam.",
		},
	},
	{
		Key:          Phishing,
		Type:         YesNo,
		Instructions: "Does this email impersonate someone in order to make the recipient click a link, give credentials or personal data, or pay?",
		Criteria: map[string]string{
			"true":  "It impersonates a person or organisation to obtain a click, credentials, data or a payment.",
			"false": "It does not impersonate anyone for such a purpose.",
		},
	},
	{
		Key:          NeedsReply,
		Type:         YesNo,
		Instructions: "Does this email need a personal reply from the recipient?",
		Criteria: map[string]string{
			"true":  "Someone expects the recipient to answer it personally.",
			"false": "No personal reply is expected.",
		},
	},
}

// Options are the options of question key in the set, as scores store
// them, or nil when the set has no such question.
func Options(key string) []string {
	for _, q := range Questions {
		if q.Key != key {
			continue
		}
		if q.Type == YesNo {
			return []string{Yes, No}
		}
		return slices.Sorted(maps.Keys(q.Criteria))
	}
	return nil
}

// Order is the questions of answers in the set's order, then any the
// set does not have (a backend's extra, or a stale profile) by name.
func Order[V any](answers map[string]V) []string {
	var keys []string
	for _, q := range Questions {
		keys = append(keys, q.Key)
	}
	for _, k := range slices.Sorted(maps.Keys(answers)) {
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	return keys
}

// ParseFilter reads a filter on what a score leans to, written
// <question>:<option> (phishing:yes), against the set: a question or
// option it does not have is an error, not a filter matching nothing.
func ParseFilter(s string) (question, option string, err error) {
	question, option, ok := strings.Cut(s, ":")
	if !ok || question == "" || option == "" {
		return "", "", errors.New("want <question>:<option>, such as phishing:yes or kind:spam")
	}
	opts := Options(question)
	if opts == nil {
		keys := make([]string, len(Questions))
		for i, q := range Questions {
			keys[i] = q.Key
		}
		return "", "", fmt.Errorf("no question %q; want one of %s", question, strings.Join(keys, ", "))
	}
	if !slices.Contains(opts, option) {
		return "", "", fmt.Errorf("%s has no option %q; want one of %s", question, option, strings.Join(opts, ", "))
	}
	return question, option, nil
}
