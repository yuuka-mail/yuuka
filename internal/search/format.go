package search

import (
	"strings"
	"time"
	"unicode"
)

// normalize flattens an AND in an AND and an OR in an OR, drops a group
// of one, and cancels a double negation. The meaning does not change;
// the echo gets shorter, and parsing it again gives the same tree.
func normalize(f *Filter) *Filter {
	if f == nil || f.Op == "" {
		return f
	}
	var args []*Filter
	for _, a := range f.Args {
		a = normalize(a)
		if a.Op == f.Op && f.Op != Not {
			args = append(args, a.Args...)
		} else {
			args = append(args, a)
		}
	}
	if f.Op == Not && len(args) == 1 && args[0].Op == Not && len(args[0].Args) == 1 {
		return args[0].Args[0]
	}
	if f.Op != Not && len(args) == 1 {
		return args[0]
	}
	return &Filter{Op: f.Op, Args: args}
}

// Format writes f in the text form, times in loc: the normalized echo
// (Query.Normalized). It is "" for nil, the query that matches all.
func Format(f *Filter, loc *time.Location) string {
	if f == nil {
		return ""
	}
	if loc == nil {
		loc = time.UTC
	}
	return writer{loc}.and(f)
}

type writer struct{ loc *time.Location }

// and writes f as a sequence of items, as at the top or inside ( ).
func (w writer) and(f *Filter) string {
	if f.Op != And {
		return w.or(f)
	}
	parts := make([]string, len(f.Args))
	for i, a := range f.Args {
		parts[i] = w.or(a)
	}
	return strings.Join(parts, " ")
}

// or writes f as one item of a sequence: OR binds tighter than it.
func (w writer) or(f *Filter) string {
	if f.Op != Or {
		return w.unary(f)
	}
	parts := make([]string, len(f.Args))
	for i, a := range f.Args {
		parts[i] = w.unary(a)
	}
	return strings.Join(parts, " OR ")
}

// unary writes f as an operand of OR or -.
func (w writer) unary(f *Filter) string {
	switch f.Op {
	case "":
		return w.leaf(f.Cond)
	case Not:
		if len(f.Args) == 1 && f.Args[0].Op == "" {
			return "-" + w.leaf(f.Args[0].Cond)
		}
		// NOT is "none of": -(a OR b).
		inner := f.Args[0]
		if len(f.Args) > 1 {
			inner = &Filter{Op: Or, Args: f.Args}
		}
		return "-(" + w.and(inner) + ")"
	}
	return "(" + w.and(f) + ")"
}

// textNames are the text form's names of the conditions with a value.
var textNames = map[Field]string{
	From: "from", To: "to", Cc: "cc", Bcc: "bcc", Subject: "subject", Body: "body", ListID: "list",
	After: "after", Before: "before", DeliveredTo: "deliveredto", Label: "label", Class: "class", Triage: "triage",
}

func (w writer) leaf(c Condition) string {
	switch c.Field {
	case Text:
		return quoteText(c.Term)
	case From, To, Cc, Bcc, Subject, Body, ListID:
		return textNames[c.Field] + ":" + quoteValue(c.Term)
	case After, Before:
		if c.Date != "" {
			return textNames[c.Field] + ":" + c.Date
		}
		return textNames[c.Field] + ":" + c.Time.In(w.loc).Format(time.RFC3339Nano)
	case HasAttachment:
		return "has:attachment"
	case Archived:
		return "is:archived"
	case Downgraded:
		return "is:downgraded"
	case Sent:
		return "is:sent"
	case DeliveredTo:
		a := c.Prefix
		if c.Zone != "" {
			a += "@" + c.Zone
		}
		return "deliveredto:" + quoteValue(a)
	case Label:
		return "label:" + quoteValue(c.Label)
	case Class:
		return "class:" + c.Class.String()
	case Triage:
		return "triage:" + c.Question + ":" + c.Option
	}
	panic("search: no text form for " + string(c.Field))
}

// quoteValue quotes a value that would not read back as one word.
func quoteValue(v string) string {
	if v != "" && !strings.ContainsFunc(v, func(r rune) bool {
		return unicode.IsSpace(r) || r == '"' || r == '(' || r == ')'
	}) {
		return v
	}
	return quote(v)
}

// quoteText quotes a text term that would not read back as text: one
// that would be an operator, a negation or OR.
func quoteText(t string) string {
	if q := quoteValue(t); q != t {
		return q
	}
	if t == "OR" || strings.HasPrefix(t, "-") {
		return quote(t)
	}
	if k := strings.IndexByte(t, ':'); k > 0 && isOperator(strings.ToLower(t[:k])) {
		return quote(t)
	}
	return t
}

func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
