package api

import (
	"cmp"
	"context"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/openapi"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/triage"
	"github.com/yuuka-mail/yuuka/internal/triage/score"
)

// Triage (§15): the scores of the configured scorer, shown and acted on
// by nothing. The message carries the latest score, lists what it leans
// to, and triage= finds mail by that. Only the active profile's scores
// count; with no scorer configured there are none to show.

// TriageScore is the latest score of a message (§15.2): a decision
// model's observation, not yuuka's verdict, and nothing acts on it.
type TriageScore struct {
	agentview.Triage
	// Input is what the scorer was shown, never the content itself. It
	// records delivery facts, so it is given to a person with
	// delivery:read who sees every delivery of the message, and to no
	// one else.
	Input *TriageInput `json:"input,omitempty"`
}

// TriageInput records what the scorer read: the facts it was given, how
// much text, a hash of the text, and whether the text left the
// deployment.
type TriageInput struct {
	Endpoint TriageEndpoint `json:"endpoint"`
	// View and Parser are the agent view and parser versions the text
	// was rendered with.
	View   int `json:"view"`
	Parser int `json:"parser"`
	// Chars is how many characters of text were sent; Truncated whether
	// the text was cut to fit.
	Chars     int              `json:"chars"`
	Truncated bool             `json:"truncated"`
	SHA256    string           `json:"sha256"`
	Facts     TriageInputFacts `json:"facts"`
}

// TriageEndpoint is where the scorer was: local (the deployment) or
// hosted (a third party received the mail).
type TriageEndpoint string

func (TriageEndpoint) Enum() []string { return []string{score.EndpointLocal, score.EndpointHosted} }

// TriageInputFacts are the facts the scorer was given, as they were
// when it scored: they change, a prefix claimed later for one.
type TriageInputFacts struct {
	Auth         string          `json:"auth"`
	Claimed      bool            `json:"claimed"`
	KnownSender  bool            `json:"known_sender"`
	ReplyToOther bool            `json:"reply_to_other"`
	ListID       bool            `json:"list_id"`
	Class        agentview.Class `json:"class"`
	HasCode      bool            `json:"has_code"`
	HasLink      bool            `json:"has_link"`
}

// TriageFilter is a triage= parameter, <question>:<option> of the
// question set (triage.ParseFilter).
type TriageFilter struct{ Question, Option string }

func (f *TriageFilter) UnmarshalText(b []byte) (err error) {
	f.Question, f.Option, err = triage.ParseFilter(string(b))
	return err
}

// answerFilter is the store filter of f, nil for none. While no scorer
// is configured a filter is refused: finding nothing would read as "the
// model found none".
func (a *API) answerFilter(f *TriageFilter) (*store.AnswerFilter, error) {
	if f == nil {
		return nil, nil
	}
	if a.triage == nil {
		return nil, openapi.Errorf(http.StatusBadRequest, openapi.CodeInvalidParam, "triage: no scorer is configured, so no mail is scored")
	}
	return &store.AnswerFilter{Profile: *a.triage, Question: f.Question, Option: f.Option}, nil
}

// tops is what the latest scores of ids lean to (TopAnswers), nil
// without a scorer; one read for a page.
func (a *API) tops(ctx context.Context, v *auth.Viewer, ids []string) (map[string]map[string]string, error) {
	if a.triage == nil || len(ids) == 0 {
		return nil, nil
	}
	return a.db.TopAnswers(ctx, v, *a.triage, ids)
}

// latestScore is the latest score of message id, nil without a scorer
// or a score.
func (a *API) latestScore(ctx context.Context, v *auth.Viewer, id string) (*store.ScoreRead, error) {
	if a.triage == nil {
		return nil, nil
	}
	return a.db.LatestScore(ctx, v, id, *a.triage)
}

// topsOf is the summary's form of s: the option each answer leans to,
// abstentions left out.
func topsOf(s *store.ScoreRead) map[string]string {
	if s == nil {
		return nil
	}
	out := map[string]string{}
	for q, ans := range s.Answers {
		if t := ans.Top(); t != "" {
			out[q] = t
		}
	}
	return out
}

