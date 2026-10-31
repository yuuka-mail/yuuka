package agentview

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/yuuka-mail/yuuka/internal/classify"
	"github.com/yuuka-mail/yuuka/internal/rules"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// The shapes of annotations, rules and proposals (§8.5) that the REST
// API and the MCP tools share, declared once like Class. Every action
// here is an annotation: a label, a class, archived, or for a rule how
// arriving mail notifies. Nothing outbound can be written in these
// types, and the store and the tables refuse it again.

// RuleMatch is a rule's match tree as stored (§8.5): one node, exactly
// one of {"all":[…]}, {"any":[…]}, {"not":{…}} or a condition
// {"field","op","value"}. Its fields and operators are a closed set
// (internal/rules); the server validates the tree and says where it is
// wrong. The schema says only "object" because the tree is recursive.
type RuleMatch map[string]any

// RuleActionKind is what a rule or a proposal does to mail: annotations
// only, never anything outbound.
type RuleActionKind string

func (RuleActionKind) Enum() []string { return []string{"label", "classify", "archive", "notify"} }

// RuleClass is the class a classify action sets.
type RuleClass string

func (RuleClass) Enum() []string { return []string{"machine", "human"} }

// NotifyLevel is how a rule lets arriving mail notify. A rule can only
// quiet a notification, never cause one: downgraded mail is never pushed.
type NotifyLevel string

func (NotifyLevel) Enum() []string { return []string{"off", "silent", "normal"} }

// RuleAction is one action of a rule or a proposal. Only the parameter
// of its kind is set.
type RuleAction struct {
	Kind  RuleActionKind `json:"kind"`
	Label string         `json:"label,omitempty" jsonschema:"label: the label to add; exact and case-sensitive, at most 64 characters"`
	Class RuleClass      `json:"class,omitempty" jsonschema:"classify: the class to set"`
	Level NotifyLevel    `json:"level,omitempty" jsonschema:"notify (rules only): off, silent or normal"`
}

// AnnotateKind is one change an annotation asks for. Unlike a rule's,
// annotations can take back: unlabel, unarchive, and unclassify, which
// returns the message to the classifier's class.
type AnnotateKind string

func (AnnotateKind) Enum() []string {
	return []string{"label", "unlabel", "archive", "unarchive", "classify", "unclassify"}
}

// Annotation is one change to a message's labels, class or archived
// state.
type Annotation struct {
	Kind  AnnotateKind `json:"kind"`
	Label string       `json:"label,omitempty" jsonschema:"label and unlabel: the label; exact and case-sensitive, at most 64 characters"`
	Class Class        `json:"class,omitempty" jsonschema:"classify: the class to set"`
}

// AnnotateScope is what an annotation covers: the message, or every
// message of its thread the caller can see.
type AnnotateScope string

func (AnnotateScope) Enum() []string { return []string{"message", "thread"} }

// AnnotateOutcome says what became of one Annotation. overruled: a
// writer of higher rank set it last (a person over an agent, an agent
// over a rule), so it was left alone; that is a normal result, not an
// error. labels_full: the message has 64 labels already.
type AnnotateOutcome string

func (AnnotateOutcome) Enum() []string {
	return []string{"applied", "unchanged", "overruled", "labels_full"}
}

// ProposalKind is what a proposal suggests: annotations for one message
// or its thread, or a whole rule.
type ProposalKind string

func (ProposalKind) Enum() []string { return []string{"annotate", "rule"} }

// ProposalState is where a proposal stands. expired is a pending one
// past its expires_at; nobody decided it.
type ProposalState string

func (ProposalState) Enum() []string {
	return []string{"pending", "accepted", "rejected", "withdrawn", "expired"}
}

