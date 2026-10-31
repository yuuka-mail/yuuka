package api

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/contenthash"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/openapi"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// deliver ingests a corpus letter, edited, to prefix@example.com as
// mail arriving now: rules run on it, and its raw is there to read. It
// returns the message id.
func (h *harness) deliver(file string, edit *strings.Replacer, prefix string) string {
	h.t.Helper()
	ctx := h.t.Context()
	raw, err := os.ReadFile("../../testdata/eml/" + file)
	if err != nil {
		h.t.Fatal(err)
	}
	raw = []byte(edit.Replace(string(raw)))
	account, err := h.db.EnsureCloudflareAccount(ctx, "devseed")
	if err != nil {
		h.t.Fatal(err)
	}
	at := time.Now()
	id := ulid.New(at)
	key := "raw/test/" + id + ".eml"
	in := store.Ingest{
		RawID: id, R2Key: key, LocalPath: key, Size: int64(len(raw)), SHA256: sha256.Sum256(raw),
		ContentHash: contenthash.V1(raw), ReceivedAt: at, DeliveryID: id, AccountID: account,
		Zone: "example.com", Prefix: prefix, EnvelopeTo: prefix + "@example.com",
	}
	if err := h.local.Put(key, raw); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.db.CommitIngest(ctx, in); err != nil {
		h.t.Fatal(err)
	}
	f, err := h.db.Fold(ctx, store.RawRef{
		DeliveryID: id, RawID: id, R2Key: key, LocalPath: key, ContentHash: in.ContentHash[:],
		Zone: "example.com", Prefix: prefix, ReceivedAt: at,
	}, mimeparse.Parse(raw), authres.Extract(raw, "mx.cloudflare.net"))
	if err != nil {
		h.t.Fatal(err)
	}
	return f.MessageID
}

// post sends body to path and decodes a 2xx answer as T.
func post[T any](t *testing.T, h *harness, path, body, bearer string, want int) T {
	t.Helper()
	w := h.do(http.MethodPost, path, body, bearer)
	if w.Code != want {
		t.Fatalf("POST %s: %d %s", path, w.Code, w.Body.String())
	}
	return decode[T](t, w)
}

func kinds(os []AnnotationOutcome) []agentview.AnnotateOutcome {
	out := make([]agentview.AnnotateOutcome, len(os))
	for i, o := range os {
		out[i] = o.Outcome
	}
	return out
}

