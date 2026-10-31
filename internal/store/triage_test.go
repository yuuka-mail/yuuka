package store_test

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/triage/score"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

var (
	insertScores = regexp.MustCompile(`(?i)insert\s+into\s+(\w+\.)?"?triage_scores"?[\s(]`)
	// Anything else that changes rows: UPDATE, DELETE, TRUNCATE, or a
	// MERGE into the table.
	rewriteScores = regexp.MustCompile(`(?i)(update|delete\s+from|truncate(\s+table)?|merge\s+into)\s+(only\s+)?(\w+\.)?"?triage_scores"?\b`)
)

// TestOneWayIntoScores: scores are observed facts, appended and never
// changed (§15.2, §4.2). The only SQL in the module that writes
// triage_scores is the INSERT of store.InsertScore, which logs the
// change in the same transaction; nothing updates or deletes a row, and
// reparse therefore cannot touch one. A new writer fails here.
func TestOneWayIntoScores(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var inserts, rewrites []string
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// As in TestOneWayToTheOutbox: what ./... leaves out, so
			// worktrees under .claude/ are not taken for this module.
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
				name == "testdata" || name == "node_modules" || name == "local") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, decl := range f.Decls {
			where := rel + ": package level"
			if fd, ok := decl.(*ast.FuncDecl); ok {
				where = rel + ": " + funcName(fd)
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				if x, ok := n.(*ast.BasicLit); ok && x.Kind == token.STRING {
					if insertScores.MatchString(x.Value) {
						inserts = append(inserts, where)
					}
					if rewriteScores.MatchString(x.Value) {
						rewrites = append(rewrites, where)
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"internal/store/triage.go: (*DB).InsertScore"}; !slices.Equal(inserts, want) {
		t.Errorf("INSERT INTO triage_scores: %q, want %q", inserts, want)
	}
	if len(rewrites) > 0 {
		t.Errorf("SQL that changes or removes scores: %q", rewrites)
	}
}

var laya = score.Profile{Scorer: "laya-ml-1", Questions: 1}

// newScore is a well-formed score of message by profile p at at.
func newScore(message string, p score.Profile, at time.Time, phishing float64) score.Score {
	return score.Score{
		ID: ulid.New(at), MessageID: message, Profile: p, Model: "laya-ml 0.3", ScoredAt: at,
		Answers: map[string]score.Answer{
			"phishing":    {P: map[string]float64{"yes": phishing, "no": 1 - phishing}},
			"kind":        {P: map[string]float64{"notification": 0.1, "conversation": 0.1, "marketing": 0.2, "spam": 0.6}},
			"needs_reply": {Abstain: true},
		},
		Input: score.Input{
			Endpoint: score.EndpointLocal, View: 1, Parser: 1, Chars: 120,
			SHA256: strings.Repeat("ab", 32),
			Facts:  score.Facts{Auth: "spf=pass dkim=pass dmarc=pass", Claimed: true, Class: "machine", HasCode: true},
		},
	}
}

// Scores are appended, read back latest first per profile, logged as
// triage changes, and act on nothing: v1 observes only (§15.4).
func TestTriageScores(t *testing.T) {
	t.Parallel()
	e := newRuleEnv(t)
	ctx := t.Context()
	e.claim("a")
	claimed := e.deliver("a", letter("billing@example.net", "a@example.com", "Invoice", "", "pay here")).MessageID
	// Unclaimed: §4.3 hides it, so asking a model is not worth it.
	hidden := e.deliver("zz", letter("x@example.org", "zz@example.com", "Hello", "", "spam")).MessageID
	since := e.now.Add(-time.Hour)

	targets, err := e.db.ScoreTargets(ctx, laya, since, 10)
	if err != nil || !slices.Equal(targets, []string{claimed}) {
		t.Fatalf("ScoreTargets %q, %v, want only the claimed one", targets, err)
	}
	if got, err := e.db.ScoreTargets(ctx, laya, e.now.Add(time.Minute), 10); err != nil || len(got) != 0 {
		t.Errorf("ScoreTargets after the lookback: %q, %v", got, err)
	}
	if _, err := e.db.ScoreTargets(ctx, score.Profile{Scorer: "Laya!", Questions: 1}, since, 10); !errors.Is(err, store.ErrInvalidScore) {
		t.Errorf("ScoreTargets with a bad scorer name: %v", err)
	}

	start, err := e.db.Changes(ctx, person, store.ChangeQuery{})
	if err != nil {
		t.Fatal(err)
	}
	first := newScore(claimed, laya, e.tick(), 0.99)
	if err := e.db.InsertScore(ctx, first); err != nil {
		t.Fatal(err)
	}
	// Once scored, it is no target of that profile, still one of another.
	if got, err := e.db.ScoreTargets(ctx, laya, since, 10); err != nil || len(got) != 0 {
		t.Errorf("ScoreTargets after scoring: %q, %v", got, err)
	}
	if got, err := e.db.ScoreTargets(ctx, score.Profile{Scorer: "laya-ml-1", Questions: 2}, since, 10); err != nil || !slices.Equal(got, []string{claimed}) {
		t.Errorf("ScoreTargets of a new question set: %q, %v", got, err)
	}
	cs, _ := drain(t, e.db, person, start.Cursor, store.ChangeQuery{Kinds: []string{"triage"}})
	if len(cs) != 1 || cs[0].ID != claimed {
		t.Errorf("triage changes %+v, want one for %s", cs, claimed)
	}

	// Rescoring appends; the read gives the latest of the profile asked.
	second := newScore(claimed, laya, e.tick(), 0.2)
	if err := e.db.InsertScore(ctx, second); err != nil {
		t.Fatal(err)
	}
	other := newScore(claimed, score.Profile{Scorer: "kev-2b", Questions: 1}, e.tick(), 0.5)
	if err := e.db.InsertScore(ctx, other); err != nil {
		t.Fatal(err)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM triage_scores WHERE message_id = $1`, claimed); n != 3 {
		t.Errorf("%d scores kept, want all 3", n)
	}
	got, err := e.db.LatestScore(ctx, person, claimed, laya)
	if err != nil || got == nil {
		t.Fatalf("LatestScore: %v, %v", got, err)
	}
	if got.ID != second.ID || got.Model != second.Model || got.Answers["phishing"].P["yes"] != 0.2 ||
		!got.Answers["needs_reply"].Abstain || got.InputWithheld || got.Input != second.Input || !got.ScoredAt.Equal(second.ScoredAt) {
		t.Errorf("LatestScore %+v, want %+v", got, second)
	}
	if got, err := e.db.LatestScore(ctx, person, hidden, laya); err != nil || got != nil {
		t.Errorf("LatestScore of unscored mail: %+v, %v", got, err)
	}
	if _, err := e.db.LatestScore(ctx, person, ulid.New(e.now), laya); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LatestScore of no message: %v", err)
	}

	// Observation only: however sure the model is, the mail stays where
	// §4.3 put it.
	if err := e.db.InsertScore(ctx, newScore(claimed, laya, e.tick(), 1)); err != nil {
		t.Fatal(err)
	}
	if d, err := e.db.MessageByID(ctx, person, claimed); err != nil || d.Downgraded {
		t.Errorf("a score downgraded the message: %v, %v", d.Downgraded, err)
	}

	// The store keeps only what it can vouch for the shape of.
	bad := map[string]func(*score.Score){
		"no id":        func(s *score.Score) { s.ID = "" },
		"bad scorer":   func(s *score.Score) { s.Scorer = "Laya ML" },
		"no version":   func(s *score.Score) { s.Questions = 0 },
		"no model":     func(s *score.Score) { s.Model = "" },
		"no answers":   func(s *score.Score) { s.Answers = nil },
		"bad question": func(s *score.Score) { s.Answers = map[string]score.Answer{"Phishing!": {Abstain: true}} },
		"bad option":   func(s *score.Score) { s.Answers = map[string]score.Answer{"kind": {P: map[string]float64{"$.x": 0.5}}} },
		"p above 1": func(s *score.Score) {
			s.Answers = map[string]score.Answer{"kind": {P: map[string]float64{"spam": 1.5}}}
		},
		"both": func(s *score.Score) {
			s.Answers = map[string]score.Answer{"kind": {P: map[string]float64{"spam": 0.5}, Abstain: true}}
		},
		"neither":        func(s *score.Score) { s.Answers = map[string]score.Answer{"kind": {}} },
		"bad endpoint":   func(s *score.Score) { s.Input.Endpoint = "cloud" },
		"bad hash":       func(s *score.Score) { s.Input.SHA256 = "abc" },
		"bad class":      func(s *score.Score) { s.Input.Facts.Class = "spam" },
		"long auth line": func(s *score.Score) { s.Input.Facts.Auth = strings.Repeat("x", 301) },
		"no time":        func(s *score.Score) { s.ScoredAt = time.Time{} },
	}
	for name, edit := range bad {
		s := newScore(claimed, laya, e.tick(), 0.5)
		edit(&s)
		if err := e.db.InsertScore(ctx, s); !errors.Is(err, store.ErrInvalidScore) {
			t.Errorf("%s: %v, want ErrInvalidScore", name, err)
		}
	}
	if err := e.db.InsertScore(ctx, newScore(ulid.New(e.now), laya, e.tick(), 0.5)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("score of no message: %v", err)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM triage_scores`); n != 4 {
		t.Errorf("%d scores, want the 4 valid ones", n)
	}
}
