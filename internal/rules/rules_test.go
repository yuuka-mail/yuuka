package rules_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/rules"
	"github.com/yuuka-mail/yuuka/internal/store"
)

const archive = `[{"kind":"archive"}]`

func spec(match, actions string) string {
	return `{"v":1,"match":` + match + `,"actions":` + actions + `}`
}

func TestParseRejects(t *testing.T) {
	leaf := `{"field":"subject","op":"eq","value":"x"}`
	for name, in := range map[string]string{
		"no version":          `{"match":` + leaf + `,"actions":` + archive + `}`,
		"version 2":           `{"v":2,"match":` + leaf + `,"actions":` + archive + `}`,
		"unknown top member":  `{"v":1,"match":` + leaf + `,"actions":` + archive + `,"stop":true}`,
		"no actions":          `{"v":1,"match":` + leaf + `}`,
		"duplicate member":    spec(`{"field":"subject","op":"eq","value":"x","value":"y"}`, archive),
		"two forms":           spec(`{"all":[`+leaf+`],"any":[`+leaf+`]}`, archive),
		"leaf with extra key": spec(`{"field":"subject","op":"eq","value":"x","flags":"i"}`, archive),
		"leaf without value":  spec(`{"field":"subject","op":"eq"}`, archive),
		"field case":          spec(`{"Field":"subject","op":"eq","value":"x"}`, archive),
		"node is an array":    spec(`[`+leaf+`]`, archive),
		"all is an object":    spec(`{"all":`+leaf+`}`, archive),
		"not is null":         spec(`{"not":null}`, archive),
		"field not a string":  spec(`{"field":1,"op":"eq","value":"x"}`, archive),
		"action extra key":    spec(leaf, `[{"kind":"archive","label":"x"}]`),
		"label missing":       spec(leaf, `[{"kind":"label"}]`),
		"action not object":   spec(leaf, `["archive"]`),
		"actions not array":   spec(leaf, `{"kind":"archive"}`),
		"invalid UTF-8":       spec(`{"field":"subject","op":"eq","value":"`+"\xff"+`"}`, archive),
		"trailing data":       spec(leaf, archive) + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := rules.ParseSpec([]byte(in)); !errors.Is(err, rules.ErrInvalid) {
				t.Errorf("ParseSpec(%s) = %v, want ErrInvalid", in, err)
			}
		})
	}
}

func TestParseAcceptsWhitespace(t *testing.T) {
	if _, err := rules.ParseMatch([]byte("\n  {\"all\": [ {\"field\": \"subject\", \"op\": \"eq\", \"value\": \"x\"} ]}\n")); err != nil {
		t.Error(err)
	}
	if _, err := rules.ParseActions([]byte(" [ {\"kind\" : \"archive\"} ] ")); err != nil {
		t.Error(err)
	}
	if _, err := rules.ParseSpec([]byte("{ \"v\": 1,\n \"match\":  {\"not\": \n{\"field\": \"attachment\", \"op\": \"eq\", \"value\": true}},\n \"actions\": [ ] }")); err != nil {
		t.Error(err)
	}
}

