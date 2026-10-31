package api

import (
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/openapi"
	"github.com/yuuka-mail/yuuka/internal/triage"
	"github.com/yuuka-mail/yuuka/internal/triage/score"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// laya is the profile of the harness's scorer.
var laya = score.Profile{Scorer: "laya-ml-1", Questions: triage.QuestionsVersion}

// score stores a score of message id by laya, as the scorer writes it:
// kind leans to notification, needs_reply abstains.
func (h *harness) score(id string, phishing float64) {
	h.t.Helper()
	at := time.Now()
	if err := h.db.InsertScore(h.t.Context(), score.Score{
		ID: ulid.New(at), MessageID: id, Profile: laya, Model: "laya-ml 0.3", ScoredAt: at,
		Answers: map[string]score.Answer{
			"phishing":    {P: map[string]float64{"yes": phishing, "no": 1 - phishing}},
			"kind":        {P: map[string]float64{"notification": 0.7, "conversation": 0.1, "marketing": 0.1, "spam": 0.1}},
			"needs_reply": {Abstain: true},
		},
		Input: score.Input{
			Endpoint: score.EndpointLocal, View: 1, Parser: 1, Chars: 100, SHA256: strings.Repeat("ab", 32),
			Facts: score.Facts{Auth: "spf=pass dkim=pass dmarc=pass", Claimed: true, Class: "machine"},
		},
	}); err != nil {
		h.t.Fatal(err)
	}
}

// Scores are shown and searched by, never acted on (§15.4): the message
// carries the latest one, lists what it leans to, triage= finds mail by
// that, and only a person who sees every delivery gets what the scorer
// was shown.
func TestTriage(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	phish, plain := h.find("Urgent: confirm your account"), h.find("Invoice and logo")
	h.score(phish, 0.9)
	h.score(plain, 0.1)
	// A later score of plain by the previous question set (v1 asked
	// injection) is kept but not current: nothing below shows, lists,
	// finds or counts it.
	at := time.Now()
	if err := h.db.InsertScore(t.Context(), score.Score{
		ID: ulid.New(at), MessageID: plain, Profile: score.Profile{Scorer: laya.Scorer, Questions: laya.Questions - 1},
		Model: "laya-ml 0.3", ScoredAt: at,
		Answers: map[string]score.Answer{
			"phishing":  {P: map[string]float64{"yes": 0.95, "no": 0.05}},
			"injection": {P: map[string]float64{"yes": 0.9, "no": 0.1}},
		},
		Input: score.Input{
			Endpoint: score.EndpointLocal, View: 1, Parser: 1, Chars: 100, SHA256: strings.Repeat("ab", 32),
			Facts: score.Facts{Auth: "spf=pass dkim=pass dmarc=pass", Claimed: true, Class: "machine"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	person := h.tokenFor(h.human, auth.MessageRead, auth.DeliveryRead)
	if m := decode[Message](t, h.get("/api/v1/messages/"+plain, person)); m.TriageScore == nil || m.TriageScore.Questions != int(laya.Questions) ||
		m.Triage["phishing"] != "no" || m.Triage["injection"] != "" {
		t.Errorf("an older question set's score shown as current: %+v %v", m.TriageScore, m.Triage)
	}

	m := decode[Message](t, h.get("/api/v1/messages/"+phish, person))
	if m.TriageScore == nil || m.TriageScore.Scorer != "laya-ml-1" || m.TriageScore.Notice != agentview.TriageNotice || m.TriageScore.Input == nil ||
		m.TriageScore.Input.Facts.Auth != "spf=pass dkim=pass dmarc=pass" || len(m.TriageScore.Answers) != 3 {
		t.Fatalf("the person's score: %+v", m.TriageScore)
	}
	if want := map[string]string{"phishing": "yes", "kind": "notification"}; !maps.Equal(m.Triage, want) {
		t.Errorf("summary triage %v, want %v (the abstention left out)", m.Triage, want)
	}
	// Agents and services see the answers, never what the scorer was
	// shown; nor does a person without delivery:read.
	for who, cred := range map[string]string{
		"an agent":                       h.token(auth.MessageRead, auth.DeliveryRead),
		"a person without delivery:read": h.tokenFor(h.human, auth.MessageRead),
	} {
		m := decode[Message](t, h.get("/api/v1/messages/"+phish, cred))
		if m.TriageScore == nil || m.TriageScore.Input != nil || len(m.TriageScore.Answers) != 3 {
			t.Errorf("%s: %+v", who, m.TriageScore)
		}
		if strings.Contains(h.get("/api/v1/messages/"+phish, cred).Body.String(), "abab") {
			t.Errorf("%s: the input's hash in the response", who)
		}
	}
	if v := decode[agentview.View](t, h.get("/api/v1/messages/"+phish+"/view?level=snippet", person)); v.Facts.Triage == nil ||
		v.Facts.Triage.Answers[2].Question != "phishing" || v.Facts.Triage.Answers[2].Top != "yes" {
		t.Errorf("view triage: %+v", v.Facts.Triage)
	}
	if unscored := h.find("This week at Example"); decode[Message](t, h.get("/api/v1/messages/"+unscored, person)).TriageScore != nil {
		t.Error("a score on unscored mail")
	}

	// triage= keeps what the latest score leans to; lists carry it.
	ids := func(ms []MessageSummary) []string {
		var out []string
		for _, m := range ms {
			out = append(out, m.ID)
			if m.Triage["phishing"] == "" {
				t.Errorf("%s listed without what its score leans to: %v", m.ID, m.Triage)
			}
		}
		return out
	}
	if got := ids(decode[MessagePage](t, h.get("/api/v1/messages?downgraded=include&triage=phishing:yes", person)).Messages); !slices.Equal(got, []string{phish}) {
		t.Errorf("list triage=phishing:yes: %q, want %s", got, phish)
	}
	res := decode[SearchResult](t, h.get("/api/v1/search?downgraded=include&q=triage:phishing:no", person))
	if got := ids(res.Hits); res.Total != 1 || !slices.Equal(got, []string{plain}) {
		t.Errorf("search triage=phishing:no: %d %q, want %s", res.Total, got, plain)
	}
	for _, bad := range []string{"phishing", "phish:yes", "phishing:maybe", ":yes", "injection:yes"} {
		for _, path := range []string{"/api/v1/messages?triage=" + bad, "/api/v1/search?q=triage:" + bad} {
			w := h.get(path, person)
			if w.Code != http.StatusBadRequest || problemOf(t, w).Code != openapi.CodeInvalidParam {
				t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
			}
		}
	}

	// The report counts the latest scores of what the person sees.
	rep := decode[TriageReport](t, h.get("/api/v1/triage/report", person))
	if rep.Scorer != "laya-ml-1" || rep.Scored != 2 || rep.Caveat == "" || len(rep.Answers) < 3 {
		t.Fatalf("report %+v", rep)
	}
	byQ := map[string]TriageQuestionCount{}
	for _, a := range rep.Answers {
		byQ[a.Question] = a
	}
	if p := byQ["phishing"]; len(p.Options) != 2 || p.Options[0].Messages != 1 || byQ["needs_reply"].Abstained != 2 ||
		rep.Answers[0].Question != "kind" || byQ["injection"].Question != "" {
		t.Errorf("report answers %+v", rep.Answers)
	}

	// Without a scorer there is nothing to filter by or report on, and
	// saying so beats an empty list that reads as "the model found none".
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	off := &harness{t: t, h: New(h.spy, nil, quiet).Handler(h.svc)}
	for _, path := range []string{"/api/v1/messages?triage=phishing:yes", "/api/v1/search?q=triage:phishing:yes"} {
		if w := off.get(path, person); w.Code != http.StatusBadRequest {
			t.Errorf("%s without a scorer: %d %s", path, w.Code, w.Body.String())
		}
	}
	if w := off.get("/api/v1/triage/report", person); w.Code != http.StatusNotFound {
		t.Errorf("report without a scorer: %d %s", w.Code, w.Body.String())
	}
	// The counts say whether there is one, to anyone who reads mail, so
	// nobody has to ask the report and get the 404.
	if !decode[Counts](t, h.get("/api/v1/counts", h.token(auth.MessageRead))).Triage ||
		decode[Counts](t, off.get("/api/v1/counts", person)).Triage {
		t.Error("counts.triage does not say whether a scorer is configured")
	}
	if page := decode[MessagePage](t, off.get("/api/v1/messages?downgraded=include", person)); slices.ContainsFunc(page.Messages,
		func(m MessageSummary) bool { return m.Triage != nil }) {
		t.Error("scores listed without a scorer")
	}
}
