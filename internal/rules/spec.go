package rules

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
)

// Version is the stored rule format this package writes and evaluates.
const Version = 1

// ErrInvalid wraps every decoding and validation error.
var ErrInvalid = errors.New("invalid rule")

// maxParseDepth bounds recursion while decoding. It is a sanity bound for
// the decoder, deliberately above MaxDepth: a stored rule from a later
// version with a raised depth limit must still load (and be evaluated),
// while hostile input cannot recurse without bound.
const maxParseDepth = 32

// Match is one node of a match tree. Exactly one form is set: All, Any,
// Not, or a leaf (Field, Op, Value). Value is a string, a bool or a
// []string in a valid v1 rule; Parse keeps any other JSON value as decoded
// (float64, map, []any) so that a rule from a later version still loads
// and is reported unsupported instead of failing to decode.
type Match struct {
	All []Match
	Any []Match
	Not *Match

	Field string
	Op    string
	Value any
}

// Action is one entry of a rule's action list. Only the parameter of its
// kind is set: Label for label, Class for classify, Level for notify.
type Action struct {
	Kind  string `json:"kind"`
	Label string `json:"label,omitzero"`
	Class string `json:"class,omitzero"`
	Level string `json:"level,omitzero"`
}

// Spec is a whole rule as proposed or stored: {"v":1,"match":…,"actions":…}.
// The rules table keeps Match and Actions in separate columns; proposals
// keep the whole Spec as their payload.
type Spec struct {
	Match   Match
	Actions []Action
}

// InvalidError is a rule that does not decode or validate, and where:
// Path names the member in the rule's JSON ("match.all[2].value",
// "actions[0].label"), or "rule" for the whole. It is ErrInvalid.
type InvalidError struct {
	Path   string
	Reason string
}

func (e *InvalidError) Error() string { return ErrInvalid.Error() + ": " + e.Path + ": " + e.Reason }

// Is makes errors.Is(err, ErrInvalid) hold.
func (e *InvalidError) Is(target error) bool { return target == ErrInvalid }

func invalid(path, format string, args ...any) error {
	return &InvalidError{Path: path, Reason: fmt.Sprintf(format, args...)}
}

// ParseSpec decodes {"v":1,"match":…,"actions":…}. Unknown members,
// duplicate names and any version other than 1 are rejected.
func ParseSpec(data []byte) (Spec, error) {
	if len(data) > 2*MaxMatchBytes {
		return Spec{}, invalid("rule", "larger than %d bytes", 2*MaxMatchBytes)
	}
	var w struct {
		V       *int           `json:"v"`
		Match   jsontext.Value `json:"match"`
		Actions jsontext.Value `json:"actions"`
	}
	if err := json.Unmarshal(data, &w, json.RejectUnknownMembers(true)); err != nil {
		return Spec{}, invalid("rule", "%v", err)
	}
	if w.V == nil || *w.V != Version {
		return Spec{}, invalid("v", "must be %d", Version)
	}
	if w.Match == nil || w.Actions == nil {
		return Spec{}, invalid("rule", "match and actions are required")
	}
	m, err := ParseMatch(w.Match)
	if err != nil {
		return Spec{}, err
	}
	as, err := ParseActions(w.Actions)
	if err != nil {
		return Spec{}, err
	}
	return Spec{Match: m, Actions: as}, nil
}

// MarshalJSON writes the canonical form, the one to store.
func (s Spec) MarshalJSON() ([]byte, error) {
	actions := s.Actions
	if actions == nil {
		actions = []Action{}
	}
	return json.Marshal(struct {
		V       int      `json:"v"`
		Match   Match    `json:"match"`
		Actions []Action `json:"actions"`
	}{Version, s.Match, actions})
}

// UnmarshalJSON is ParseSpec, so Spec can sit in request types.
func (s *Spec) UnmarshalJSON(data []byte) error {
	p, err := ParseSpec(data)
	if err != nil {
		return err
	}
	*s = p
	return nil
}

// ParseMatch decodes a match tree strictly: every node is exactly one of
// {"all":[…]}, {"any":[…]}, {"not":{…}} or {"field","op","value"}; unknown
// or duplicate members are errors. It checks structure only, not the v1
// field table or limits (Validate does), so stored rules from a later
// version still decode. Values of address fields are folded with FoldAddr,
// which makes the stored form canonical.
func ParseMatch(data []byte) (Match, error) {
	if len(data) > MaxMatchBytes {
		return Match{}, invalid("match", "larger than %d bytes", MaxMatchBytes)
	}
	return parseNode(jsontext.Value(data), "match", 1)
}

func parseNode(v jsontext.Value, path string, depth int) (Match, error) {
	if depth > maxParseDepth {
		return Match{}, invalid(path, "nested deeper than %d", maxParseDepth)
	}
	if v.Kind() != '{' {
		return Match{}, invalid(path, "a node must be an object")
	}
	var obj map[string]jsontext.Value
	if err := json.Unmarshal(v, &obj); err != nil {
		return Match{}, invalid(path, "%v", err)
	}
	if len(obj) == 1 {
		for key, sub := range obj {
			switch key {
			case "all", "any":
				kids, err := parseList(sub, path+"."+key, depth)
				if err != nil {
					return Match{}, err
				}
				if key == "all" {
					return Match{All: kids}, nil
				}
				return Match{Any: kids}, nil
			case "not":
				kid, err := parseNode(sub, path+".not", depth+1)
				if err != nil {
					return Match{}, err
				}
				return Match{Not: &kid}, nil
			}
		}
	}
	field, fok := obj["field"]
	op, ook := obj["op"]
	value, vok := obj["value"]
	if len(obj) != 3 || !fok || !ook || !vok {
		return Match{}, invalid(path, `a node is exactly one of {"all":[…]}, {"any":[…]}, {"not":{…}} or {"field","op","value"}`)
	}
	m := Match{}
	if err := unmarshalString(field, &m.Field); err != nil {
		return Match{}, invalid(path+".field", "%v", err)
	}
	if err := unmarshalString(op, &m.Op); err != nil {
		return Match{}, invalid(path+".op", "%v", err)
	}
	if err := json.Unmarshal(value, &m.Value); err != nil {
		return Match{}, invalid(path+".value", "%v", err)
	}
	m.Value = tidyValue(m.Value)
	if fields[m.Field].addr && m.Op != OpRegex {
		m.Value = foldValue(m.Value)
	}
	return m, nil
}