func TestValidate(t *testing.T) {
	leaf := func(field, op string, value any) string {
		b, err := json.Marshal(map[string]any{"field": field, "op": op, "value": value})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	nest := func(depth int, inner string) string {
		for range depth {
			inner = `{"not":` + inner + `}`
		}
		return inner
	}
	many := func(n int, l string) string {
		return `{"any":[` + strings.TrimSuffix(strings.Repeat(l+",", n), ",") + `]}`
	}
	ok := leaf("subject", "contains", "x")
	re := leaf("subject", "regex", "x")
	labels := func(n int) string {
		var as []string
		for i := range n {
			as = append(as, fmt.Sprintf(`{"kind":"label","label":"l%d"}`, i))
		}
		return "[" + strings.Join(as, ",") + "]"
	}
	in := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprint("p", i)
		}
		return out
	}
	cases := []struct {
		name, match, actions string
		valid                bool
	}{
		{"leaf", ok, archive, true},
		{"depth 4", nest(3, ok), archive, true},
		{"depth 5", nest(4, ok), archive, false},
		{"32 leaves", many(32, ok), archive, true},
		{"33 leaves", many(33, ok), archive, false},
		{"8 regexes", many(8, re), archive, true},
		{"9 regexes", many(9, re), archive, false},
		{"empty any", `{"any":[]}`, archive, false},
		{"in 100", leaf("to.prefix", "in", in(100)), archive, true},
		{"in 101", leaf("to.prefix", "in", in(101)), archive, false},
		{"in empty", leaf("to.prefix", "in", []string{}), archive, false},
		{"in with empty item", leaf("to.prefix", "in", []string{""}), archive, false},
		{"value 256 bytes", leaf("subject", "eq", strings.Repeat("x", 256)), archive, true},
		{"value 257 bytes", leaf("subject", "eq", strings.Repeat("x", 257)), archive, false},
		{"empty value", leaf("subject", "contains", ""), archive, false},
		{"regex 512 bytes", leaf("subject", "regex", strings.Repeat("x", 512)), archive, true},
		{"regex 513 bytes", leaf("subject", "regex", strings.Repeat("x", 513)), archive, false},
		{"regex too many instructions", leaf("body", "regex", `[a-z]{1000}[a-z]{1000}`), archive, false},
		{"regex syntax error", leaf("subject", "regex", `(`), archive, false},
		{"regex backreference", leaf("subject", "regex", `(a)\1`), archive, false},
		{"unknown field", leaf("header.to", "eq", "x"), archive, false},
		{"op on wrong type", leaf("attachment", "contains", "x"), archive, false},
		{"regex on enum", leaf("class", "regex", "m"), archive, false},
		{"bool as string", leaf("attachment", "eq", "true"), archive, false},
		{"enum value", leaf("class", "in", []string{"machine", "unknown"}), archive, true},
		{"unknown enum value", leaf("class", "eq", "spam"), archive, false},
		{"set value", leaf("verdict", "has", "spoof"), archive, true},
		{"unknown set value", leaf("extraction", "has", "tracking"), archive, false},
		{"number value", leaf("subject", "eq", 3), archive, false},
		{"no actions", ok, `[]`, false},
		{"8 labels", ok, labels(8), true},
		{"9 labels", ok, labels(9), false},
		{"same label twice", ok, `[{"kind":"label","label":"a"},{"kind":"label","label":"a"}]`, false},
		{"labels differing in case", ok, `[{"kind":"label","label":"a"},{"kind":"label","label":"A"}]`, true},
		{"two archives", ok, `[{"kind":"archive"},{"kind":"archive"}]`, false},
		{"two classify", ok, `[{"kind":"classify","class":"machine"},{"kind":"classify","class":"human"}]`, false},
		{"classify unknown", ok, `[{"kind":"classify","class":"unknown"}]`, false},
		{"notify levels", ok, `[{"kind":"notify","level":"silent"}]`, true},
		{"notify bad level", ok, `[{"kind":"notify","level":"loud"}]`, false},
		{"forward", ok, `[{"kind":"forward","to":"a@example.com"}]`, false},
		{"send", ok, `[{"kind":"send"}]`, false},
		{"all kinds", ok, `[{"kind":"label","label":"a"},{"kind":"classify","class":"human"},{"kind":"archive"},{"kind":"notify","level":"off"}]`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := rules.ParseSpec([]byte(spec(c.match, c.actions)))
			if err == nil {
				_, err = s.Validate()
			}
			if (err == nil) != c.valid {
				t.Errorf("valid = %v, want %v (%v)", err == nil, c.valid, err)
			}
			if err != nil && !errors.Is(err, rules.ErrInvalid) {
				t.Errorf("error %v does not wrap ErrInvalid", err)
			}
		})
	}
}