// An agent annotates directly; a person overrides; the agent's later
// annotation of the same thing is overruled, a result and not an error;
// the history says who did what, to each only what it may see; undo
// takes back what is still the agent's.
func TestAnnotations(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id := h.find("This week at Example")
	agent := h.token(auth.MessageAnnotate, auth.MessageRead)
	person := h.tokenFor(h.human, auth.MessageAnnotate, auth.MessageRead)
	path := "/api/v1/messages/" + id + "/annotations"

	res := post[AnnotateResult](t, h, path, `{"actions":[{"kind":"label","label":"news"},{"kind":"archive"}]}`, agent, http.StatusOK)
	if res.ActionID == "" || len(res.Messages) != 1 || res.Messages[0].MessageID != id ||
		!slices.Equal(kinds(res.Messages[0].Outcomes), []agentview.AnnotateOutcome{"applied", "applied"}) {
		t.Fatalf("agent annotates: %+v", res)
	}
	undo := res.ActionID
	if m := decode[Message](t, h.get("/api/v1/messages/"+id, agent)); !slices.Equal(m.Labels, []string{"news"}) || !m.Archived {
		t.Errorf("message after annotating: %q %v", m.Labels, m.Archived)
	}
	listed := func(query string) bool {
		page := decode[MessagePage](t, h.get("/api/v1/messages?downgraded=include&limit=500&"+query, agent))
		return slices.ContainsFunc(page.Messages, func(m MessageSummary) bool { return m.ID == id })
	}
	if !listed("label=news&archived=true") || listed("label=news&archived=false") || listed("label=News") {
		t.Error("label and archived filters of the list")
	}
	hits := decode[SearchResult](t, h.get("/api/v1/search?downgraded=include&q="+url.QueryEscape(`label:news "This week at Example"`), agent))
	if len(hits.Hits) != 1 || !slices.Equal(hits.Hits[0].Labels, []string{"news"}) || !hits.Hits[0].Archived {
		t.Errorf("search with label: %+v", hits)
	}
	if ls := decode[LabelList](t, h.get("/api/v1/labels", agent)); !slices.Equal(ls.Labels, []LabelCount{{Label: "news", Messages: 1}}) {
		t.Errorf("labels: %+v", ls)
	}

	// The person brings it back; the agent cannot archive it again.
	res = post[AnnotateResult](t, h, path, `{"actions":[{"kind":"unarchive"}]}`, person, http.StatusOK)
	if res.ActionID == "" || res.Messages[0].Outcomes[0].Outcome != "applied" {
		t.Fatalf("person overrides: %+v", res)
	}
	res = post[AnnotateResult](t, h, path, `{"actions":[{"kind":"archive"},{"kind":"label","label":"news"}]}`, agent, http.StatusOK)
	if res.ActionID != "" || !slices.Equal(kinds(res.Messages[0].Outcomes), []agentview.AnnotateOutcome{"overruled", "unchanged"}) {
		t.Errorf("agent after the person: %+v", res)
	}

	hist := decode[AnnotationHistory](t, h.get(path, person))
	if len(hist.History) != 3 {
		t.Fatalf("history as the person: %+v", hist)
	}
	if e := hist.History[0]; e.Change != (agentview.Annotation{Kind: "label", Label: "news"}) || e.Rank != "agent" ||
		e.By != (Actor{ID: h.agent.ID, Kind: "agent", Name: "arisu"}) || e.Tool != "message.annotate" || e.ActionID != undo {
		t.Errorf("first change: %+v", e)
	}
	if e := hist.History[2]; e.Change.Kind != "unarchive" || e.Rank != "person" || e.By.Kind != "human" {
		t.Errorf("the person's change: %+v", e)
	}
	// The audit log is the agent's own only (§13.8).
	if mine := decode[AnnotationHistory](t, h.get(path, agent)); len(mine.History) != 2 || mine.History[1].Change.Kind != "archive" {
		t.Errorf("history as the agent: %+v", mine)
	}

	// Undo: the label is still the agent's, archived is the person's.
	u := post[UndoResult](t, h, "/api/v1/actions/"+undo+"/undo", "", agent, http.StatusOK)
	if u.Reverted != 1 || u.Superseded != 1 {
		t.Errorf("undo: %+v", u)
	}
	if m := decode[Message](t, h.get("/api/v1/messages/"+id, agent)); len(m.Labels) != 0 || m.Archived {
		t.Errorf("message after undo: %q %v", m.Labels, m.Archived)
	}
	hist = decode[AnnotationHistory](t, h.get(path, person))
	if last := hist.History[len(hist.History)-1]; hist.History[0].UndoneBy != last.Seq || last.Undoes != hist.History[0].Seq {
		t.Errorf("undo in the history: %+v", hist.History)
	}

	// A thread at once, as one undoable action.
	lunch := h.find("Re: Lunch on Friday?")
	th := post[AnnotateResult](t, h, "/api/v1/messages/"+lunch+"/annotations", `{"scope":"thread","actions":[{"kind":"classify","class":"human"},{"kind":"label","label":"lunch"}]}`, agent, http.StatusOK)
	if th.ActionID == "" || len(th.Messages) != 3 {
		t.Errorf("thread: %+v", th)
	}
	if u := post[UndoResult](t, h, "/api/v1/actions/"+th.ActionID+"/undo", "", agent, http.StatusOK); u.Reverted < 3 {
		t.Errorf("thread undo: %+v", u)
	}

	for _, bad := range []string{
		`{"actions":[]}`,
		`{"actions":[{"kind":"label","label":"$Seen"}]}`,
		`{"actions":[{"kind":"classify"}]}`,
		`{"actions":[{"kind":"archive","label":"x"}]}`,
		`{"actions":[{"kind":"forward"}]}`,
	} {
		if w := h.do(http.MethodPost, path, bad, agent); w.Code != http.StatusBadRequest || problemOf(t, w).Code != openapi.CodeInvalidBody {
			t.Errorf("%s: %d %s", bad, w.Code, w.Body.String())
		}
	}
}

