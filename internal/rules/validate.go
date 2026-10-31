package rules

import (
	"encoding/json/v2"
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits checked when a rule is saved. They keep one rule's cost on a
// hostile message small and predictable. Raising one later is compatible;
// lowering one would invalidate stored rules, so don't.
const (
	MaxDepth        = 4        // nodes on the longest path from the root, leaf included
	MaxLeaves       = 32       // leaves per rule
	MaxIn           = 100      // items of one "in"
	MaxValueBytes   = 256      // one text value or "in" item
	MaxRegexBytes   = 512      // one regex source
	MaxRegexPerRule = 8        // regex leaves per rule
	MaxRegexTotal   = 64       // regex leaves across all enabled rules; see CheckRegexBudget
	MaxRegexInsts   = 2000     // compiled program size of one regex
	MaxMatchBytes   = 16 << 10 // canonical JSON of a match tree
	RegexWindow     = 64 << 10 // bytes of a text field a regex sees (frozen v1 semantics)
	MaxActions      = 16       // actions per rule
	MaxLabels       = 8        // label actions per rule
	MaxLabelRunes   = 64       // characters in a label
)

// Warning is something allowed but probably not intended. Code is stable
// for UIs to key on; Message is for people.
type Warning struct {
	Code    string
	Message string
}

// WarnUnverifiedFrom: the rule trusts the From header without requiring
// DMARC, and its actions make a forged message look legitimate.
const WarnUnverifiedFrom = "unverified_from"

// Validate checks s against v1 for saving: the field table, operator and
// value types, every limit, and the action list. It returns warnings for
// rules that are valid but risky. Callers store Marshal(s), not the input
// bytes, so the stored form is canonical.
func (s Spec) Validate() ([]Warning, error) {
	if err := ValidateMatch(s.Match); err != nil {
		return nil, err
	}
	if err := ValidateActions(s.Actions); err != nil {
		return nil, err
	}
	var warns []Warning
	if usesFrom(s.Match) && !requiresVerified(s.Match) && vouches(s.Actions) {
		warns = append(warns, Warning{
			Code: WarnUnverifiedFrom,
			Message: "the rule matches on the From header without from.verified = true; " +
				"anyone can forge From, so forged mail can get these labels or notifications",
		})
	}
	return warns, nil
}

// ValidateMatch checks a match tree against v1 and its limits.
func ValidateMatch(m Match) error {
	var c counts
	if err := c.walk(m, "match", 1); err != nil {
		return err
	}
	if c.leaves > MaxLeaves {
		return invalid("match", "%d leaves, at most %d", c.leaves, MaxLeaves)
	}
	if c.regexes > MaxRegexPerRule {
		return invalid("match", "%d regex conditions, at most %d", c.regexes, MaxRegexPerRule)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return invalid("match", "%v", err)
	}
	if len(b) > MaxMatchBytes {
		return invalid("match", "%d bytes, at most %d", len(b), MaxMatchBytes)
	}
	return nil
}

type counts struct{ leaves, regexes int }

func (c *counts) walk(m Match, path string, depth int) error {
	if depth > MaxDepth {
		return invalid(path, "nested deeper than %d", MaxDepth)
	}
	forms := 0
	for _, set := range []bool{m.All != nil, m.Any != nil, m.Not != nil, m.Field != "" || m.Op != "" || m.Value != nil} {
		if set {
			forms++
		}
	}
	if forms != 1 {
		return invalid(path, "a node is exactly one of all, any, not or a {field, op, value} leaf")
	}
	switch {
	case m.All != nil, m.Any != nil:
		kids, key := m.All, "all"
		if m.Any != nil {
			kids, key = m.Any, "any"
		}
		if len(kids) == 0 {
			return invalid(path+"."+key, "must not be empty")
		}
		for i, k := range kids {
			if err := c.walk(k, fmt.Sprintf("%s.%s[%d]", path, key, i), depth+1); err != nil {
				return err
			}
		}
		return nil
	case m.Not != nil:
		return c.walk(*m.Not, path+".not", depth+1)
	}
	c.leaves++
	if m.Op == OpRegex {
		c.regexes++
	}
	return checkLeaf(m, path)
}

func checkLeaf(m Match, path string) error {
	spec, ok := fields[m.Field]
	if !ok {
		return invalid(path+".field", "unknown field %q", m.Field)
	}
	if !spec.allows(m.Op) {
		return invalid(path+".op", "%q does not apply to %s (allowed: %s)", m.Op, m.Field, strings.Join(ops[spec.typ], ", "))
	}
	vpath := path + ".value"
	switch {
	case spec.typ == typeBool:
		if _, ok := m.Value.(bool); !ok {
			return invalid(vpath, "%s takes true or false", m.Field)
		}
	case m.Op == OpIn:
		list, ok := m.Value.([]string)
		if !ok || len(list) == 0 || len(list) > MaxIn {
			return invalid(vpath, "in takes an array of 1 to %d strings", MaxIn)
		}
		for i, s := range list {
			if err := checkScalar(spec, s, fmt.Sprintf("%s[%d]", vpath, i)); err != nil {
				return err
			}
		}
	case m.Op == OpRegex:
		s, ok := m.Value.(string)
		if !ok || s == "" {
			return invalid(vpath, "regex takes a non-empty string")
		}
		if len(s) > MaxRegexBytes {
			return invalid(vpath, "regex is %d bytes, at most %d", len(s), MaxRegexBytes)
		}
		if _, err := compileRegex(s); err != nil {
			return invalid(vpath, "%v", err)
		}
	default:
		s, ok := m.Value.(string)
		if !ok {
			return invalid(vpath, "%s takes a string", m.Op)
		}
		return checkScalar(spec, s, vpath)
	}
	return nil
}

func checkScalar(spec fieldSpec, s, path string) error {
	if spec.typ == typeEnum || spec.typ == typeSet {
		if !spec.known(s) {
			return invalid(path, "%q is not one of %s", s, strings.Join(spec.values, ", "))
		}
		return nil
	}
	switch {
	case s == "":
		return invalid(path, "must not be empty")
	case len(s) > MaxValueBytes:
		return invalid(path, "%d bytes, at most %d", len(s), MaxValueBytes)
	case !utf8.ValidString(s):
		return invalid(path, "not valid UTF-8")
	case spec.addr && s != FoldAddr(s):
		return invalid(path, "address values are stored folded: %q", FoldAddr(s))
	}
	return nil
}

// compileRegex compiles RE2 syntax and rejects programs over
// MaxRegexInsts. RE2 already runs in time linear in the input, but the
// constant is the program size, so the cap is what bounds one regex leaf
// on RegexWindow bytes of hostile text.
func compileRegex(expr string) (*regexp.Regexp, error) {
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		return nil, err
	}
	prog, err := syntax.Compile(re.Simplify())
	if err != nil {
		return nil, err
	}
	if n := len(prog.Inst); n > MaxRegexInsts {
		return nil, fmt.Errorf("regex compiles to %d instructions, at most %d", n, MaxRegexInsts)
	}
	return regexp.Compile(expr)
}