func TestValidLabel(t *testing.T) {
	for label, valid := range map[string]bool{
		"aws":                     true,
		"Work stuff":              true,
		"请求":                      true,
		strings.Repeat("长", 64):   true,
		strings.Repeat("长", 65):   false,
		"":                        false,
		"$Junk":                   false,
		`\Seen`:                   false,
		" aws":                    false,
		"aws\u3000":               false,
		"a\tb":                    false,
		"a\u202eb":                false, // bidi override
		"a\u200bb":                false, // zero-width space
		"a\x00b":                  false,
		string([]byte{'a', 0xff}): false,
		"a$b":                     true,
		"aws/billing":             true,
		"\U0001F4E7 mail":         true,
		strings.Repeat("a", 64):   true,
	} {
		if err := rules.ValidLabel(label); (err == nil) != valid {
			t.Errorf("ValidLabel(%q) = %v, want valid %v", label, err, valid)
		}
	}
}

func TestParseFoldsAddressValues(t *testing.T) {
	s, err := rules.ParseSpec([]byte(spec(`{"all":[
		{"field":"from.addr","op":"in","value":["A@Example.COM"]},
		{"field":"to.zone","op":"eq","value":"Example.NET"},
		{"field":"from.addr","op":"regex","value":"^A@"},
		{"field":"subject","op":"eq","value":"Hello"}]}`, archive)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"v":1,"match":{"all":[{"field":"from.addr","op":"in","value":["a@example.com"]},` +
		`{"field":"to.zone","op":"eq","value":"example.net"},{"field":"from.addr","op":"regex","value":"^A@"},` +
		`{"field":"subject","op":"eq","value":"Hello"}]},"actions":[{"kind":"archive"}]}`
	if string(got) != want {
		t.Errorf("canonical form\n got %s\nwant %s", got, want)
	}
	// An unfolded address value built in Go is not silently accepted.
	m := rules.Match{Field: rules.FieldFromAddr, Op: rules.OpEq, Value: "A@example.com"}
	if err := rules.ValidateMatch(m); err == nil {
		t.Error("unfolded address value validated")
	}
}

func TestWarnUnverifiedFrom(t *testing.T) {
	from := `{"field":"from.org","op":"eq","value":"example.com"}`
	verified := `{"field":"from.verified","op":"eq","value":true}`
	label := `[{"kind":"label","label":"bank"}]`
	for _, c := range []struct {
		name, match, actions string
		warn                 bool
	}{
		{"from alone, label", from, label, true},
		{"from alone, notify", from, `[{"kind":"notify","level":"normal"}]`, true},
		{"from alone, archive", from, archive, false},
		{"from and verified", `{"all":[` + from + `,` + verified + `]}`, label, false},
		{"verified nested in all", `{"all":[` + from + `,{"all":[` + verified + `]}]}`, label, false},
		{"verified in only one branch of any", `{"any":[` + from + `,` + verified + `]}`, label, true},
		{"verified in every branch of any", `{"any":[{"all":[` + from + `,` + verified + `]},` + verified + `]}`, label, false},
		{"verified under not", `{"all":[` + from + `,{"not":{"not":` + verified + `}}]}`, label, true},
		{"verified false", `{"all":[` + from + `,{"field":"from.verified","op":"eq","value":false}]}`, label, true},
		{"from under not", `{"not":` + from + `}`, label, true},
		{"no from", `{"field":"to.prefix","op":"eq","value":"bank"}`, label, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, err := rules.ParseSpec([]byte(spec(c.match, c.actions)))
			if err != nil {
				t.Fatal(err)
			}
			warns, err := s.Validate()
			if err != nil {
				t.Fatal(err)
			}
			got := len(warns) == 1 && warns[0].Code == rules.WarnUnverifiedFrom
			if got != c.warn || len(warns) > 1 {
				t.Errorf("warnings %v, want unverified_from %v", warns, c.warn)
			}
		})
	}
}

func TestCheckRegexBudget(t *testing.T) {
	m := rules.Match{Any: []rules.Match{
		{Field: rules.FieldSubject, Op: rules.OpRegex, Value: "a"},
		{Not: &rules.Match{Field: rules.FieldBody, Op: rules.OpRegex, Value: "b"}},
		{Field: rules.FieldBody, Op: rules.OpContains, Value: "c"},
	}}
	if n := rules.RegexCount(m); n != 2 {
		t.Fatalf("RegexCount = %d", n)
	}
	ms := make([]rules.Match, 32)
	for i := range ms {
		ms[i] = m
	}
	if err := rules.CheckRegexBudget(ms...); err != nil {
		t.Errorf("64 regexes: %v", err)
	}
	if err := rules.CheckRegexBudget(append(ms, m)...); !errors.Is(err, rules.ErrInvalid) {
		t.Errorf("66 regexes: %v", err)
	}
}

// TestHostileInput: what a sender or a careless rule author can throw at
// the decoder and evaluator is rejected or bounded.
func TestHostileInput(t *testing.T) {
	t.Run("deep nesting does not recurse without bound", func(t *testing.T) {
		in := strings.Repeat(`{"not":`, 100000) + `{"field":"subject","op":"eq","value":"x"}` + strings.Repeat(`}`, 100000)
		if _, err := rules.ParseMatch([]byte(in)); !errors.Is(err, rules.ErrInvalid) {
			t.Errorf("ParseMatch: %v", err)
		}
		in = strings.Repeat(`{"not":`, 33) + `{"field":"subject","op":"eq","value":"x"}` + strings.Repeat(`}`, 33)
		if _, err := rules.ParseMatch([]byte(in)); !errors.Is(err, rules.ErrInvalid) {
			t.Errorf("ParseMatch past the parse depth: %v", err)
		}
	})
	t.Run("oversized JSON is rejected before decoding", func(t *testing.T) {
		in := `{"field":"subject","op":"eq","value":"` + strings.Repeat("x", rules.MaxMatchBytes) + `"}`
		if _, err := rules.ParseMatch([]byte(in)); !errors.Is(err, rules.ErrInvalid) {
			t.Errorf("ParseMatch: %v", err)
		}
	})
	t.Run("canonical size is checked on save", func(t *testing.T) {
		var kids []rules.Match
		for range 31 {
			kids = append(kids, rules.Match{Field: rules.FieldToPrefix, Op: rules.OpIn, Value: []string{strings.Repeat("p", 250), strings.Repeat("q", 250), strings.Repeat("r", 250)}})
		}
		if err := rules.ValidateMatch(rules.Match{Any: kids}); !errors.Is(err, rules.ErrInvalid) {
			t.Errorf("ValidateMatch: %v", err)
		}
	})
	t.Run("large regex programs are refused even when loaded", func(t *testing.T) {
		for _, expr := range []string{
			`[a-z]{1000}[a-z]{1000}`,
			`(?:(?:(?:a{10}){10}){10}){10}`,
			`(?i)(?:\w{100}\s{100}){10}`,
		} {
			b, err := json.Marshal(map[string]any{"field": "body", "op": "regex", "value": expr})
			if err != nil {
				t.Fatal(err)
			}
			if u := rules.Load(b, []byte(archive)).Unsupported(); len(u) == 0 {
				t.Errorf("%q loaded", expr)
			}
		}
	})
	t.Run("a pathological pattern stays linear on a huge body", func(t *testing.T) {
		m := rules.Match{Field: rules.FieldBody, Op: rules.OpRegex, Value: `(x+x+)+y`}
		p := rules.Compile(rules.Spec{Match: m, Actions: []rules.Action{{Kind: rules.KindArchive}}})
		f := &rules.Facts{Body: strings.Repeat("x", 8<<20) + "y"}
		start := time.Now()
		ok, err := p.Eval(t.Context(), f)
		if err != nil || ok {
			t.Errorf("Eval = %v, %v; the y is outside the window", ok, err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("took %v", d)
		}
	})
}

func TestDeadline(t *testing.T) {
	leaf := rules.Match{Field: rules.FieldAttachment, Op: rules.OpEq, Value: false}
	notLeaf := rules.Match{Not: &leaf}
	p := rules.Compile(rules.Spec{Match: notLeaf, Actions: []rules.Action{{Kind: rules.KindArchive}}})
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	f := &rules.Facts{Attachment: true}
	ok, err := p.Eval(ctx, f)
	if ok || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Eval past deadline = %v, %v; want false, deadline (also under not)", ok, err)
	}
	in := rules.Combine(ctx, []rules.Rule{{ID: "A", Program: p}, {ID: "B", Program: p}}, f)
	if len(in.Effects) != 0 || len(in.Hits) != 2 || !errors.Is(in.Hits[1].Err, context.DeadlineExceeded) {
		t.Errorf("Combine past deadline = %+v", in)
	}
}

func TestGeneralize(t *testing.T) {
	type cand struct {
		Leaf     string
		Selected bool
	}
	show := func(cs []rules.Candidate) []cand {
		var out []cand
		for _, c := range cs {
			b, err := json.Marshal(c.Condition)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, cand{string(b), c.Selected})
		}
		return out
	}
	eq := func(field, value string) string {
		return `{"field":"` + field + `","op":"eq","value":"` + value + `"}`
	}
	verified := `{"field":"from.verified","op":"eq","value":true}`
	for _, c := range []struct {
		name  string
		facts rules.Facts
		want  []cand
	}{
		{"verified sender", rules.Facts{
			FromAddr: "Billing@Mail.Example.com", FromVerified: true, ToPrefix: "Shop", ToZone: "example.net",
			Subject: "Invoice 42", Body: "pay now",
		}, []cand{
			{eq("from.addr", "billing@mail.example.com"), true},
			{eq("from.domain", "mail.example.com"), false},
			{eq("from.org", "example.com"), false},
			{verified, true},
			{eq("to.addr", "shop@example.net"), false},
		}},
		{"unverified sender", rules.Facts{
			FromAddr: "news@example.com", ToPrefix: "a", ToZone: "example.net",
		}, []cand{
			{eq("from.addr", "news@example.com"), true},
			{eq("from.domain", "example.com"), false},
			{eq("to.addr", "a@example.net"), false},
		}},
		{"list", rules.Facts{
			FromAddr: "someone@example.org", FromVerified: true, ListID: "Dev <dev.lists.example.org>",
			ToPrefix: "lists", ToZone: "example.net",
		}, []cand{
			{eq("list.id", "dev.lists.example.org"), true},
			{eq("from.addr", "someone@example.org"), false},
			{eq("from.domain", "example.org"), false},
			{verified, true},
			{eq("to.addr", "lists@example.net"), false},
		}},
		{"no sender", rules.Facts{FromVerified: true, ToPrefix: "a", ToZone: "example.net"}, []cand{
			{verified, false},
			{eq("to.addr", "a@example.net"), true},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cs := rules.Generalize(&c.facts)
			assertJSON(t, "candidates", show(cs), c.want)
			for _, x := range cs {
				if f := x.Condition.Field; f == rules.FieldSubject || f == rules.FieldBody {
					t.Errorf("generalized from %s", f)
				}
			}
			m, ok := rules.Draft(cs)
			if !ok {
				t.Fatal("empty draft")
			}
			s := rules.Spec{Match: m, Actions: []rules.Action{{Kind: rules.KindArchive}}}
			if _, err := s.Validate(); err != nil {
				t.Fatalf("draft does not validate: %v", err)
			}
			if ok, err := rules.Compile(s).Eval(t.Context(), &c.facts); !ok || err != nil {
				t.Errorf("draft does not match its example: %v, %v", ok, err)
			}
		})
	}
}

func TestFoldAddrMirrorsStore(t *testing.T) {
	for _, s := range []string{"A@Example.COM", "ÉCOLE@exämple.org", "Straße", "\xff@x", "İ"} {
		if rules.FoldAddr(s) != store.FoldAddr(s) {
			t.Errorf("FoldAddr(%q): rules %q, store %q", s, rules.FoldAddr(s), store.FoldAddr(s))
		}
	}
}

func TestListID(t *testing.T) {
	for in, want := range map[string]string{
		"Dev Talk <dev.lists.example.org>": "dev.lists.example.org",
		"<dev.lists.example.org>":          "dev.lists.example.org",
		" dev.lists.example.org ":          "dev.lists.example.org",
		`"a <b>" <c.example.org>`:          "c.example.org",
		"unterminated <x":                  "unterminated <x",
		"":                                 "",
	} {
		got := rules.ListID(in)
		if got != want {
			t.Errorf("ListID(%q) = %q, want %q", in, got, want)
		}
		if again := rules.ListID(got); again != got {
			t.Errorf("ListID not idempotent on %q: %q", got, again)
		}
	}
}

func TestOrgDomain(t *testing.T) {
	for in, want := range map[string]string{
		"mail.example.com":      "example.com",
		"example.com":           "example.com",
		"a.b.example.co.uk":     "example.co.uk",
		"com":                   "com",
		"":                      "",
		"[192.0.2.1]":           "[192.0.2.1]",
		"host.example.internal": "example.internal",
	} {
		if got := rules.OrgDomain(in); got != want {
			t.Errorf("OrgDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

// FuzzParse: whatever bytes arrive, decoding, validation, compilation and
// evaluation neither panic nor disagree with each other, and a valid
// rule's canonical form decodes to itself.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		spec(`{"field":"subject","op":"regex","value":"(?i)invoice"}`, archive),
		spec(`{"all":[{"field":"from.addr","op":"in","value":["A@x"]},{"not":{"field":"verdict","op":"has","value":"spoof"}}]}`,
			`[{"kind":"label","label":"x"},{"kind":"notify","level":"off"}]`),
		spec(`{"any":[{"field":"class","op":"eq","value":"machine"},{"field":"attachment","op":"eq","value":true}]}`,
			`[{"kind":"classify","class":"human"}]`),
		spec(`{"not":{"not":{"not":{"field":"body","op":"contains","value":"x"}}}}`, archive),
		spec(`{"field":"size","op":"gt","value":1e400}`, `[{"kind":"forward"}]`),
		`{"v":1,"match":{"all":[]},"actions":[]}`,
		`{"v":1}`, `[]`, `null`, `"`, ``,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := rules.ParseSpec(data)
		if err != nil {
			if !errors.Is(err, rules.ErrInvalid) {
				t.Fatalf("error %v does not wrap ErrInvalid", err)
			}
			return
		}
		p := rules.Compile(s)
		facts := &rules.Facts{FromAddr: "a@example.com", Subject: "x", Body: "x", ToPrefix: "p", ToZone: "example.com"}
		ok, evalErr := p.Eval(t.Context(), facts)
		if evalErr != nil && ok {
			t.Fatal("matched with an error")
		}
		if _, err := s.Validate(); err != nil {
			return
		}
		if len(p.Unsupported()) > 0 {
			t.Fatalf("valid rule is unsupported: %v", p.Unsupported())
		}
		canon, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		again, err := rules.ParseSpec(canon)
		if err != nil {
			t.Fatalf("canonical form %s does not parse: %v", canon, err)
		}
		canon2, err := json.Marshal(again)
		if err != nil {
			t.Fatal(err)
		}
		if string(canon) != string(canon2) {
			t.Fatalf("canonical form unstable:\n%s\n%s", canon, canon2)
		}
	})
}

// BenchmarkWorstRegex is the cost of the most expensive single leaf v1
// allows: a regex at the instruction cap over a full window.
func BenchmarkWorstRegex(b *testing.B) {
	expr := `[a-z]{999}[a-z]{900}0`
	p := rules.Compile(rules.Spec{
		Match:   rules.Match{Field: rules.FieldBody, Op: rules.OpRegex, Value: expr},
		Actions: []rules.Action{{Kind: rules.KindArchive}},
	})
	if u := p.Unsupported(); len(u) > 0 {
		b.Fatal(u)
	}
	f := &rules.Facts{Body: strings.Repeat("a", rules.RegexWindow)}
	for b.Loop() {
		if _, err := p.Eval(b.Context(), f); err != nil {
			b.Fatal(err)
		}
	}
}