// StoreAnnotations turns asked changes into the store's, in order. The
// error names the offending entry; the store validates the values.
func StoreAnnotations(as []Annotation) ([]store.Annotation, error) {
	if len(as) == 0 {
		return nil, errors.New("actions: at least one")
	}
	out := make([]store.Annotation, len(as))
	for i, a := range as {
		switch a.Kind {
		case "label", "unlabel":
			out[i] = store.SetLabel(a.Label, a.Kind == "label")
		case "archive", "unarchive":
			out[i] = store.SetArchived(a.Kind == "archive")
		case "classify":
			c, ok := classes[a.Class]
			if !ok {
				return nil, fmt.Errorf("actions[%d].class: classify needs unknown, machine or human", i)
			}
			out[i] = store.SetClass(c)
		case "unclassify":
			out[i] = store.ClearClass()
		default:
			return nil, fmt.Errorf("actions[%d].kind: %q is not an annotation", i, a.Kind)
		}
		if (a.Label != "") != (a.Kind == "label" || a.Kind == "unlabel") || a.Class != "" && a.Kind != "classify" {
			return nil, fmt.Errorf("actions[%d]: %s takes only its own parameter", i, a.Kind)
		}
	}
	return out, nil
}

var classes = map[Class]classify.Class{ClassUnknown: classify.Unknown, ClassMachine: classify.Machine, ClassHuman: classify.Human}

// AnnotationOf is the change a stored annotation (attr, label, to) made.
func AnnotationOf(attr, label string, to *int16) Annotation {
	on := to != nil && *to == 1
	switch attr {
	case store.AttrLabel:
		if on {
			return Annotation{Kind: "label", Label: label}
		}
		return Annotation{Kind: "unlabel", Label: label}
	case store.AttrArchived:
		if on {
			return Annotation{Kind: "archive"}
		}
		return Annotation{Kind: "unarchive"}
	}
	if to == nil {
		return Annotation{Kind: "unclassify"}
	}
	return Annotation{Kind: "classify", Class: Class(classify.Class(*to).String())}
}

// RuleSpec is the stored form of a rule, {"v":1,"match":…,"actions":…},
// for the store to validate: the paths of its errors are those of the
// match and actions given.
func RuleSpec(m RuleMatch, as []RuleAction) []byte {
	b, _ := json.Marshal(struct {
		V       int          `json:"v"`
		Match   RuleMatch    `json:"match"`
		Actions []RuleAction `json:"actions"`
	}{rules.Version, m, nonNilActions(as)})
	return b
}

func nonNilActions(as []RuleAction) []RuleAction {
	if as == nil {
		return []RuleAction{}
	}
	return as
}

// ProposalPayload is the stored payload of a proposal of kind: a rule's
// spec (RuleSpec), or an annotate proposal's {"v":1,"scope":…,"actions":…}
// with scope message by default. The store validates it.
func ProposalPayload(kind ProposalKind, scope AnnotateScope, m RuleMatch, as []RuleAction) ([]byte, error) {
	switch kind {
	case "rule":
		if scope != "" {
			return nil, errors.New("scope: annotate proposals only")
		}
		return RuleSpec(m, as), nil
	case "annotate":
		if m != nil {
			return nil, errors.New("match: rule proposals only")
		}
		if scope == "" {
			scope = "message"
		}
		return json.Marshal(struct {
			V       int           `json:"v"`
			Scope   AnnotateScope `json:"scope"`
			Actions []RuleAction  `json:"actions"`
		}{1, scope, nonNilActions(as)})
	}
	return nil, errors.New("kind: annotate or rule")
}

// ProposalContent is what a stored proposal payload proposes: the scope
// of an annotate proposal, the match of a rule proposal, and the actions.
func ProposalContent(payload []byte) (AnnotateScope, RuleMatch, []RuleAction) {
	// The payload was validated and canonicalized when stored.
	var pl struct {
		Scope   AnnotateScope `json:"scope"`
		Match   RuleMatch     `json:"match"`
		Actions []RuleAction  `json:"actions"`
	}
	_ = json.Unmarshal(payload, &pl)
	return pl.Scope, pl.Match, nonNilActions(pl.Actions)
}

// MatchOf is a stored match tree in its JSON form.
func MatchOf(m rules.Match) RuleMatch {
	var out RuleMatch
	if b, err := json.Marshal(m); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}
