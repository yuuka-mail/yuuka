package search

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yuuka-mail/yuuka/internal/classify"
)

// The bounds of a query (§16.2). A query may come from an agent that mail
// talked into writing it, so its cost is bounded by its size.
const (
	MaxBytes      = 1024
	MaxConditions = 32
	// MaxDepth bounds groups and negations nested in one another.
	MaxDepth = 8
)

// Env is what reading a query depends on besides its text.
type Env struct {
	// Now anchors relative times (after:-7d); zero is time.Now.
	Now time.Time
	// Loc is the time zone a date (after:2026-10-01) starts its day in;
	// nil is UTC.
	Loc *time.Location
	// Triage reads the value of triage: against the question set
	// (triage.ParseFilter). Nil means no scorer is configured, and
	// triage: is refused: matching nothing would read as "the model
	// found none".
	Triage func(string) (question, option string, err error)
}

// Query is a parsed query.
type Query struct {
	// Filter is nil for an empty query, which matches everything.
	Filter *Filter
	// Normalized is the query as yuuka read it, in the text form: what
	// the caller echoes. Parsing it again gives the same Filter.
	Normalized string
	// Hints are items that were searched as text although they look like
	// a misspelt operator.
	Hints []Hint
}

// The text form's operator names, in the order hints prefer them. The
// reserved ones are JMAP conditions this version does not support.
var operators = []string{
	"from", "to", "cc", "bcc", "subject", "body", "after", "before", "has", "is",
	"deliveredto", "label", "class", "list", "triage",
	"minsize", "maxsize", "header", "keyword",
}

var stringFields = map[string]Field{
	"from": From, "to": To, "cc": Cc, "bcc": Bcc, "subject": Subject, "body": Body, "list": ListID,
}

// reserved are JMAP's conditions without support yet, by text name.
var reserved = map[string]string{
	"minsize": "minSize", "maxsize": "maxSize", "header": "header", "keyword": "hasKeyword",
}

// aliases are names other mail clients, and yuuka's own former search
// parameters, use for an operator: an item written with one gets a hint.
var aliases = map[string]string{
	"since": "after:", "until": "before:", "newer_than": "after:", "older_than": "before:",
	"prefix": "deliveredto:", "tag": "label:", "hasattachment": "has:attachment", "listid": "list:",
}

func isOperator(name string) bool {
	for _, o := range operators {
		if o == name {
			return true
		}
	}
	return false
}

// Parse reads q in the text form (§16.2):
//
//   - items separated by white space must all hold;
//   - OR (upper case) joins the items on either side and binds tighter
//     than the implicit AND, so a b OR c is a AND (b OR c);
//   - ( ) groups, and - in front of an item or group negates it;
//   - "a phrase" is one term, with \" and \\ escaped; an unclosed quote
//     runs to the end;
//   - name:value is an operator when name is one (case-insensitive), and
//     otherwise the whole item is text: Re:hello, 10:30 and URLs search
//     as written.
//
// An operator with a value it cannot take is an error at its position,
// as is a reserved one; so are a stray ), an unclosed (, and OR without a
// term on one side.
func Parse(q string, env Env) (Query, error) {
	if len(q) > MaxBytes {
		return Query{}, &Error{Msg: fmt.Sprintf("the query is longer than %d bytes", MaxBytes)}
	}
	// Postgres refuses both in text; say so rather than fail later.
	if !utf8.ValidString(q) || strings.ContainsRune(q, 0) {
		return Query{}, &Error{Msg: "the query is not UTF-8 text"}
	}
	if env.Now.IsZero() {
		env.Now = time.Now()
	}
	if env.Loc == nil {
		env.Loc = time.UTC
	}
	p := &parser{s: q, env: env}
	f, err := p.and()
	if err != nil {
		return Query{}, err
	}
	if !p.eof() {
		return Query{}, p.errorf(p.i, ") without a matching (")
	}
	f = normalize(f)
	return Query{Filter: f, Normalized: Format(f, env.Loc), Hints: p.hints}, nil
}

