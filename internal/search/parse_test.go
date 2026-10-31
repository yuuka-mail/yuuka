package search

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// now is the clock of every test: relative times resolve against it.
var now = time.Date(2026, 11, 1, 12, 30, 45, 0, time.UTC)

func env() Env {
	return Env{Now: now, Triage: func(s string) (string, string, error) {
		q, o, _ := strings.Cut(s, ":")
		if q != "phishing" || (o != "yes" && o != "no") {
			return "", "", errors.New("want phishing:yes or phishing:no")
		}
		return q, o, nil
	}}
}

// tree writes f as an S-expression, for comparing structure.
func tree(f *Filter) string {
	if f == nil {
		return "()"
	}
	if f.Op == "" {
		c := f.Cond
		switch c.Field {
		case After, Before:
			return fmt.Sprintf("%s=%s", c.Field, c.Time.UTC().Format(time.RFC3339))
		case DeliveredTo:
			return fmt.Sprintf("%s=%s|%s", c.Field, c.Prefix, c.Zone)
		case Label:
			return fmt.Sprintf("%s=%q", c.Field, c.Label)
		case Class:
			return fmt.Sprintf("%s=%s", c.Field, c.Class)
		case Triage:
			return fmt.Sprintf("%s=%s:%s", c.Field, c.Question, c.Option)
		case Text, From, To, Cc, Bcc, Subject, Body, ListID:
			return fmt.Sprintf("%s=%q", c.Field, c.Term)
		}
		return string(c.Field)
	}
	parts := []string{string(f.Op)}
	for _, a := range f.Args {
		parts = append(parts, tree(a))
	}
	return "(" + strings.Join(parts, " ") + ")"
}

