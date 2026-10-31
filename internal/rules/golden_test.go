package rules_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuuka-mail/yuuka/internal/classify"
	"github.com/yuuka-mail/yuuka/internal/rules"
)

// goldenFacts is the testdata spelling of rules.Facts, keyed by the rule
// field names so the files read like the rules they test.
type goldenFacts struct {
	FromAddr      string         `json:"from.addr"`
	FromVerified  bool           `json:"from.verified"`
	ToPrefix      string         `json:"to.prefix"`
	ToZone        string         `json:"to.zone"`
	PrefixClaimed bool           `json:"prefix.claimed"`
	Verdict       []string       `json:"verdict"`
	Subject       string         `json:"subject"`
	Body          jsontext.Value `json:"body"`
	ListID        string         `json:"list.id"`
	Class         string         `json:"class"`
	Extraction    []string       `json:"extraction"`
	Attachment    bool           `json:"attachment"`
}

func (g goldenFacts) facts(t *testing.T) *rules.Facts {
	t.Helper()
	f := &rules.Facts{
		FromAddr: g.FromAddr, FromVerified: g.FromVerified,
		ToPrefix: g.ToPrefix, ToZone: g.ToZone, PrefixClaimed: g.PrefixClaimed,
		Subject: g.Subject, ListID: g.ListID,
		Extractions: g.Extraction, Attachment: g.Attachment,
	}
	for _, v := range g.Verdict {
		switch v {
		case rules.VerdictLeak:
			f.Leak = true
		case rules.VerdictSpoof:
			f.Spoof = true
		default:
			t.Fatalf("verdict %q", v)
		}
	}
	switch g.Class {
	case "", rules.ClassUnknown:
	case rules.ClassMachine:
		f.Class = classify.Machine
	case rules.ClassHuman:
		f.Class = classify.Human
	default:
		t.Fatalf("class %q", g.Class)
	}
	f.Body = goldenBody(t, g.Body)
	return f
}

func goldenBody(t *testing.T, v jsontext.Value) string {
	t.Helper()
	if v == nil {
		return ""
	}
	if v.Kind() == '"' {
		var s string
		mustUnmarshal(t, v, &s)
		return s
	}
	var pieces []jsontext.Value
	mustUnmarshal(t, v, &pieces)
	var b strings.Builder
	for _, p := range pieces {
		if p.Kind() == '"' {
			var s string
			mustUnmarshal(t, p, &s)
			b.WriteString(s)
			continue
		}
		var r struct {
			Repeat string `json:"repeat"`
			Count  int    `json:"count"`
		}
		mustUnmarshal(t, p, &r)
		b.WriteString(strings.Repeat(r.Repeat, r.Count))
	}
	return b.String()
}

func mustUnmarshal(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v, json.RejectUnknownMembers(true)); err != nil {
		t.Fatal(err)
	}
}

func goldenFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "v1", dir, "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no golden files in %s: %v", dir, err)
	}
	out := map[string][]byte{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(p)] = b
	}
	return out
}

func outcome(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, rules.ErrUnsupported):
		return "unsupported"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "deadline"
	}
	return err.Error()
}

// TestGoldenEval pins what a stored match tree means. The cases are
// append-only (testdata/v1/README.md).
func TestGoldenEval(t *testing.T) {
	for file, data := range goldenFiles(t, "eval") {
		var doc struct {
			Cases []struct {
				Name  string         `json:"name"`
				Match jsontext.Value `json:"match"`
				Facts goldenFacts    `json:"facts"`
				Want  string         `json:"want"`
			} `json:"cases"`
		}
		mustUnmarshal(t, data, &doc)
		for _, c := range doc.Cases {
			t.Run(file+"/"+c.Name, func(t *testing.T) {
				p := rules.Load(c.Match, []byte(`[{"kind":"archive"}]`))
				ok, err := p.Eval(t.Context(), c.Facts.facts(t))
				got := "no_match"
				switch {
				case err != nil:
					got = outcome(err)
				case ok:
					got = "match"
				}
				if got != c.Want {
					t.Errorf("got %s, want %s (err %v)", got, c.Want, err)
				}
			})
		}
	}
}

// TestGoldenCombine pins how matching rules resolve into one intent.
func TestGoldenCombine(t *testing.T) {
	type effect struct {
		Rule   string       `json:"rule"`
		Action rules.Action `json:"action"`
	}
	type hit struct {
		Rule  string `json:"rule"`
		Error string `json:"error"`
	}
	for file, data := range goldenFiles(t, "combine") {
		var doc struct {
			Cases []struct {
				Name  string `json:"name"`
				Rules []struct {
					ID       string         `json:"id"`
					Priority int            `json:"priority"`
					Match    jsontext.Value `json:"match"`
					Actions  jsontext.Value `json:"actions"`
				} `json:"rules"`
				Facts goldenFacts `json:"facts"`
				Want  struct {
					Effects []effect `json:"effects"`
					Hits    []hit    `json:"hits"`
				} `json:"want"`
			} `json:"cases"`
		}
		mustUnmarshal(t, data, &doc)
		for _, c := range doc.Cases {
			t.Run(file+"/"+c.Name, func(t *testing.T) {
				var rs []rules.Rule
				for _, r := range c.Rules {
					rs = append(rs, rules.Rule{ID: r.ID, Priority: r.Priority, Program: rules.Load(r.Match, r.Actions)})
				}
				in := rules.Combine(t.Context(), rs, c.Facts.facts(t))
				var effects []effect
				for _, e := range in.Effects {
					effects = append(effects, effect{e.Rule, e.Action})
				}
				var hits []hit
				for _, h := range in.Hits {
					hits = append(hits, hit{h.Rule, outcome(h.Err)})
				}
				assertJSON(t, "effects", effects, c.Want.Effects)
				assertJSON(t, "hits", hits, c.Want.Hits)
			})
		}
	}
}

func assertJSON(t *testing.T, what string, got, want any) {
	t.Helper()
	g, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	w, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(g) != string(w) {
		t.Errorf("%s:\n got %s\nwant %s", what, g, w)
	}
}