// RegexCount is the number of regex leaves in m.
func RegexCount(m Match) int {
	n := 0
	for _, k := range m.All {
		n += RegexCount(k)
	}
	for _, k := range m.Any {
		n += RegexCount(k)
	}
	if m.Not != nil {
		n += RegexCount(*m.Not)
	}
	if m.Op == OpRegex {
		n++
	}
	return n
}

// CheckRegexBudget checks the regex leaves of all enabled rules together
// against MaxRegexTotal: every enabled rule runs on every delivery, so
// the total, not the per-rule count, is what a message costs. The store
// calls it with every enabled rule's match, including the one being saved
// or enabled.
func CheckRegexBudget(ms ...Match) error {
	n := 0
	for _, m := range ms {
		n += RegexCount(m)
	}
	if n > MaxRegexTotal {
		return invalid("match", "enabled rules would have %d regex conditions, at most %d", n, MaxRegexTotal)
	}
	return nil
}

// ValidateActions checks an action list for saving.
func ValidateActions(as []Action) error {
	if len(as) == 0 || len(as) > MaxActions {
		return invalid("actions", "1 to %d actions", MaxActions)
	}
	per := map[string]int{}
	labels := map[string]bool{}
	for i, a := range as {
		path := fmt.Sprintf("actions[%d]", i)
		if err := checkAction(a, path); err != nil {
			return err
		}
		per[a.Kind]++
		if a.Kind == KindLabel {
			if labels[a.Label] {
				return invalid(path, "label %q twice", a.Label)
			}
			labels[a.Label] = true
		}
	}
	if per[KindLabel] > MaxLabels {
		return invalid("actions", "%d labels, at most %d", per[KindLabel], MaxLabels)
	}
	for _, k := range []string{KindClassify, KindArchive, KindNotify} {
		if per[k] > 1 {
			return invalid("actions", "%s at most once", k)
		}
	}
	return nil
}

