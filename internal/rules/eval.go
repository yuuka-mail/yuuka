package rules

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrUnsupported: the rule uses a field, operator, value or action this
// version does not know, or cannot honor. Such a rule never matches.
var ErrUnsupported = errors.New("rule is unsupported by this version")

// Program is a rule compiled for evaluation. It is immutable and safe for
// concurrent use; the store caches one per (rule id, revision).
type Program struct {
	root        *node
	actions     []Action
	unsupported []string
}

type nodeKind int

const (
	nodeAll nodeKind = iota
	nodeAny
	nodeNot
	nodeLeaf
)

type node struct {
	kind  nodeKind
	kids  []*node
	field string
	typ   fieldType
	op    string
	str   string   // eq, contains, has: case-folded for text
	set   []string // in: case-folded for text
	b     bool
	re    *regexp.Regexp
}

// Load compiles a stored rule from its match and actions columns. It never
// fails: a rule that does not decode is returned unsupported, so one bad
// row cannot stop ingest, and it fails closed like any unknown field.
func Load(match, actions []byte) *Program {
	m, err := ParseMatch(match)
	if err != nil {
		return &Program{unsupported: []string{err.Error()}}
	}
	as, err := ParseActions(actions)
	if err != nil {
		return &Program{unsupported: []string{err.Error()}}
	}
	return Compile(Spec{Match: m, Actions: as})
}

// Compile prepares s for evaluation. It does not apply the save-time
// limits (stored rules passed them when saved, and later versions may
// raise them) but it does check everything the semantics depend on:
// anything unknown or malformed makes the whole program unsupported,
// including under "not", where a never-matching leaf would otherwise turn
// into an always-matching rule. Regexes are compiled under the same
// program-size cap as on save.
func Compile(s Spec) *Program {
	p := &Program{actions: append([]Action(nil), s.Actions...)}
	p.root = p.compile(s.Match, 0)
	for i, a := range s.Actions {
		if err := checkAction(a, fmt.Sprintf("actions[%d]", i)); err != nil {
			p.unsupported = append(p.unsupported, err.Error())
		}
	}
	if len(s.Actions) == 0 {
		p.unsupported = append(p.unsupported, "no actions")
	}
	return p
}

func (p *Program) compile(m Match, depth int) *node {
	if depth > maxParseDepth {
		p.unsupported = append(p.unsupported, "nested too deep")
		return nil
	}
	switch {
	case m.All != nil, m.Any != nil:
		n := &node{kind: nodeAll}
		kids := m.All
		if m.All == nil {
			n.kind, kids = nodeAny, m.Any
		}
		if len(kids) == 0 {
			p.unsupported = append(p.unsupported, "empty all or any")
		}
		for _, k := range kids {
			n.kids = append(n.kids, p.compile(k, depth+1))
		}
		return n
	case m.Not != nil:
		return &node{kind: nodeNot, kids: []*node{p.compile(*m.Not, depth+1)}}
	}
	n, err := compileLeaf(m)
	if err != nil {
		p.unsupported = append(p.unsupported, err.Error())
	}
	return n
}

func compileLeaf(m Match) (*node, error) {
	spec, ok := fields[m.Field]
	if !ok {
		return nil, fmt.Errorf("field %q", m.Field)
	}
	if !spec.allows(m.Op) {
		return nil, fmt.Errorf("operator %q on %s", m.Op, m.Field)
	}
	n := &node{kind: nodeLeaf, field: m.Field, typ: spec.typ, op: m.Op}
	bad := fmt.Errorf("value of %s %s", m.Field, m.Op)
	scalar := func(s string) (string, error) {
		if spec.typ == typeText {
			return strings.ToLower(s), nil
		}
		if !spec.known(s) {
			return "", fmt.Errorf("value %q of %s", s, m.Field)
		}
		return s, nil
	}
	var err error
	switch {
	case spec.typ == typeBool:
		b, ok := m.Value.(bool)
		if !ok {
			return nil, bad
		}
		n.b = b
	case m.Op == OpIn:
		list, ok := m.Value.([]string)
		if !ok {
			return nil, bad
		}
		for _, s := range list {
			s, err := scalar(s)
			if err != nil {
				return nil, err
			}
			n.set = append(n.set, s)
		}
	case m.Op == OpRegex:
		s, ok := m.Value.(string)
		if !ok {
			return nil, bad
		}
		if n.re, err = compileRegex(s); err != nil {
			return nil, fmt.Errorf("regex on %s: %w", m.Field, err)
		}
	default:
		s, ok := m.Value.(string)
		if !ok {
			return nil, bad
		}
		if n.str, err = scalar(s); err != nil {
			return nil, err
		}
	}
	return n, nil
}

// Unsupported lists why the program never matches, or nothing.
func (p *Program) Unsupported() []string { return p.unsupported }

// Actions returns the rule's actions as compiled.
func (p *Program) Actions() []Action { return p.actions }

// Eval reports whether the rule matches f. It fails closed: an
// unsupported rule returns false with ErrUnsupported, and when ctx is done
// before evaluation finishes it returns false with ctx's error. The
// deadline is checked before each leaf; a leaf already running is not
// interrupted but bounded by RegexWindow and MaxRegexInsts: a regex at
// both caps costs on the order of a second on current hardware
// (BenchmarkWorstRegex), which is the most a deadline can overshoot.
func (p *Program) Eval(ctx context.Context, f *Facts) (bool, error) {
	return p.eval(ctx, newView(f))
}

func (p *Program) eval(ctx context.Context, v *view) (bool, error) {
	if len(p.unsupported) > 0 {
		return false, fmt.Errorf("%w: %s", ErrUnsupported, strings.Join(p.unsupported, "; "))
	}
	return p.root.eval(ctx, v)
}

func (n *node) eval(ctx context.Context, v *view) (bool, error) {
	switch n.kind {
	case nodeAll, nodeAny:
		want := n.kind == nodeAny
		for _, k := range n.kids {
			ok, err := k.eval(ctx, v)
			if err != nil {
				return false, err
			}
			if ok == want {
				return want, nil
			}
		}
		return !want, nil
	case nodeNot:
		ok, err := n.kids[0].eval(ctx, v)
		return !ok && err == nil, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	switch n.typ {
	case typeBool:
		return v.bool(n.field) == n.b, nil
	case typeEnum:
		got := v.enum(n.field)
		if n.op == OpIn {
			return contains(n.set, got), nil
		}
		return got == n.str, nil
	case typeSet:
		return v.has(n.field, n.str), nil
	}
	switch n.op {
	case OpEq:
		return v.lowered(n.field) == n.str, nil
	case OpIn:
		return contains(n.set, v.lowered(n.field)), nil
	case OpContains:
		return strings.Contains(v.lowered(n.field), n.str), nil
	case OpRegex:
		return n.re.MatchString(window(v.text[n.field])), nil
	}
	return false, nil
}

func contains(set []string, s string) bool {
	for _, x := range set {
		if x == s {
			return true
		}
	}
	return false
}