type parser struct {
	s      string
	i      int // byte offset
	env    Env
	parens int // open groups
	depth  int // open groups and negations
	conds  int
	hints  []Hint
}

func (p *parser) eof() bool  { return p.i >= len(p.s) }
func (p *parser) peek() byte { return p.s[p.i] }

// at is the 1-based character position of byte offset i.
func (p *parser) at(i int) int { return utf8.RuneCountInString(p.s[:i]) + 1 }

func (p *parser) errorf(i int, format string, args ...any) *Error {
	return &Error{At: p.at(i), Msg: fmt.Sprintf(format, args...)}
}

func (p *parser) spaceAt(i int) bool {
	r, _ := utf8.DecodeRuneInString(p.s[i:])
	return unicode.IsSpace(r)
}

func (p *parser) space() {
	for !p.eof() {
		r, n := utf8.DecodeRuneInString(p.s[p.i:])
		if !unicode.IsSpace(r) {
			return
		}
		p.i += n
	}
}

// wordEnd is where an unquoted word starting at i ends: at white space,
// or at a ) that closes an open group.
func (p *parser) wordEnd(i int) int {
	for i < len(p.s) {
		if p.spaceAt(i) || p.s[i] == ')' && p.parens > 0 {
			return i
		}
		_, n := utf8.DecodeRuneInString(p.s[i:])
		i += n
	}
	return i
}

// atOR reports whether the next word is the operator OR.
func (p *parser) atOR() bool {
	return !p.eof() && p.s[p.i:p.wordEnd(p.i)] == "OR"
}

// and reads items up to the end or a closing ); nil when there are none.
func (p *parser) and() (*Filter, error) {
	var args []*Filter
	for {
		p.space()
		if p.eof() || p.peek() == ')' {
			break
		}
		if p.atOR() {
			return nil, p.errorf(p.i, "OR needs a term before it")
		}
		f, err := p.or()
		if err != nil {
			return nil, err
		}
		args = append(args, f)
	}
	switch len(args) {
	case 0:
		return nil, nil
	case 1:
		return args[0], nil
	}
	return &Filter{Op: And, Args: args}, nil
}

func (p *parser) or() (*Filter, error) {
	f, err := p.unary()
	if err != nil {
		return nil, err
	}
	args := []*Filter{f}
	for {
		save := p.i
		p.space()
		if !p.atOR() {
			p.i = save
			break
		}
		at := p.i
		p.i += len("OR")
		p.space()
		if p.eof() || p.peek() == ')' || p.atOR() {
			return nil, p.errorf(at, "OR needs a term after it")
		}
		g, err := p.unary()
		if err != nil {
			return nil, err
		}
		args = append(args, g)
	}
	if len(args) == 1 {
		return f, nil
	}
	return &Filter{Op: Or, Args: args}, nil
}

func (p *parser) enter(i int) error {
	p.depth++
	if p.depth > MaxDepth {
		return p.errorf(i, "groups and negations nest deeper than %d", MaxDepth)
	}
	return nil
}

// unary is an item or group, negated by a - right in front of it. A -
// followed by white space or a ) is the text "-".
func (p *parser) unary() (*Filter, error) {
	if p.peek() == '-' && p.i+1 < len(p.s) && !p.spaceAt(p.i+1) && p.s[p.i+1] != ')' {
		if err := p.enter(p.i); err != nil {
			return nil, err
		}
		p.i++
		f, err := p.unary()
		if err != nil {
			return nil, err
		}
		p.depth--
		return &Filter{Op: Not, Args: []*Filter{f}}, nil
	}
	if p.peek() == '(' {
		open := p.i
		if err := p.enter(open); err != nil {
			return nil, err
		}
		p.i++
		p.parens++
		f, err := p.and()
		if err != nil {
			return nil, err
		}
		if p.eof() {
			return nil, p.errorf(open, "( is not closed")
		}
		if f == nil {
			return nil, p.errorf(open, "( ) holds nothing")
		}
		p.i++ // )
		p.parens--
		p.depth--
		return f, nil
	}
	return p.item()
}