func TestParse(t *testing.T) {
	cases := []struct {
		q, tree, echo string
	}{
		{"", "()", ""},
		{"   ", "()", ""},
		{"invoice", `text="invoice"`, "invoice"},
		{"验证码 通知", `(AND text="验证码" text="通知")`, "验证码 通知"},
		{"验证码　通知", `(AND text="验证码" text="通知")`, "验证码 通知"}, // ideographic space
		{`"foo bar"`, `text="foo bar"`, `"foo bar"`},
		{`"会议 时间"`, `text="会议 时间"`, `"会议 时间"`},
		{`"say \"hi\" \\ \n"`, `text="say \"hi\" \\ \\n"`, `"say \"hi\" \\ \\n"`},
		{`"unclosed phrase`, `text="unclosed phrase"`, `"unclosed phrase"`},
		{`"one"`, `text="one"`, "one"},
		{`don't l'été`, `(AND text="don't" text="l'été")`, "don't l'été"},
		// OR binds tighter than the implicit AND.
		{"a b OR c", `(AND text="a" (OR text="b" text="c"))`, "a b OR c"},
		{"a OR b OR c", `(OR text="a" text="b" text="c")`, "a OR b OR c"},
		{"(a b) OR c", `(OR (AND text="a" text="b") text="c")`, "(a b) OR c"},
		{"a or b", `(AND text="a" text="or" text="b")`, "a or b"},
		{"((a))", `text="a"`, "a"},
		{"(a (b c))", `(AND text="a" text="b" text="c")`, "a b c"},
		{"((a OR b) OR c)", `(OR text="a" text="b" text="c")`, "a OR b OR c"},
		// Negation.
		{"-spam", `(NOT text="spam")`, "-spam"},
		{"--spam", `text="spam"`, "spam"},
		{"-(a OR b) c", `(AND (NOT (OR text="a" text="b")) text="c")`, "-(a OR b) c"},
		{"-(a b)", `(NOT (AND text="a" text="b"))`, "-(a b)"},
		{`-"a b"`, `(NOT text="a b")`, `-"a b"`},
		{"-from:x", `(NOT from="x")`, "-from:x"},
		{"a - b", `(AND text="a" text="-" text="b")`, `a "-" b`},
		{"a-b", `text="a-b"`, "a-b"},
		{"-a OR b", `(OR (NOT text="a") text="b")`, "-a OR b"},
		// A name that is not an operator leaves the item text.
		{"Re:hello", `text="Re:hello"`, "Re:hello"},
		{"10:30", `text="10:30"`, "10:30"},
		{"https://example.com/a?b=c", `text="https://example.com/a?b=c"`, "https://example.com/a?b=c"},
		{":x", `text=":x"`, ":x"},
		{`"from:alice"`, `text="from:alice"`, `"from:alice"`},
		{`"OR"`, `text="OR"`, `"OR"`},
		{`"-x"`, `text="-x"`, `"-x"`},
		{`"(注)"`, `text="(注)"`, `"(注)"`},
		{"(注)", `text="注"`, "注"},
		{"a)b", `text="a)b"`, `"a)b"`},
		// Operators, case-insensitive by name.
		{"FROM:Alice", `from="Alice"`, "from:Alice"},
		{`from:"Alice Smith"`, `from="Alice Smith"`, `from:"Alice Smith"`},
		{`subject:"foo bar"`, `subject="foo bar"`, `subject:"foo bar"`},
		{"subject:Re:hello", `subject="Re:hello"`, "subject:Re:hello"},
		{
			"to:a@example.com cc:b bcc:c body:d list:e", `(AND to="a@example.com" cc="b" bcc="c" body="d" listId="e")`,
			"to:a@example.com cc:b bcc:c body:d list:e",
		},
		{"(from:a OR from:b)", `(OR from="a" from="b")`, "from:a OR from:b"},
		{"label:x", `label="x"`, "label:x"},
		{`label:"带 空格"`, `label="带 空格"`, `label:"带 空格"`},
		{"label:Work", `label="Work"`, "label:Work"},
		{"deliveredto:Shop", "deliveredTo=shop|", "deliveredto:shop"},
		{"deliveredto:shop@Example.COM", "deliveredTo=shop|example.com", "deliveredto:shop@example.com"},
		{
			"is:archived IS:Downgraded is:sent has:Attachment", "(AND archived downgraded sent hasAttachment)",
			"is:archived is:downgraded is:sent has:attachment",
		},
		{"-has:attachment", "(NOT hasAttachment)", "-has:attachment"},
		{"class:Human", "class=human", "class:human"},
		{"triage:phishing:yes", "triage=phishing:yes", "triage:phishing:yes"},
		// Times: dates in the zone, RFC 3339, relative to now.
		{
			"after:2026-10-01 before:2026-11-01", "(AND after=2026-10-01T00:00:00Z before=2026-11-01T00:00:00Z)",
			"after:2026-10-01 before:2026-11-01",
		},
		{"after:2026-10-01T08:00:00+08:00", "after=2026-10-01T00:00:00Z", "after:2026-10-01T00:00:00Z"},
		{"after:-7d", "after=2026-10-25T12:30:45Z", "after:2026-10-25T12:30:45Z"},
		{
			"after:-2w before:-12h", "(AND after=2026-10-18T12:30:45Z before=2026-11-01T00:30:45Z)",
			"after:2026-10-18T12:30:45Z before:2026-11-01T00:30:45Z",
		},
		{"after:-0h", "after=2026-11-01T12:30:45Z", "after:2026-11-01T12:30:45Z"},
		// Within a group, ) ends a word.
		{"(a OR b)c", `(AND (OR text="a" text="b") text="c")`, "a OR b c"},
		{`(label:"x)" OR y)`, `(OR label="x)" text="y")`, `label:"x)" OR y`},
	}
	for _, c := range cases {
		q, err := Parse(c.q, env())
		if err != nil {
			t.Errorf("%q: %v", c.q, err)
			continue
		}
		if got := tree(q.Filter); got != c.tree {
			t.Errorf("%q: tree %s, want %s", c.q, got, c.tree)
		}
		if q.Normalized != c.echo {
			t.Errorf("%q: echo %q, want %q", c.q, q.Normalized, c.echo)
		}
		// The echo reads back as the same query.
		again, err := Parse(q.Normalized, env())
		if err != nil || tree(again.Filter) != c.tree {
			t.Errorf("%q: echo %q reads back as %s, %v", c.q, q.Normalized, tree(again.Filter), err)
		}
	}
}