// triageOut is s as v reads it: the input only for a person who may see
// every delivery fact it records.
func triageOut(v *auth.Viewer, s *store.ScoreRead) *TriageScore {
	if s == nil {
		return nil
	}
	out := &TriageScore{Triage: *agentview.TriageOf(s)}
	if isPerson(v) && v.Has(auth.DeliveryRead) && !s.InputWithheld {
		in, f := s.Input, s.Input.Facts
		out.Input = &TriageInput{
			Endpoint: TriageEndpoint(in.Endpoint), View: in.View, Parser: in.Parser, Chars: in.Chars,
			Truncated: in.Truncated, SHA256: in.SHA256,
			Facts: TriageInputFacts{
				Auth: f.Auth, Claimed: f.Claimed, KnownSender: f.KnownSender, ReplyToOther: f.ReplyToOther,
				ListID: f.ListID, Class: agentview.Class(f.Class), HasCode: f.HasCode, HasLink: f.HasLink,
			},
		}
	}
	return out
}

// TriageReportParams are the parameters of GET /api/v1/triage/report.
type TriageReportParams struct {
	Since *time.Time `query:"since" jsonschema:"only mail received at or after this time (RFC 3339); absent is all"`
}

// TriageReport is how the latest scores of the configured scorer fall
// over the mail the viewer sees: `yuuka triage report` (§15.5). Counts
// only.
type TriageReport struct {
	Scorer    string     `json:"scorer"`
	Questions int        `json:"questions"`
	Since     *time.Time `json:"since,omitempty"`
	// Scored messages have a score of this scorer; Unscored are mail it
	// would score (that escapes the §4.3 downgrade) without one.
	Scored   int `json:"scored"`
	Unscored int `json:"unscored"`
	// Answers are by question, in the question set's order.
	Answers []TriageQuestionCount `json:"answers"`
	// Caveat says what the counts are not.
	Caveat string `json:"caveat"`
}

// TriageQuestionCount counts the answers to one question by the option
// each message's latest score gave the highest probability.
type TriageQuestionCount struct {
	Question string `json:"question"`
	// Options are most frequent first.
	Options   []TriageOptionCount `json:"options"`
	Abstained int                 `json:"abstained"`
}

// TriageOptionCount is how many messages leaned to one option.
type TriageOptionCount struct {
	Option   string `json:"option"`
	Messages int    `json:"messages"`
}

func (a *API) triageReport(ctx context.Context, v *auth.Viewer, p TriageReportParams) (TriageReport, error) {
	if err := peopleOnly(v, "reads the triage report"); err != nil {
		return TriageReport{}, err
	}
	if a.triage == nil {
		return TriageReport{}, openapi.Errorf(http.StatusNotFound, openapi.CodeNotFound, "no scorer is configured")
	}
	var since time.Time
	if p.Since != nil {
		since = *p.Since
	}
	c, err := a.db.ScoreCounts(ctx, v, *a.triage, since)
	if err != nil {
		return TriageReport{}, err
	}
	out := TriageReport{
		Scorer: a.triage.Scorer, Questions: int(a.triage.Questions), Since: p.Since,
		Scored: c.Scored, Unscored: c.Unscored, Answers: []TriageQuestionCount{}, Caveat: triage.Caveat,
	}
	for _, q := range triage.Order(c.Answers) {
		counts := c.Answers[q]
		qc := TriageQuestionCount{Question: q, Options: []TriageOptionCount{}, Abstained: counts[""]}
		opts := slices.SortedFunc(maps.Keys(counts), func(x, y string) int {
			return cmp.Or(cmp.Compare(counts[y], counts[x]), cmp.Compare(x, y))
		})
		for _, o := range opts {
			if o != "" {
				qc.Options = append(qc.Options, TriageOptionCount{Option: o, Messages: counts[o]})
			}
		}
		out.Answers = append(out.Answers, qc)
	}
	return out, nil
}