// An agent proposes; only a person decides; "and from now on" drafts a
// rule from the example's sender, never its subject or body, and
// creates it with the decision, linked to the proposal; the next such
// delivery is handled by it.
func TestProposalBecomesRule(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id := h.find("This week at Example")
	agent := h.token(auth.MessageAnnotate, auth.MessageRead)
	person := h.tokenFor(h.human, auth.MessageAnnotate, auth.MessageRead, auth.RuleManage)

	p := post[ProposalCreated](t, h, "/api/v1/proposals", `{"kind":"annotate","message_id":"`+id+`",
		"actions":[{"kind":"label","label":"newsletter"},{"kind":"archive"}],
		"rationale":"a weekly digest","cites":[{"uri":"yuuka://message/`+id+`","quote":"This week"}]}`, agent, http.StatusCreated)
	if p.State != "pending" || p.Kind != "annotate" || p.Scope != "message" || p.Proposer.Name != "arisu" || len(p.Actions) != 2 || len(p.Cites) != 1 {
		t.Fatalf("proposal: %+v", p)
	}
	if w := h.do(http.MethodPost, "/api/v1/proposals/"+p.ID+"/accept", `{}`, h.token(auth.AllScopes...)); w.Code != http.StatusForbidden {
		t.Errorf("an agent accepting: %d %s", w.Code, w.Body.String())
	}
	if ps := decode[ProposalPage](t, h.get("/api/v1/proposals?state=pending", agent)); len(ps.Proposals) != 1 || ps.Proposals[0].ID != p.ID {
		t.Errorf("the agent's proposals: %+v", ps)
	}
	momoi := principalNamed(t, h, "agent", "momoi")
	if ps := decode[ProposalPage](t, h.get("/api/v1/proposals", h.tokenFor(momoi, auth.MessageRead))); len(ps.Proposals) != 0 {
		t.Errorf("another agent sees %+v", ps)
	}

	d := post[RuleDraft](t, h, "/api/v1/rules/draft", `{"message_id":"`+id+`"}`, person, http.StatusOK)
	var fields []string
	for _, c := range d.Candidates {
		fields = append(fields, c.Condition.Field)
		if c.Condition.Field == "subject" || c.Condition.Field == "body" {
			t.Errorf("drafted from what the sender wrote: %+v", c)
		}
		if c.Selected && c.Matches < 1 {
			t.Errorf("a ticked condition does not match its example: %+v", c)
		}
	}
	// The newsletter has a List-Id and passed DMARC.
	if !slices.Contains(fields, "list.id") || !d.Candidates[0].Selected || d.Candidates[0].Condition.Field != "list.id" ||
		d.Match == nil || d.Matches < 1 {
		t.Fatalf("draft: %+v", d)
	}
	match, _ := json.Marshal(d.Match)
	ruleBody := `{"rule":{"name":"weekly","match":` + string(match) + `,"actions":[{"kind":"label","label":"newsletter"},{"kind":"archive"}]}}`

	// Creating a rule needs rule:manage, even through a proposal.
	noRules := h.tokenFor(h.human, auth.MessageAnnotate, auth.MessageRead)
	if w := h.do(http.MethodPost, "/api/v1/proposals/"+p.ID+"/accept", ruleBody, noRules); w.Code != http.StatusForbidden || problemOf(t, w).Code != openapi.CodeScopeRequired {
		t.Errorf("accept and automate without rule:manage: %d %s", w.Code, w.Body.String())
	}
	acc := post[AcceptResult](t, h, "/api/v1/proposals/"+p.ID+"/accept", ruleBody, person, http.StatusOK)
	if acc.Proposal.State != "accepted" || acc.Rule == nil || acc.Rule.ProposalID != p.ID || acc.Proposal.RuleID != acc.Rule.ID ||
		len(acc.Messages) != 1 || !slices.Equal(kinds(acc.Messages[0].Outcomes), []agentview.AnnotateOutcome{"applied", "applied"}) {
		t.Fatalf("accept: %+v", acc)
	}
	if w := h.do(http.MethodPost, "/api/v1/proposals/"+p.ID+"/accept", `{}`, person); w.Code != http.StatusConflict {
		t.Errorf("accept twice: %d", w.Code)
	}

	next := h.deliver("html-only-newsletter.eml", strings.NewReplacer("Subject: This week at Example", "Subject: Next week at Example",
		"<weekly-44@", "<weekly-45@"), "news")
	page := decode[MessagePage](t, h.get("/api/v1/messages?downgraded=include&label=newsletter&archived=true", agent))
	if !slices.ContainsFunc(page.Messages, func(m MessageSummary) bool { return m.ID == next }) {
		t.Errorf("the next newsletter is not labelled and archived: %+v", page.Messages)
	}
	hist := decode[AnnotationHistory](t, h.get("/api/v1/messages/"+next+"/annotations", person))
	if len(hist.History) != 2 || hist.History[0].Rank != "rule" || hist.History[0].By.Kind != "rule" ||
		hist.History[0].By.Name != acc.Rule.ID || hist.History[0].Tool != "rule.run" {
		t.Errorf("provenance of the rule's label: %+v", hist)
	}
}