func TestParseZone(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	e := env()
	e.Loc = shanghai
	q, err := Parse("after:2026-10-01 before:-1d", e)
	if err != nil {
		t.Fatal(err)
	}
	if got := tree(q.Filter); got != "(AND after=2026-09-30T16:00:00Z before=2026-10-31T12:30:45Z)" {
		t.Errorf("tree %s", got)
	}
	if q.Normalized != "after:2026-10-01 before:2026-10-31T20:30:45+08:00" {
		t.Errorf("echo %q", q.Normalized)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		q           string
		at          int
		msg         string
		unsupported bool
	}{
		{"a )", 3, ") without a matching (", false},
		{")", 1, ") without a matching (", false},
		{"(a b", 1, "( is not closed", false},
		{"x (a (b)", 3, "( is not closed", false},
		{"()", 1, "( ) holds nothing", false},
		{"OR a", 1, "OR needs a term before it", false},
		{"a OR", 3, "OR needs a term after it", false},
		{"a OR OR b", 3, "OR needs a term after it", false},
		{"(a OR)", 4, "OR needs a term after it", false},
		{`""`, 1, `"" is empty`, false},
		{"from:", 1, "from: needs a value", false},
		{`验证码 from:""`, 5, "from: needs a value", false},
		{"subject: hello", 1, "subject: needs a value", false},
		{"after:2026-13-01", 1, "after: want YYYY-MM-DD", false},
		{"会议 before:yesterday", 4, "before: want YYYY-MM-DD", false},
		{"after:-7m", 1, "after: want YYYY-MM-DD", false},
		{"after:-d", 1, "after: want", false},
		{"after:-+7d", 1, "after: want", false},
		{"after:-99999999999999999999d", 1, "too far back", false},
		{"after:-366001d", 1, "too far back", false},
		{"after:0000-01-01", 1, "out of range", false},
		{"has:code", 1, `has: takes attachment, not "code"`, false},
		{"is:starred", 1, `is: takes archived, downgraded or sent, not "starred"`, false},
		{"class:robot", 1, "class: takes machine, human or unknown", false},
		{"deliveredto:@example.com", 1, "deliveredto: takes a prefix", false},
		{"deliveredto:shop@", 1, "deliveredto: takes a prefix", false},
		{"triage:phishing:maybe", 1, "triage: want phishing:yes", false},
		// JMAP's, reserved: refused so that supporting them later
		// changes no query.
		{"is:unread", 1, "is:unread (JMAP notKeyword $seen) is reserved", true},
		{"a -minsize:100", 4, "minsize: (JMAP minSize) is reserved", true},
		{"maxsize:1", 1, "maxsize: (JMAP maxSize) is reserved", true},
		{"header:X-Foo", 1, "header: (JMAP header) is reserved", true},
		{"keyword:$flagged", 1, "keyword: (JMAP hasKeyword) is reserved", true},
		// Bounds.
		{strings.Repeat("a", MaxBytes+1), 0, "longer than 1024 bytes", false},
		{strings.Repeat("a ", MaxConditions) + "z", 2*MaxConditions + 1, "more than 32 conditions", false},
		{strings.Repeat("(", MaxDepth) + "a" + strings.Repeat(")", MaxDepth), 0, "", false},
		{strings.Repeat("(", MaxDepth+1) + "a" + strings.Repeat(")", MaxDepth+1), MaxDepth + 1, "nest deeper than 8", false},
		{strings.Repeat("-", MaxDepth+1) + "a", MaxDepth + 1, "nest deeper than 8", false},
		{"a \xff", 0, "not UTF-8 text", false},
		{"a \x00", 0, "not UTF-8 text", false},
	}
	for _, c := range cases {
		_, err := Parse(c.q, env())
		if c.msg == "" {
			if err != nil {
				t.Errorf("%.40q: %v", c.q, err)
			}
			continue
		}
		e, ok := errors.AsType[*Error](err)
		if !ok {
			t.Errorf("%.40q: %v, want an error", c.q, err)
			continue
		}
		if e.At != c.at || !strings.Contains(e.Msg, c.msg) || e.Unsupported != c.unsupported {
			t.Errorf("%.40q: %+v, want at %d %q unsupported %v", c.q, e, c.at, c.msg, c.unsupported)
		}
	}
	// Without a scorer, triage: finds nothing to filter by.
	_, err := Parse("triage:phishing:yes", Env{})
	if e, ok := errors.AsType[*Error](err); !ok || !strings.Contains(e.Msg, "no decision model") {
		t.Errorf("triage without a scorer: %v", err)
	}
}

