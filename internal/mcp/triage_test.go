package mcp_test

import (
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/mcp"
	"github.com/yuuka-mail/yuuka/internal/triage/score"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// An agent reads a model's score as a labelled estimate beside yuuka's
// facts, finds mail by what it leans to, and follows new scores through
// changes_since (§15). It never sees what the scorer was shown.
func TestTriage(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	laya := score.Profile{Scorer: "laya-ml-1", Questions: 1}
	off := e.client(e.viewer(auth.MessageRead, auth.DeliveryRead))
	e.srv = mcp.New(mcp.Config{DB: e.db, Raw: e.raws, Log: quiet, Version: "test", Triage: &laya})
	cs := e.client(e.viewer(auth.MessageRead, auth.DeliveryRead))

	start := structured[mcp.ChangesOutput](t, callTool(t, cs, "changes_since", map[string]any{"kinds": []string{"triage"}}))
	phish, otp := e.ids["phish-dmarc-fail"], e.ids["plain-otp"]
	for id, p := range map[string]float64{phish: 0.95, otp: 0.02} {
		at := time.Now()
		if err := e.db.InsertScore(ctx, score.Score{
			ID: ulid.New(at), MessageID: id, Profile: laya, Model: "laya-ml 0.3", ScoredAt: at,
			Answers: map[string]score.Answer{
				"phishing": {P: map[string]float64{"yes": p, "no": 1 - p}},
				"kind":     {P: map[string]float64{"notification": 0.8, "conversation": 0.1, "marketing": 0.05, "spam": 0.05}},
			},
			Input: score.Input{Endpoint: score.EndpointLocal, SHA256: strings.Repeat("ab", 32), Facts: score.Facts{Class: "machine"}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	r := callTool(t, cs, "get_message", map[string]any{"uri": agentview.MessageURI(phish)})
	out := structured[mcp.MessageOutput](t, r)
	tr := out.Facts.Triage
	if tr == nil || tr.Notice != agentview.TriageNotice || len(tr.Answers) != 2 || tr.Answers[1].Question != "phishing" || tr.Answers[1].Top != "yes" {
		t.Fatalf("facts.triage %+v", tr)
	}
	// Labelled, among yuuka's facts, before the sender's fence; no input.
	at, fence := strings.Index(r.text, "triage: "), strings.Index(r.text, "<untrusted_mail_content")
	if at < 0 || at > fence || !strings.Contains(r.text, agentview.TriageNotice) || strings.Contains(r.text, "abab") {
		t.Errorf("get_message text:\n%s", r.text)
	}
	if out := structured[mcp.MessageOutput](t, callTool(t, cs, "get_message", map[string]any{"uri": agentview.MessageURI(e.ids["lf-no-mime"])})); out.Facts.Triage != nil {
		t.Errorf("unscored mail with a score: %+v", out.Facts.Triage)
	}
	if out := structured[mcp.MessageOutput](t, callTool(t, off, "get_message", map[string]any{"uri": agentview.MessageURI(phish)})); out.Facts.Triage != nil {
		t.Errorf("a score without a scorer configured: %+v", out.Facts.Triage)
	}

	// search_messages finds by what the latest score leans to.
	r = callTool(t, cs, "search_messages", map[string]any{"triage": "phishing:yes", "include_downgraded": true})
	res := structured[mcp.SearchOutput](t, r)
	if res.Total != 1 || res.Hits[0].URI != agentview.MessageURI(phish) || res.Hits[0].Triage["phishing"] != "yes" ||
		!strings.Contains(r.text, "model leans to kind notification, phishing yes") {
		t.Errorf("triage=phishing:yes: %+v\n%s", res, r.text)
	}
	if res := structured[mcp.SearchOutput](t, callTool(t, cs, "search_messages", map[string]any{"triage": "phishing:no"})); res.Total != 1 ||
		res.Hits[0].URI != agentview.MessageURI(otp) {
		t.Errorf("triage=phishing:no: %+v", res)
	}
	for _, c := range []struct {
		got  call
		want string
	}{
		{callTool(t, cs, "search_messages", map[string]any{"triage": "phishing:maybe"}), "no option"},
		{callTool(t, cs, "search_messages", map[string]any{"triage": "injected"}), "<question>:<option>"},
		{callTool(t, off, "search_messages", map[string]any{"triage": "phishing:yes"}), "no decision model"},
	} {
		if !c.got.isError || !strings.Contains(c.got.text, c.want) {
			t.Errorf("want an error with %q: %+v", c.want, c.got)
		}
	}

	// A score arriving is a change of kind triage, with the message to read.
	ch := structured[mcp.ChangesOutput](t, callTool(t, cs, "changes_since", map[string]any{"cursor": start.Cursor, "kinds": []string{"triage"}}))
	uris := map[string]bool{}
	for _, c := range ch.Changes {
		uris[c.URI] = c.Kind == "triage"
	}
	if len(ch.Changes) != 2 || !uris[agentview.MessageURI(phish)] || !uris[agentview.MessageURI(otp)] {
		t.Errorf("triage changes %+v", ch.Changes)
	}
}