// A rule proposal carries its rule; invalid rules say where; nothing
// outbound can be proposed; people manage the rules.
func TestRuleProposalsAndRules(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id := h.find("This week at Example")
	agent := h.token(auth.MessageAnnotate, auth.MessageRead)
	person := h.tokenFor(h.human, auth.MessageAnnotate, auth.MessageRead, auth.RuleManage)

	p := post[ProposalCreated](t, h, "/api/v1/proposals", `{"kind":"rule","message_id":"`+id+`",
		"match":{"field":"from.addr","op":"eq","value":"Weekly@News.example.net"},"actions":[{"kind":"label","label":"weekly"}]}`,
		agent, http.StatusCreated)
	if len(p.Warnings) != 1 || p.Warnings[0].Code != "unverified_from" || p.Match["value"] != "weekly@news.example.net" {
		t.Errorf("rule proposal: %+v", p)
	}
	for body, path := range map[string]string{
		`{"kind":"rule","match":{"field":"subject","op":"eq","value":1},"actions":[{"kind":"archive"}]}`:                                               "match.value",
		`{"kind":"rule","match":{"all":[]},"actions":[{"kind":"archive"}]}`:                                                                            "match.all",
		`{"kind":"annotate","message_id":"` + id + `","actions":[{"kind":"notify","level":"normal"}]}`:                                                 "actions",
		`{"kind":"rule","match":{"field":"to.prefix","op":"eq","value":"news"},"actions":[{"kind":"label","label":"x"},{"kind":"label","label":"x"}]}`: "actions[1]",
	} {
		w := h.do(http.MethodPost, "/api/v1/proposals", body, agent)
		if pr := problemOf(t, w); w.Code != http.StatusBadRequest || pr.Code != openapi.CodeInvalidRule || pr.Path != path {
			t.Errorf("%s: %d %+v, want path %s", body, w.Code, pr, path)
		}
	}
	// An outbound action is not even a value of the type.
	if w := h.do(http.MethodPost, "/api/v1/proposals", `{"kind":"rule","match":{"field":"to.prefix","op":"eq","value":"news"},"actions":[{"kind":"forward"}]}`, agent); w.Code != http.StatusBadRequest {
		t.Errorf("an outbound proposal: %d", w.Code)
	}

	acc := post[AcceptResult](t, h, "/api/v1/proposals/"+p.ID+"/accept", `{}`, person, http.StatusOK)
	if acc.Rule == nil || acc.Rule.ProposalID != p.ID || len(acc.Rule.Warnings) != 1 || len(acc.Messages) != 0 {
		t.Fatalf("accept a rule proposal: %+v", acc)
	}
	rule := acc.Rule.ID

	rl := decode[RuleList](t, h.get("/api/v1/rules", person))
	if len(rl.Rules) != 1 || rl.Rules[0].ID != rule || !rl.Rules[0].Enabled {
		t.Errorf("rules: %+v", rl)
	}
	pv := post[RulePreview](t, h, "/api/v1/rules/preview", `{"match":{"field":"list.id","op":"eq","value":"weekly.news.example.net"},"actions":[{"kind":"archive"}],"samples":5}`, person, http.StatusOK)
	if pv.Matched < 1 || len(pv.Samples) < 1 || pv.Samples[0].ID == "" {
		t.Errorf("preview: %+v", pv)
	}
	if w := h.do(http.MethodPut, "/api/v1/rules/"+rule, `{"match":{"field":"subject","op":"regex","value":"("},"actions":[{"kind":"archive"}]}`, person); w.Code != http.StatusBadRequest || problemOf(t, w).Path != "match.value" {
		t.Errorf("an invalid regex: %d %s", w.Code, w.Body.String())
	}
	upd := decode[SavedRule](t, h.do(http.MethodPut, "/api/v1/rules/"+rule, `{"name":"weekly","disabled":true,"match":{"all":[{"field":"from.addr","op":"eq","value":"weekly@news.example.net"},{"field":"from.verified","op":"eq","value":true}]},"actions":[{"kind":"label","label":"weekly"}]}`, person))
	if upd.Enabled || upd.Revision != 2 || len(upd.Warnings) != 0 || upd.Name != "weekly" {
		t.Errorf("update: %+v", upd)
	}
	st := post[RetroStats](t, h, "/api/v1/rules/"+rule+"/apply", `{}`, person, http.StatusOK)
	if st.Matched < 1 || st.Changed != st.Matched {
		t.Errorf("apply: %+v", st)
	}
	if m := decode[Message](t, h.get("/api/v1/messages/"+id, agent)); !slices.Equal(m.Labels, []string{"weekly"}) {
		t.Errorf("after apply: %q", m.Labels)
	}
	if w := h.do(http.MethodDelete, "/api/v1/rules/"+rule, "", person); w.Code != http.StatusNoContent {
		t.Errorf("delete: %d", w.Code)
	}
	rv := post[RevertStats](t, h, "/api/v1/rules/"+rule+"/revert", `{}`, person, http.StatusOK)
	if rv.Reverted != st.Changed {
		t.Errorf("revert: %+v after %+v", rv, st)
	}
	if r := decode[Rule](t, h.get("/api/v1/rules/"+rule, person)); r.DeletedAt == nil || r.Enabled {
		t.Errorf("deleted rule: %+v", r)
	}
	if rl := decode[RuleList](t, h.get("/api/v1/rules", person)); len(rl.Rules) != 0 {
		t.Errorf("rules after delete: %+v", rl)
	}

	// Reject and withdraw.
	q := post[ProposalCreated](t, h, "/api/v1/proposals", `{"kind":"annotate","message_id":"`+id+`","scope":"thread","actions":[{"kind":"archive"}]}`, agent, http.StatusCreated)
	if w := h.do(http.MethodPost, "/api/v1/proposals/"+q.ID+"/withdraw", "", person); w.Code != http.StatusForbidden {
		t.Errorf("a person withdrawing an agent's proposal: %d", w.Code)
	}
	if r := post[Proposal](t, h, "/api/v1/proposals/"+q.ID+"/reject", "", person, http.StatusOK); r.State != "rejected" || r.DecidedBy != h.human.ID {
		t.Errorf("reject: %+v", r)
	}
	q = post[ProposalCreated](t, h, "/api/v1/proposals", `{"kind":"annotate","message_id":"`+id+`","actions":[{"kind":"label","label":"later"}]}`, agent, http.StatusCreated)
	if r := post[Proposal](t, h, "/api/v1/proposals/"+q.ID+"/withdraw", "", agent, http.StatusOK); r.State != "withdrawn" {
		t.Errorf("withdraw: %+v", r)
	}
	// unarchive takes back; a proposal only ever adds, like a rule.
	if w := h.do(http.MethodPost, "/api/v1/proposals", `{"kind":"annotate","message_id":"`+id+`","actions":[{"kind":"unarchive"}]}`, agent); w.Code != http.StatusBadRequest {
		t.Errorf("a proposal to unarchive: %d", w.Code)
	}
}