// phrase reads a quoted string starting at the quote.
func (p *parser) phrase() string {
	var b strings.Builder
	p.i++
	for !p.eof() {
		c := p.peek()
		switch {
		case c == '\\' && p.i+1 < len(p.s) && (p.s[p.i+1] == '"' || p.s[p.i+1] == '\\'):
			b.WriteByte(p.s[p.i+1])
			p.i += 2
		case c == '"':
			p.i++
			return b.String()
		default:
			b.WriteByte(c)
			p.i++
		}
	}
	return b.String()
}

func (p *parser) item() (*Filter, error) {
	start := p.i
	if p.peek() == '"' {
		term := p.phrase()
		if term == "" {
			return nil, p.errorf(start, `"" is empty`)
		}
		return p.leaf(Condition{Field: Text, Term: term}, start)
	}
	end := p.wordEnd(p.i)
	word := p.s[p.i:end]
	if k := strings.IndexByte(word, ':'); k > 0 {
		name := strings.ToLower(word[:k])
		if isOperator(name) {
			p.i += k + 1
			var value string
			if !p.eof() && p.peek() == '"' {
				value = p.phrase()
			} else {
				e := p.wordEnd(p.i)
				value, p.i = p.s[p.i:e], e
			}
			if value == "" {
				return nil, p.errorf(start, "%s: needs a value", name)
			}
			c, err := p.condition(name, value, start)
			if err != nil {
				return nil, err
			}
			return p.leaf(c, start)
		}
		if s := suggest(name); s != "" {
			p.hints = append(p.hints, Hint{Term: word, Suggest: s})
		}
	}
	p.i = end
	return p.leaf(Condition{Field: Text, Term: word}, start)
}

func (p *parser) leaf(c Condition, start int) (*Filter, error) {
	p.conds++
	if p.conds > MaxConditions {
		return nil, p.errorf(start, "more than %d conditions", MaxConditions)
	}
	c.At = p.at(start)
	return &Filter{Cond: c}, nil
}

// condition reads the value of operator name, written at start.
func (p *parser) condition(name, value string, start int) (Condition, error) {
	if f, ok := stringFields[name]; ok {
		return Condition{Field: f, Term: value}, nil
	}
	if jmap, ok := reserved[name]; ok {
		return Condition{}, &Error{
			At: p.at(start), Unsupported: true,
			Msg: fmt.Sprintf("%s: (JMAP %s) is reserved and not supported yet", name, jmap),
		}
	}
	switch name {
	case "after", "before":
		t, date, err := p.time(value)
		if err != nil {
			return Condition{}, p.errorf(start, "%s: %v", name, err)
		}
		f := After
		if name == "before" {
			f = Before
		}
		return Condition{Field: f, Time: t, Date: date}, nil
	case "has":
		if strings.EqualFold(value, "attachment") {
			return Condition{Field: HasAttachment}, nil
		}
		return Condition{}, p.errorf(start, "has: takes attachment, not %q", value)
	case "is":
		switch strings.ToLower(value) {
		case "archived":
			return Condition{Field: Archived}, nil
		case "downgraded":
			return Condition{Field: Downgraded}, nil
		case "sent":
			return Condition{Field: Sent}, nil
		case "unread":
			return Condition{}, &Error{
				At: p.at(start), Unsupported: true,
				Msg: "is:unread (JMAP notKeyword $seen) is reserved and not supported yet: read state is not tracked",
			}
		}
		return Condition{}, p.errorf(start, "is: takes archived, downgraded or sent, not %q", value)
	case "deliveredto":
		// store.FoldAddr's folding, as deliveries store prefix and zone.
		a := strings.ToLower(value)
		prefix, zone := a, ""
		if i := strings.LastIndexByte(a, '@'); i >= 0 {
			prefix, zone = a[:i], a[i+1:]
			if zone == "" {
				return Condition{}, p.errorf(start, "deliveredto: takes a prefix or prefix@domain, not %q", value)
			}
		}
		if prefix == "" {
			return Condition{}, p.errorf(start, "deliveredto: takes a prefix or prefix@domain, not %q", value)
		}
		return Condition{Field: DeliveredTo, Prefix: prefix, Zone: zone}, nil
	case "label":
		return Condition{Field: Label, Label: value}, nil
	case "class":
		for _, c := range []classify.Class{classify.Machine, classify.Human, classify.Unknown} {
			if strings.EqualFold(value, c.String()) {
				return Condition{Field: Class, Class: c}, nil
			}
		}
		return Condition{}, p.errorf(start, "class: takes machine, human or unknown, not %q", value)
	case "triage":
		if p.env.Triage == nil {
			return Condition{}, p.errorf(start, "triage: no decision model is configured here, so no mail is scored")
		}
		q, o, err := p.env.Triage(value)
		if err != nil {
			return Condition{}, p.errorf(start, "triage: %v", err)
		}
		return Condition{Field: Triage, Question: q, Option: o}, nil
	}
	panic("search: operator without a condition: " + name)
}

