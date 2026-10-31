package store_test

import (
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/triage/score"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// KnownSender: earlier mail from the same domain to the same prefix
// that escaped the downgrade, and only what the viewer can see.
func TestKnownSender(t *testing.T) {
	t.Parallel()
	db, pool := storetest.New(t)
	db.EnforceAccountAccess()
	ctx := t.Context()
	open := storetest.Account(t, db)
	closed, err := db.EnsureCloudflareAccount(ctx, "closed")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := db.EnsurePrincipal(ctx, store.Principal{ID: ulid.New(time.Now()), Kind: "agent", Name: "arisu", DisplayName: "arisu", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO account_access (account_id, principal_id, can_read)
		VALUES ($1, $3, true), ($2, $3, false), ($1, $4, true), ($2, $4, true)`, open, closed, agent.ID, systemID); err != nil {
		t.Fatal(err)
	}
	restricted := &auth.Viewer{PrincipalID: agent.ID, PrincipalKind: "agent", PrincipalName: "arisu"}
	everyone := &auth.Viewer{PrincipalID: systemID, PrincipalKind: "system", PrincipalName: "system"}

	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	send := func(account, prefix, from, subject string) string {
		at = at.Add(time.Minute)
		return inbound(t, db, account, prefix, letter(from, prefix+"@example.com", subject, "", subject), at)
	}
	bulk1 := send(open, "spam", "Bulk <bulk@example.net>", "first")
	bulk2 := send(open, "spam", "other@EXAMPLE.net", "second")
	hidden := send(closed, "shop", "Shop <orders@example.org>", "hidden")
	shop := send(open, "shop", "codes@example.org", "codes")
	elsewhere := send(open, "news", "news@example.org", "elsewhere")
	if _, err := pool.Exec(ctx, `UPDATE prefixes SET state = 1 WHERE zone = 'example.com' AND prefix IN ('shop', 'news')`); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		v    *auth.Viewer
		id   string
		want bool
	}{
		{"nothing earlier", everyone, bulk1, false},
		{"earlier mail was downgraded", everyone, bulk2, false},
		{"same domain wrote to this prefix", everyone, shop, true},
		{"that mail is hidden", restricted, shop, false},
		{"another prefix", everyone, elsewhere, false},
		{"first of its domain", everyone, hidden, false},
	} {
		got, err := db.KnownSender(ctx, c.v, c.id)
		if err != nil || got != c.want {
			t.Errorf("%s: %v, %v; want %v", c.name, got, err, c.want)
		}
	}
	if _, err := db.KnownSender(ctx, restricted, hidden); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a hidden message: %v, want ErrNotFound", err)
	}

	// Rescoring pages through every target in id order, scored or not;
	// the spam prefix is unclaimed, so its mail is none.
	if err := db.InsertScore(ctx, newScore(shop, laya, at.Add(time.Hour), 0.2)); err != nil {
		t.Fatal(err)
	}
	var all []string
	for after := ""; ; {
		page, err := db.RescoreTargets(ctx, time.Time{}, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page...)
		if len(page) < 2 {
			break
		}
		after = page[len(page)-1]
	}
	if want := []string{hidden, shop, elsewhere}; !slices.Equal(all, want) {
		t.Errorf("RescoreTargets %q, want %q", all, want)
	}

	// The report counts what the viewer sees.
	for v, want := range map[*auth.Viewer]store.ScoreCounts{
		everyone:   {Scored: 1, Unscored: 2},
		restricted: {Scored: 1, Unscored: 1},
	} {
		got, err := db.ScoreCounts(ctx, v, laya, time.Time{})
		if err != nil || got.Scored != want.Scored || got.Unscored != want.Unscored {
			t.Errorf("%s: ScoreCounts %+v, %v; want %+v", v.PrincipalName, got, err, want)
		}
	}
}

// The report counts each message once, by its latest score, and the
// option with the highest probability.
func TestScoreCounts(t *testing.T) {
	t.Parallel()
	db, _ := storetest.New(t)
	ctx := t.Context()
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	everyone := &auth.Viewer{PrincipalID: systemID, PrincipalKind: "system", PrincipalName: "system"}
	a := deliver(t, db, "plain-otp.eml", nil, "a", at)
	b := deliver(t, db, "plain-otp.eml", shipped, "b", at.Add(time.Minute))
	for _, s := range []score.Score{
		newScore(a, laya, at.Add(time.Hour), 0.9),
		newScore(a, laya, at.Add(2*time.Hour), 0.1), // the latest of a
		newScore(b, laya, at.Add(time.Hour), 0.8),
		newScore(b, score.Profile{Scorer: "kev-1", Questions: 1}, at.Add(3*time.Hour), 0.1),
	} {
		if err := db.InsertScore(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.ScoreCounts(ctx, everyone, laya, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]int{
		"phishing":    {"yes": 1, "no": 1},
		"kind":        {"spam": 2},
		"needs_reply": {"": 2},
	}
	if got.Scored != 2 || !maps.EqualFunc(got.Answers, want, maps.Equal) {
		t.Errorf("%+v, want answers %v", got, want)
	}
	if got, err := db.ScoreCounts(ctx, everyone, laya, at.Add(30*time.Second)); err != nil || got.Scored != 1 {
		t.Errorf("since: %+v, %v", got, err)
	}
	if _, err := db.ScoreCounts(ctx, nil, laya, time.Time{}); err == nil {
		t.Error("nil viewer accepted")
	}

	// Lists show, and triage= finds, what the latest score of the
	// profile leans to, the way the report counts it: a's earlier yes
	// and b's score by another scorer are not current.
	tops, err := db.TopAnswers(ctx, everyone, laya, []string{a, b, ulid.New(at)})
	want2 := map[string]map[string]string{
		a: {"phishing": "no", "kind": "spam"},
		b: {"phishing": "yes", "kind": "spam"},
	}
	if err != nil || !maps.EqualFunc(tops, want2, maps.Equal) {
		t.Errorf("TopAnswers %v, %v; want %v (no abstention, no unknown id)", tops, err, want2)
	}
	if tops, err := db.TopAnswers(ctx, everyone, laya, nil); err != nil || len(tops) != 0 {
		t.Errorf("TopAnswers of none: %v, %v", tops, err)
	}
	if _, err := db.TopAnswers(ctx, nil, laya, []string{a}); err == nil {
		t.Error("TopAnswers: nil viewer accepted")
	}
	for f, want := range map[store.AnswerFilter][]string{
		{Profile: laya, Question: "phishing", Option: "yes"}:   {b},
		{Profile: laya, Question: "phishing", Option: "no"}:    {a},
		{Profile: laya, Question: "kind", Option: "spam"}:      {a, b},
		{Profile: laya, Question: "kind", Option: "marketing"}: {},
		// An abstention leans to nothing.
		{Profile: laya, Question: "needs_reply", Option: "yes"}: {},
		// b's kev-1 score says no; only the profile asked counts.
		{Profile: score.Profile{Scorer: "kev-1", Questions: 1}, Question: "phishing", Option: "no"}: {b},
	} {
		list, err := db.ListMessages(ctx, everyone, store.MessageQuery{MessageFilter: store.MessageFilter{
			Downgraded: store.IncludeDowngraded, Answer: &f,
		}})
		if err != nil {
			t.Fatal(err)
		}
		got := []string{}
		for _, m := range list {
			got = append(got, m.ID)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("ListMessages %s:%s by %s: %q, want %q", f.Question, f.Option, f.Scorer, got, want)
		}
		res, err := db.Search(ctx, everyone, store.SearchQuery{IncludeDowngraded: true, Answer: &f})
		if err != nil || res.Total != len(want) || len(res.Hits) != len(want) {
			t.Errorf("Search %s:%s by %s: %+v, %v; want %d", f.Question, f.Option, f.Scorer, res, err, len(want))
		}
	}
	bad := store.AnswerFilter{Profile: laya, Question: "phishing'--", Option: "yes"}
	if _, err := db.ListMessages(ctx, everyone, store.MessageQuery{MessageFilter: store.MessageFilter{Answer: &bad}}); !errors.Is(err, store.ErrInvalidScore) {
		t.Errorf("a malformed filter: %v", err)
	}
}

// Top is what the store's topOption picks: the highest probability,
// ties to the first by name, nothing for an abstention.
func TestAnswerTop(t *testing.T) {
	for _, c := range []struct {
		a    score.Answer
		want string
	}{
		{score.Answer{P: map[string]float64{"yes": 0.9, "no": 0.1}}, "yes"},
		{score.Answer{P: map[string]float64{"yes": 0.5, "no": 0.5}}, "no"},
		{score.Answer{P: map[string]float64{"spam": 0.3, "marketing": 0.3, "conversation": 0.2}}, "marketing"},
		{score.Answer{Abstain: true}, ""},
	} {
		if got := c.a.Top(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.a, got, c.want)
		}
	}
}