// checkAction checks one action's kind and parameter, without the
// per-rule counts. Compile uses it too, to fail closed on stored actions
// it cannot honor.
func checkAction(a Action, path string) error {
	keys, ok := actionKeys[a.Kind]
	if !ok {
		return invalid(path, "unknown action kind %q", a.Kind)
	}
	set := map[string]bool{"kind": true, "label": a.Label != "", "class": a.Class != "", "level": a.Level != ""}
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	for k, isSet := range set {
		if isSet != want[k] {
			return invalid(path, "%s takes exactly %s", a.Kind, strings.Join(keys, ", "))
		}
	}
	switch a.Kind {
	case KindLabel:
		if err := ValidLabel(a.Label); err != nil {
			return invalid(path+".label", "%v", err)
		}
	case KindClassify:
		if a.Class != ClassMachine && a.Class != ClassHuman {
			return invalid(path+".class", "%q is not %s or %s", a.Class, ClassMachine, ClassHuman)
		}
	case KindNotify:
		switch a.Level {
		case NotifyOff, NotifySilent, NotifyNormal:
		default:
			return invalid(path+".level", "%q is not %s, %s or %s", a.Level, NotifyOff, NotifySilent, NotifyNormal)
		}
	}
	return nil
}

// ValidLabel reports whether s may be a label. Labels are exact,
// case-sensitive strings. Leading "$" and "\" are reserved for IMAP and
// system keywords; control and format characters (bidi overrides,
// zero-width) are rejected because labels are shown to people as a trust
// signal and must read as what they are. Every writer of labels (rules,
// people, agents) checks with this one function.
func ValidLabel(s string) error {
	switch {
	case s == "":
		return fmt.Errorf("label is empty")
	case !utf8.ValidString(s):
		return fmt.Errorf("label is not valid UTF-8")
	case utf8.RuneCountInString(s) > MaxLabelRunes:
		return fmt.Errorf("label is longer than %d characters", MaxLabelRunes)
	case s[0] == '$' || s[0] == '\\':
		return fmt.Errorf("label must not start with $ or \\")
	}
	first, _ := utf8.DecodeRuneInString(s)
	last, _ := utf8.DecodeLastRuneInString(s)
	if unicode.IsSpace(first) || unicode.IsSpace(last) {
		return fmt.Errorf("label must not start or end with a space")
	}
	for _, r := range s {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("label contains control character %U", r)
		}
	}
	return nil
}

func usesFrom(m Match) bool {
	switch m.Field {
	case FieldFromAddr, FieldFromDomain, FieldFromOrg:
		return true
	}
	for _, k := range append(append([]Match(nil), m.All...), m.Any...) {
		if usesFrom(k) {
			return true
		}
	}
	return m.Not != nil && usesFrom(*m.Not)
}

// requiresVerified reports whether m can only match when from.verified is
// true: the leaf itself, or an all with such a child, or an any whose
// children all require it. "not" is never credited.
func requiresVerified(m Match) bool {
	switch {
	case m.All != nil:
		for _, k := range m.All {
			if requiresVerified(k) {
				return true
			}
		}
		return false
	case m.Any != nil:
		for _, k := range m.Any {
			if !requiresVerified(k) {
				return false
			}
		}
		return true
	case m.Not != nil:
		return false
	}
	v, _ := m.Value.(bool)
	return m.Field == FieldFromVerified && m.Op == OpEq && v
}

// vouches reports whether the actions would make a message look
// legitimate to its reader: a label reads as a trust signal and a
// notification puts the message in front of them.
func vouches(as []Action) bool {
	for _, a := range as {
		if a.Kind == KindLabel || a.Kind == KindNotify {
			return true
		}
	}
	return false
}
