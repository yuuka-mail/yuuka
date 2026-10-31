package triage

import (
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/triage/score"
)

func TestWriteReport(t *testing.T) {
	var out strings.Builder
	err := WriteReport(&out, score.Profile{Scorer: "laya-ml-1", Questions: 1}, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), store.ScoreCounts{
		Scored: 12, Unscored: 3,
		Answers: map[string]map[string]int{
			Kind:      {KindSpam: 2, KindNotification: 9, "": 1},
			Injection: {No: 12},
			"tone":    {"rude": 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, s := range []string{
		"scorer laya-ml-1, questions v1, mail since 2026-10-01T00:00:00Z: 12 scored, 3 to score",
		"kind\n  notification  9\n  spam          2\n  (abstained)   1\n",
		"phishing\n  (no answers)",
		"tone\n  rude",
		"no person has judged these",
	} {
		if !strings.Contains(got, s) {
			t.Errorf("report lacks %q:\n%s", s, got)
		}
	}
	if strings.Index(got, "needs_reply") > strings.Index(got, "tone") {
		t.Error("questions outside the set before those in it")
	}
}