// The bounds of a relative time, about a thousand years: far enough for
// any mail, near enough that Postgres can hold the result.
var relativeMax = map[byte]int{'h': 1000 * 366 * 24, 'd': 1000 * 366, 'w': 1000 * 53}

// time reads YYYY-MM-DD (the day starting in Loc), an RFC 3339 time, or
// -<n>h, -<n>d, -<n>w before now. date is the day as written, if it was.
func (p *parser) time(v string) (t time.Time, date string, err error) {
	want := fmt.Errorf("want YYYY-MM-DD, an RFC 3339 time, or -<n>h, -<n>d or -<n>w, not %q", v)
	if len(v) >= 3 && v[0] == '-' {
		limit, ok := relativeMax[v[len(v)-1]]
		digits := v[1 : len(v)-1]
		if !ok || strings.TrimLeft(digits, "0123456789") != "" {
			return time.Time{}, "", want
		}
		n, err := strconv.Atoi(digits)
		if err != nil || n > limit {
			return time.Time{}, "", fmt.Errorf("%q is too far back", v)
		}
		// Whole seconds, so that the echo parses back to the same time.
		now := p.env.Now.In(p.env.Loc).Truncate(time.Second)
		switch v[len(v)-1] {
		case 'h':
			return now.Add(-time.Duration(n) * time.Hour), "", nil
		case 'd':
			return now.AddDate(0, 0, -n), "", nil
		default:
			return now.AddDate(0, 0, -7*n), "", nil
		}
	}
	if t, err = time.ParseInLocation(time.DateOnly, v, p.env.Loc); err == nil {
		date = t.Format(time.DateOnly)
	} else if t, err = time.Parse(time.RFC3339, v); err != nil {
		return time.Time{}, "", want
	}
	// Any zone's echo of it stays a four-digit year, which RFC 3339 needs.
	if t.Year() < 1 || t.Year() > 9998 {
		return time.Time{}, "", fmt.Errorf("%q is out of range", v)
	}
	return t, date, nil
}

// suggest is the operator an unknown name probably meant, "" for none:
// an alias, or an operator one or two edits away. Names of one or two
// letters get none: Re:, AW:, TR: begin subjects, and every such name is
// an edit or two from to: or cc:.
func suggest(name string) string {
	if s, ok := aliases[name]; ok {
		return s
	}
	n := utf8.RuneCountInString(name)
	if n <= 2 {
		return ""
	}
	limit := 1
	if n >= 5 {
		limit = 2
	}
	for _, o := range operators {
		if _, ok := reserved[o]; !ok && distance(name, o) <= limit {
			return o + ":"
		}
	}
	return ""
}

// distance is the optimal string alignment distance: insertions,
// deletions, substitutions and adjacent transpositions (form, from).
func distance(a, b string) int {
	x, y := []rune(a), []rune(b)
	d := make([][]int, len(x)+1)
	for i := range d {
		d[i] = make([]int, len(y)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(x); i++ {
		for j := 1; j <= len(y); j++ {
			cost := 1
			if x[i-1] == y[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && x[i-1] == y[j-2] && x[i-2] == y[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(x)][len(y)]
}