func parseList(v jsontext.Value, path string, depth int) ([]Match, error) {
	if v.Kind() != '[' {
		return nil, invalid(path, "must be an array")
	}
	var items []jsontext.Value
	if err := json.Unmarshal(v, &items); err != nil {
		return nil, invalid(path, "%v", err)
	}
	kids := make([]Match, 0, len(items))
	for i, item := range items {
		kid, err := parseNode(item, fmt.Sprintf("%s[%d]", path, i), depth+1)
		if err != nil {
			return nil, err
		}
		kids = append(kids, kid)
	}
	return kids, nil
}

func unmarshalString(v jsontext.Value, s *string) error {
	if v.Kind() != '"' {
		return errors.New("must be a string")
	}
	return json.Unmarshal(v, s)
}

// tidyValue turns a JSON array of strings into a []string, the shape of
// an "in" value; everything else stays as decoded.
func tidyValue(v any) any {
	list, ok := v.([]any)
	if !ok {
		return v
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			return v
		}
		out = append(out, s)
	}
	return out
}

func foldValue(v any) any {
	switch v := v.(type) {
	case string:
		return FoldAddr(v)
	case []string:
		out := make([]string, len(v))
		for i, s := range v {
			out[i] = FoldAddr(s)
		}
		return out
	}
	return v
}

// MarshalJSON writes the node in its one form. A Match with several forms
// set is invalid (Validate rejects it); this writes the first of all, any,
// not, leaf.
func (m Match) MarshalJSON() ([]byte, error) {
	switch {
	case m.All != nil:
		return json.Marshal(struct {
			All []Match `json:"all"`
		}{m.All})
	case m.Any != nil:
		return json.Marshal(struct {
			Any []Match `json:"any"`
		}{m.Any})
	case m.Not != nil:
		return json.Marshal(struct {
			Not *Match `json:"not"`
		}{m.Not})
	}
	return json.Marshal(struct {
		Field string `json:"field"`
		Op    string `json:"op"`
		Value any    `json:"value"`
	}{m.Field, m.Op, m.Value})
}

// UnmarshalJSON is ParseMatch, so Match can sit in request types.
func (m *Match) UnmarshalJSON(data []byte) error {
	p, err := ParseMatch(data)
	if err != nil {
		return err
	}
	*m = p
	return nil
}

// ParseActions decodes an action list strictly. Each known kind must carry
// exactly its own members. An unknown kind decodes (keeping only Kind), so
// that a stored rule from a later version loads and is reported
// unsupported; Validate rejects it on save.
func ParseActions(data []byte) ([]Action, error) {
	if len(data) > MaxMatchBytes {
		return nil, invalid("actions", "larger than %d bytes", MaxMatchBytes)
	}
	v := jsontext.Value(data)
	if v.Kind() != '[' {
		return nil, invalid("actions", "must be an array")
	}
	var items []jsontext.Value
	if err := json.Unmarshal(v, &items); err != nil {
		return nil, invalid("actions", "%v", err)
	}
	out := make([]Action, 0, len(items))
	for i, item := range items {
		a, err := parseAction(item, fmt.Sprintf("actions[%d]", i))
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func parseAction(v jsontext.Value, path string) (Action, error) {
	if v.Kind() != '{' {
		return Action{}, invalid(path, "an action must be an object")
	}
	var obj map[string]jsontext.Value
	if err := json.Unmarshal(v, &obj); err != nil {
		return Action{}, invalid(path, "%v", err)
	}
	var a Action
	kind, ok := obj["kind"]
	if !ok {
		return Action{}, invalid(path, "kind is required")
	}
	if err := unmarshalString(kind, &a.Kind); err != nil {
		return Action{}, invalid(path+".kind", "%v", err)
	}
	keys, known := actionKeys[a.Kind]
	if !known {
		return Action{Kind: a.Kind}, nil
	}
	if len(obj) != len(keys) {
		return Action{}, invalid(path, "%s takes exactly %s", a.Kind, strings.Join(keys, ", "))
	}
	for _, k := range keys {
		raw, ok := obj[k]
		if !ok {
			return Action{}, invalid(path, "%s takes exactly %s", a.Kind, strings.Join(keys, ", "))
		}
		var dst *string
		switch k {
		case "kind":
			continue
		case "label":
			dst = &a.Label
		case "class":
			dst = &a.Class
		case "level":
			dst = &a.Level
		}
		if err := unmarshalString(raw, dst); err != nil {
			return Action{}, invalid(path+"."+k, "%v", err)
		}
	}
	return a, nil
}

// UnmarshalJSON decodes one action strictly, as ParseActions does.
func (a *Action) UnmarshalJSON(data []byte) error {
	p, err := parseAction(jsontext.Value(data), "action")
	if err != nil {
		return err
	}
	*a = p
	return nil
}