func TestHints(t *testing.T) {
	cases := []struct {
		q    string
		want []Hint
	}{
		{"form:alice", []Hint{{"form:alice", "from:"}}},
		{"lable:x subjet:y", []Hint{{"lable:x", "label:"}, {"subjet:y", "subject:"}}},
		{"delivered:shop", []Hint{{"delivered:shop", "deliveredto:"}}},
		{"since:2026-10-01 prefix:shop tag:x", []Hint{{"since:2026-10-01", "after:"}, {"prefix:shop", "deliveredto:"}, {"tag:x", "label:"}}},
		{"hasattachment:true", []Hint{{"hasattachment:true", "has:attachment"}}},
		// Subjects and times are not misspelt operators.
		{"Re:hello AW:hallo TR:salut Fwd:x 10:30 https://example.com mailto:a@example.com", nil},
		{`"form:alice"`, nil},
	}
	for _, c := range cases {
		q, err := Parse(c.q, env())
		if err != nil {
			t.Fatalf("%q: %v", c.q, err)
		}
		if !slices.Equal(q.Hints, c.want) {
			t.Errorf("%q: hints %v, want %v", c.q, q.Hints, c.want)
		}
	}
	if got := (Hint{"form:alice", "from:"}).String(); got != "form:alice was searched as text; did you mean from:?" {
		t.Errorf("hint text %q", got)
	}
}

func TestUses(t *testing.T) {
	for q, want := range map[string]bool{
		"is:downgraded": true, "-is:downgraded": true, "a OR (b -(c is:downgraded))": true,
		"downgraded": false, "": false, `"is:downgraded"`: false,
	} {
		p, err := Parse(q, env())
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Filter.Uses(Downgraded); got != want {
			t.Errorf("%q uses downgraded: %v", q, got)
		}
	}
}

// FuzzParse: any text either parses or fails with an *Error, never
// panics, stays within the bounds, and a query that parses echoes a
// form that reads back as the same query. The seeds run in every test
// run; go test -fuzz=FuzzParse explores further.
func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"", "invoice", `"foo bar" -baz`, "a b OR c", "(a OR b) -(c d)", "from:alice subject:\"x y\"",
		"after:-7d before:2026-11-01", "deliveredto:shop@example.com label:\"带 空格\" is:archived",
		"Re:hello 10:30", `"unclosed \" \\`, "((((a))))", "-(-(-a))", "triage:phishing:yes class:human has:attachment",
		"is:unread", "minsize:1", "a)b (c", "OR", "验证码　通知", "after:2026-10-01T08:00:00+08:00",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		q, err := Parse(s, env())
		if err != nil {
			if _, ok := errors.AsType[*Error](err); !ok {
				t.Fatalf("%q: %T %v", s, err, err)
			}
			return
		}
		n, depth := measure(q.Filter, 0)
		if n > MaxConditions || depth > 2*MaxDepth+1 {
			t.Fatalf("%q: %d conditions, depth %d", s, n, depth)
		}
		again, err := Parse(q.Normalized, env())
		if err != nil {
			t.Fatalf("%q: echo %q does not parse: %v", s, q.Normalized, err)
		}
		if tree(again.Filter) != tree(q.Filter) || again.Normalized != q.Normalized {
			t.Fatalf("%q: echo %q reads back as %s, not %s", s, q.Normalized, tree(again.Filter), tree(q.Filter))
		}
	})
}

func measure(f *Filter, d int) (conds, depth int) {
	if f == nil {
		return 0, d
	}
	if f.Op == "" {
		return 1, d
	}
	depth = d
	for _, a := range f.Args {
		n, ad := measure(a, d+1)
		conds += n
		depth = max(depth, ad)
	}
	return conds, depth
}
