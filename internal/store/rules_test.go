package store_test

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/classify"
	"github.com/yuuka-mail/yuuka/internal/contenthash"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/rules"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// ruleEnv is a database with one account and a clock for actions.
type ruleEnv struct {
	t    *testing.T
	db   *store.DB
	pool *pgxpool.Pool
	acct string
	now  time.Time
}

func newRuleEnv(t *testing.T) *ruleEnv {
	t.Helper()
	db, pool := storetest.New(t)
	return &ruleEnv{t: t, db: db, pool: pool, acct: storetest.Account(t, db), now: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
}

func (e *ruleEnv) tick() time.Time { e.now = e.now.Add(time.Minute); return e.now }

func (e *ruleEnv) action(tool string) store.Action {
	at := e.tick()
	return store.Action{ID: ulid.New(at), Actor: store.SystemPrincipalID, Tool: tool, At: at}
}

func (e *ruleEnv) principal(kind, name string) *auth.Viewer {
	e.t.Helper()
	at := e.tick()
	p := store.Principal{ID: ulid.New(at), Kind: kind, Name: name, DisplayName: name, CreatedAt: at}
	if err := e.db.CreatePrincipal(e.t.Context(), p, store.Action{ID: ulid.New(at), Tool: "principal.create", At: at}); err != nil {
		e.t.Fatal(err)
	}
	return &auth.Viewer{PrincipalID: p.ID, PrincipalKind: kind, PrincipalName: name}
}

// person is the host acting: rank 3.
var person = &auth.Viewer{PrincipalID: store.SystemPrincipalID, PrincipalKind: "system", PrincipalName: "system"}

func (e *ruleEnv) claim(prefix string) {
	e.t.Helper()
	if _, err := e.db.SetPrefixState(e.t.Context(), "example.com", prefix, store.PrefixClaimed, e.action("prefix.claim")); err != nil {
		e.t.Fatal(err)
	}
}

// deliver commits and folds raw to prefix@example.com.
func (e *ruleEnv) deliver(prefix string, raw []byte) store.Folded {
	e.t.Helper()
	at := e.tick()
	id := ulid.New(at)
	key := fmt.Sprintf("raw/2026/10/%s.eml", id)
	in := store.Ingest{
		RawID: id, R2Key: key, LocalPath: key, Size: int64(len(raw)), SHA256: sha256.Sum256(raw),
		ContentHash: contenthash.V1(raw), ReceivedAt: at, DeliveryID: id, AccountID: e.acct,
		Zone: "example.com", Prefix: prefix, EnvelopeTo: prefix + "@example.com",
	}
	if _, err := e.db.CommitIngest(e.t.Context(), in); err != nil {
		e.t.Fatal(err)
	}
	f, err := e.db.Fold(e.t.Context(), store.RawRef{
		DeliveryID: id, RawID: id, R2Key: key, LocalPath: key, ContentHash: in.ContentHash[:],
		Zone: "example.com", Prefix: prefix, ReceivedAt: at,
	}, mimeparse.Parse(raw), authres.Extract(raw, "mx.cloudflare.net"))
	if err != nil {
		e.t.Fatal(err)
	}
	return f
}

func (e *ruleEnv) rule(name string, priority int, spec string) store.Rule {
	e.t.Helper()
	r, _, err := e.db.CreateRule(e.t.Context(), store.NewRule{Name: name, Priority: priority, Spec: []byte(spec)}, e.action("rule.add"))
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// state is a message's labels and archived flag as the list shows them.
func (e *ruleEnv) state(id string) (labels []string, archived bool, class classify.Class) {
	e.t.Helper()
	d, err := e.db.MessageByID(e.t.Context(), person, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return d.Labels, d.Archived, d.Class
}

func (e *ruleEnv) annotate(v *auth.Viewer, id string, ins ...store.Annotation) []store.AnnotationResult {
	e.t.Helper()
	res, _, err := e.db.Annotate(e.t.Context(), v, id, ins, e.action("message.annotate"))
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

func outcomes(rs []store.AnnotationResult) []store.AnnotateOutcome {
	out := make([]store.AnnotateOutcome, len(rs))
	for i, r := range rs {
		out[i] = r.Outcome
	}
	return out
}

const shopRule = `{"v":1,"match":{"field":"to.prefix","op":"eq","value":"shop"},
	"actions":[{"kind":"label","label":"shop"},{"kind":"archive"},{"kind":"notify","level":"silent"}]}`

// Ingest runs the enabled rules on every new delivery, in the fold's
// transaction: labels, archive and the notify level, with one rule.run
// action per rule that did something. A rule can quiet or silence a
// notification, never cause one: downgraded mail stays unpushed.
// Rebuilding (reparse, backfills) never runs rules and never undoes
// their effects.
func TestFoldRunsRules(t *testing.T) {
	t.Parallel()
	e := newRuleEnv(t)
	ctx := t.Context()
	shop := e.rule("shop", 0, shopRule)
	lists := e.rule("lists", 1, `{"v":1,"match":{"field":"list.id","op":"eq","value":"dev.lists.example.org"},
		"actions":[{"kind":"label","label":"list"},{"kind":"classify","class":"human"},{"kind":"notify","level":"off"}]}`)
	// Matches everything, but its notify level loses to earlier rules.
	e.rule("loud", 9, `{"v":1,"match":{"field":"attachment","op":"eq","value":false},"actions":[{"kind":"notify","level":"normal"}]}`)
	e.claim("shop")
	e.claim("dev")

	f := e.deliver("shop", letter("orders@example.net", "shop@example.com", "Your order", "", "shipped"))
	if !f.Notify || !f.Silent || len(f.RuleFailures) != 0 {
		t.Errorf("claimed shop mail: %+v, want a silent notification", f)
	}
	labels, archived, _ := e.state(f.MessageID)
	if !slices.Equal(labels, []string{"shop"}) || !archived {
		t.Errorf("shop mail: labels %q archived %v", labels, archived)
	}
	var actor, tool string
	if err := e.pool.QueryRow(ctx, `SELECT a.actor, a.tool FROM annotation_log l JOIN actions a ON a.id = l.action_id
		WHERE l.message_id = $1 LIMIT 1`, f.MessageID).Scan(&actor, &tool); err != nil || actor != shop.PrincipalID || tool != "rule.run" {
		t.Errorf("shop mail annotated by %s %s, %v", actor, tool, err)
	}

	// A list message: label, class, and notification off.
	f = e.deliver("dev", letter("someone@example.net", "dev@example.com", "Re: design", "List-Id: Dev list <dev.lists.example.org>\r\n", "hi"))
	labels, _, class := e.state(f.MessageID)
	if f.Notify || !slices.Equal(labels, []string{"list"}) || class != classify.Human {
		t.Errorf("list mail: notify %v labels %q class %v", f.Notify, labels, class)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM actions WHERE actor = $1 AND tool = 'rule.run'`, lists.PrincipalID); n != 1 {
		t.Errorf("list rule has %d actions, want 1", n)
	}

	// Downgraded: the prefix is not claimed. The shop rule labels it,
	// but no rule makes it notify.
	e.rule("everything loud", -1, `{"v":1,"match":{"field":"attachment","op":"eq","value":false},"actions":[{"kind":"notify","level":"normal"}]}`)
	f = e.deliver("random", letter("x@example.net", "random@example.com", "hello", "", "spam"))
	if f.Notify || f.Silent {
		t.Errorf("downgraded mail: %+v, want no notification", f)
	}

	// Reparse rewrites the content and leaves the rule's decisions,
	// including a person's later change, alone.
	shopMsg := e.deliver("shop", letter("orders@example.net", "shop@example.com", "Second order", "", "shipped")).MessageID
	e.annotate(person, shopMsg, store.SetLabel("shop", false))
	runs := count(t, e.pool, `SELECT count(*) FROM actions WHERE tool = 'rule.run'`)
	targets, err := e.db.ReparseTargets(ctx, time.Time{}, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range targets {
		raw := letter("orders@example.net", "shop@example.com", "Second order", "", "shipped")
		if tg.MessageID != shopMsg {
			continue
		}
		if _, err := e.db.Rebuild(ctx, tg, mimeparse.Parse(raw), map[string]authres.Auth{tg.Deliveries[0].DeliveryID: {}}); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, e.pool, `SELECT count(*) FROM actions WHERE tool = 'rule.run'`); n != runs {
		t.Errorf("rebuild ran rules: %d rule.run actions, had %d", n, runs)
	}
	if labels, archived, _ := e.state(shopMsg); len(labels) != 0 || !archived {
		t.Errorf("after rebuild: labels %q archived %v, want the person's removal and the rule's archive", labels, archived)
	}
}

// Rules evaluating on a message see it as it was before the round: the
// class a person set, and one rule's effects are not another's facts.
func TestRulesSeeTheEffectiveClass(t *testing.T) {
	t.Parallel()
	e := newRuleEnv(t)
	e.rule("humans", 0, `{"v":1,"match":{"field":"class","op":"eq","value":"human"},"actions":[{"kind":"label","label":"people"}]}`)
	e.rule("classifier", -1, `{"v":1,"match":{"field":"to.prefix","op":"eq","value":"a"},"actions":[{"kind":"classify","class":"human"}]}`)
	f := e.deliver("a", letter("noreply@example.net", "a@example.com", "Your receipt", "", "thanks"))
	labels, _, class := e.state(f.MessageID)
	if class != classify.Human || len(labels) != 0 {
		t.Errorf("class %v labels %q: the second rule must not see the first one's class", class, labels)
	}
}

// The precedence ladder: person/system (3) > agent/service (2) > rule
// (1), later writers winning among equals; a person's removal is a
// tombstone that a rule's later run respects; asking for what already
// is writes nothing.
func TestAnnotationPrecedence(t *testing.T) {
	t.Parallel()
	e := newRuleEnv(t)
	ctx := t.Context()
	shop := e.rule("shop", 0, shopRule)
	arisu := e.principal("agent", "arisu")
	monitoring := e.principal("service", "monitoring")
	msg := e.deliver("shop", letter("orders@example.net", "shop@example.com", "Order", "", "x")).MessageID

	// An agent outranks the rule; a service is its equal.
	if got := outcomes(e.annotate(arisu, msg, store.SetLabel("shop", false), store.SetLabel("orders", true))); !slices.Equal(got, []store.AnnotateOutcome{store.Applied, store.Applied}) {
		t.Errorf("agent over rule: %v", got)
	}
	if got := outcomes(e.annotate(monitoring, msg, store.SetLabel("orders", false))); !slices.Equal(got, []store.AnnotateOutcome{store.Applied}) {
		t.Errorf("service after agent: %v", got)
	}
	// A person's decision stands against agents.
	e.annotate(person, msg, store.SetLabel("orders", true), store.SetClass(classify.Machine))
	if got := outcomes(e.annotate(arisu, msg, store.SetLabel("orders", false), store.ClearClass())); !slices.Equal(got, []store.AnnotateOutcome{store.Overruled, store.Overruled}) {
		t.Errorf("agent against person: %v", got)
	}
	// Idempotent: nothing new is written.
	logRows := count(t, e.pool, `SELECT count(*) FROM annotation_log`)
	actions := count(t, e.pool, `SELECT count(*) FROM actions`)
	res, recorded, err := e.db.Annotate(ctx, person, msg, []store.Annotation{store.SetLabel("orders", true)}, e.action("message.annotate"))
	if err != nil || recorded || res[0].Outcome != store.Unchanged {
		t.Errorf("same again: %v recorded %v, %v", outcomes(res), recorded, err)
	}
	if count(t, e.pool, `SELECT count(*) FROM annotation_log`) != logRows || count(t, e.pool, `SELECT count(*) FROM actions`) != actions {
		t.Error("an unchanged annotation was logged")
	}

	// A person removing the rule's label leaves a tombstone: applying
	// the rule again does not bring it back, and is idempotent.
	other := e.deliver("shop", letter("orders@example.net", "shop@example.com", "Order 2", "", "y")).MessageID
	e.annotate(person, other, store.SetLabel("shop", false))
	st, err := e.db.ApplyRule(ctx, shop.ID, time.Time{}, false, e.action("rule.apply"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Matched != 2 || st.Changed != 0 || st.Overruled != 2 {
		t.Errorf("apply over decided mail: %+v", st)
	}
	if labels, _, _ := e.state(other); slices.Contains(labels, "shop") {
		t.Errorf("tombstone ignored: %q", labels)
	}
	if _, _, err := e.db.Annotate(ctx, &auth.Viewer{PrincipalKind: "rule", PrincipalName: "x"}, msg,
		[]store.Annotation{store.SetArchived(false)}, e.action("x")); !errors.Is(err, store.ErrNotPermitted) {
		t.Errorf("annotate as a rule: %v", err)
	}
	if _, _, err := e.db.Annotate(ctx, person, msg, []store.Annotation{store.SetLabel("$Seen", true)}, e.action("x")); err == nil {
		t.Error("reserved label accepted")
	}
}

// Undo reverts an action's rows that are still the latest for their
// attribute; agents undo only their own actions; undoing twice changes
// nothing; a rule's revert takes back its changes at rule rank and
// leaves what others changed since.
func TestUndoAndRevert(t *testing.T) {
	t.Parallel()
	e := newRuleEnv(t)
	ctx := t.Context()
	arisu, other := e.principal("agent", "arisu"), e.principal("agent", "other")
	msg := e.deliver("a", letter("x@example.net", "a@example.com", "Hi", "", "x")).MessageID

	a := e.action("message.annotate")
	a.Actor = arisu.PrincipalID
	if _, recorded, err := e.db.Annotate(ctx, arisu, msg, []store.Annotation{store.SetLabel("todo", true), store.SetArchived(true)}, a); err != nil || !recorded {
		t.Fatal(err)
	}
	if _, err := e.db.Undo(ctx, other, a.ID, e.action("annotation.undo")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("undo of someone else's action: %v", err)
	}
	// The person changed archived since; only the label goes back.
	e.annotate(person, msg, store.SetArchived(false))
	res, err := e.db.Undo(ctx, arisu, a.ID, e.action("annotation.undo"))
	if err != nil || res.Reverted != 1 || res.Superseded != 1 {
		t.Errorf("undo: %+v %v", res, err)
	}
	if labels, archived, _ := e.state(msg); len(labels) != 0 || archived {
		t.Errorf("after undo: %q %v", labels, archived)
	}
	if res, err := e.db.Undo(ctx, arisu, a.ID, e.action("annotation.undo")); err != nil || res.Reverted != 0 {
		t.Errorf("undo twice: %+v %v", res, err)
	}

	// Revert of a rule: its changes go, except the one a person
	// changed again; deleted rules can be reverted; running it again
	// does nothing.
	r := e.rule("shop", 0, shopRule)
	m1 := e.deliver("shop", letter("o@example.net", "shop@example.com", "1", "", "1")).MessageID
	m2 := e.deliver("shop", letter("o@example.net", "shop@example.com", "2", "", "2")).MessageID
	e.annotate(person, m2, store.SetArchived(false))
	if err := e.db.DeleteRule(ctx, r.ID, e.action("rule.rm")); err != nil {
		t.Fatal(err)
	}
	st, err := e.db.RevertRule(ctx, r.ID, time.Time{}, e.action("rule.revert"))
	if err != nil || st.Messages != 2 || st.Reverted != 3 || st.Superseded != 1 {
		t.Errorf("revert: %+v %v", st, err)
	}
	for _, m := range []string{m1, m2} {
		if labels, archived, _ := e.state(m); len(labels) != 0 || archived {
			t.Errorf("after revert %s: %q %v", m, labels, archived)
		}
	}
	if st, err := e.db.RevertRule(ctx, r.ID, time.Time{}, e.action("rule.revert")); err != nil || st.Reverted != 0 {
		t.Errorf("revert again: %+v %v", st, err)
	}
	// The reverts are at rule rank: an agent may change them.
	if got := outcomes(e.annotate(arisu, m1, store.SetLabel("shop", true))); got[0] != store.Applied {
		t.Errorf("agent after a rule revert: %v", got)
	}
}

// A retroactive run goes over past mail in pages, each its own
// transaction; it is idempotent, so a run that stopped is finished by
// running it again, and a dry run writes nothing.
func TestApplyRuleInPages(t *testing.T) {
	t.Parallel()
	e := newRuleEnv(t)
	ctx := t.Context()
	const n = 230 // more than one page
	var half time.Time
	for i := range n {
		e.deliver("news", letter("news@example.net", "news@example.com", fmt.Sprintf("Issue %d", i), "", "read me"))
		if i == n/2 {
			half = e.now
		}
	}
	e.deliver("other", letter("x@example.net", "other@example.com", "Unrelated", "", "x"))
	r, _, err := e.db.CreateRule(ctx, store.NewRule{
		Name: "news", Disabled: true,
		Spec: []byte(`{"v":1,"match":{"field":"to.prefix","op":"eq","value":"news"},"actions":[{"kind":"label","label":"news"},{"kind":"notify","level":"normal"}]}`),
	},
		e.action("rule.add"))
	if err != nil {
		t.Fatal(err)
	}
	dry, err := e.db.ApplyRule(ctx, r.ID, time.Time{}, true, e.action("rule.apply"))
	if err != nil || dry.Messages != n+1 || dry.Matched != n || dry.Changed != n {
		t.Errorf("dry run: %+v %v", dry, err)
	}
	if c := count(t, e.pool, `SELECT count(*) FROM annotation_log`); c != 0 {
		t.Errorf("dry run wrote %d rows", c)
	}
	// The run that "stopped" halfway: the newer half.
	st, err := e.db.ApplyRule(ctx, r.ID, half, false, e.action("rule.apply"))
	if err != nil || st.Changed != n-n/2 {
		t.Errorf("first half: %+v %v", st, err)
	}
	st, err = e.db.ApplyRule(ctx, r.ID, time.Time{}, false, e.action("rule.apply"))
	if err != nil || st.Matched != n || st.Changed != n/2 {
		t.Errorf("finishing run: %+v %v", st, err)
	}
	if c := count(t, e.pool, `SELECT count(*) FROM message_state WHERE labels = '{news}'`); c != n {
		t.Errorf("%d messages labeled, want %d", c, n)
	}
	// One rule.apply action of the rule per run that changed something,
	// each hanging off the CLI's request.
	if c := count(t, e.pool, `SELECT count(*) FROM actions WHERE actor = $1 AND tool = 'rule.apply'`, r.PrincipalID); c != 2 {
		t.Errorf("%d rule.apply actions of the rule, want 2", c)
	}
	if c := count(t, e.pool, `SELECT count(*) FROM actions WHERE actor = $1 AND tool = 'rule.apply' AND result->>'complete' = 'true'`, store.SystemPrincipalID); c != 2 {
		t.Errorf("%d completed requests, want 2", c)
	}
	st, err = e.db.ApplyRule(ctx, r.ID, time.Time{}, false, e.action("rule.apply"))
	if err != nil || st.Changed != 0 {
		t.Errorf("third run: %+v %v", st, err)
	}
	// Editing the rule bumps its revision.
	upd, _, err := e.db.UpdateRule(ctx, r.ID, store.RuleUpdate{Priority: new(5)}, e.action("rule.set"))
	if err != nil || upd.Revision != r.Revision+1 || upd.Priority != 5 {
		t.Errorf("update: %+v %v", upd, err)
	}
}

// Save-time validation: the format, warnings for an unverified From,
// and the regex budget across enabled rules.
func TestRuleValidation(t *testing.T) {
	t.Parallel()
	e := newRuleEnv(t)
	ctx := t.Context()
	for _, bad := range []string{
		`{"v":1,"match":{"field":"from.addr","op":"eq","value":"a@example.net"},"actions":[{"kind":"forward","to":"x@example.org"}]}`,
		`{"v":2,"match":{"field":"from.addr","op":"eq","value":"a@example.net"},"actions":[{"kind":"archive"}]}`,
		`{"v":1,"match":{"field":"to.header","op":"eq","value":"a@example.net"},"actions":[{"kind":"archive"}]}`,
		`{"v":1,"match":{"all":[]},"actions":[{"kind":"archive"}]}`,
	} {
		if _, _, err := e.db.CreateRule(ctx, store.NewRule{Spec: []byte(bad)}, e.action("rule.add")); !errors.Is(err, rules.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", bad, err)
		}
	}
	_, warns, err := e.db.CreateRule(ctx, store.NewRule{Spec: []byte(`{"v":1,"match":{"field":"from.domain","op":"eq","value":"example.net"},"actions":[{"kind":"label","label":"bank"}]}`)}, e.action("rule.add"))
	if err != nil || len(warns) != 1 || warns[0].Code != rules.WarnUnverifiedFrom {
		t.Errorf("unverified from: %v %v", warns, err)
	}
	var leaves []string
	for i := range rules.MaxRegexPerRule {
		leaves = append(leaves, fmt.Sprintf(`{"field":"subject","op":"regex","value":"x%d"}`, i))
	}
	spec := []byte(`{"v":1,"match":{"any":[` + strings.Join(leaves, ",") + `]},"actions":[{"kind":"archive"}]}`)
	for i := range rules.MaxRegexTotal / rules.MaxRegexPerRule {
		if _, _, err := e.db.CreateRule(ctx, store.NewRule{Spec: spec}, e.action("rule.add")); err != nil {
			t.Fatalf("rule %d: %v", i, err)
		}
	}
	if _, _, err := e.db.CreateRule(ctx, store.NewRule{Spec: spec}, e.action("rule.add")); !errors.Is(err, rules.ErrInvalid) {
		t.Errorf("over the regex budget: %v", err)
	}
	r, _, err := e.db.CreateRule(ctx, store.NewRule{Spec: spec, Disabled: true}, e.action("rule.add"))
	if err != nil {
		t.Fatalf("a disabled rule is outside the budget: %v", err)
	}
	if _, _, err := e.db.UpdateRule(ctx, r.ID, store.RuleUpdate{Enabled: new(true)}, e.action("rule.set")); !errors.Is(err, rules.ErrInvalid) {
		t.Errorf("enabling over the budget: %v", err)
	}
}

// The schema refuses outbound actions on its own, whatever writes the
// row (§8.5): a hand-written rule or proposal that would send fails its
// CHECK.
func TestOutboundActionsRefusedBySchema(t *testing.T) {
	t.Parallel()
	e := newRuleEnv(t)
	ctx := t.Context()
	for _, actions := range []string{
		`[{"kind":"send","to":"x@example.org"}]`,
		`[{"kind":"label","label":"a"},{"kind":"forward","to":"x@example.org"}]`,
		`[{"label":"no kind"}]`,
		`[]`,
	} {
		_, err := e.pool.Exec(ctx, `
			INSERT INTO rules (id, principal_id, version, match, actions, created_by)
			VALUES ($1, $2, 1, '{"field":"subject","op":"eq","value":"x"}', $3::jsonb, $2)`,
			ulid.New(time.Now()), store.SystemPrincipalID, actions)
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != "23514" {
			t.Errorf("rule with %s: %v, want a check violation", actions, err)
		}
		_, err = e.pool.Exec(ctx, `
			INSERT INTO proposals (id, proposed_by, kind, payload, expires_at)
			VALUES ($1, $2, 'rule', jsonb_build_object('v', 1, 'match', '{}'::jsonb, 'actions', $3::jsonb), now())`,
			ulid.New(time.Now()), store.SystemPrincipalID, actions)
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); actions != `[]` && (!ok || pgErr.Code != "23514") {
			t.Errorf("proposal with %s: %v, want a check violation", actions, err)
		}
	}
}

// A rule principal acts only through its rule: the store refuses it
// credentials, identities and grants.
func TestRulePrincipalHoldsNothing(t *testing.T) {
	t.Parallel()
	e := newRuleEnv(t)
	ctx := t.Context()
	r := e.rule("x", 0, shopRule)
	at := e.tick()
	err := e.db.CreateCredential(ctx, store.Credential{
		ID: ulid.New(at), PrincipalID: r.PrincipalID, Kind: "token",
		SecretHash: make([]byte, 32), Scopes: []string{"message:read"}, CreatedAt: at,
	}, e.action("token.issue"))
	if !errors.Is(err, store.ErrRulePrincipal) {
		t.Errorf("token for a rule: %v", err)
	}
	storetest.Transports(t, e.db, "cf")
	ident := store.Identity{ID: ulid.New(at), AccountID: e.acct, Zone: "example.com", Prefix: "rule", Transport: "cf", OwnerID: r.PrincipalID, CreatedAt: at}
	if err := e.db.AddIdentity(ctx, ident, e.action("identity.add")); !errors.Is(err, store.ErrRulePrincipal) {
		t.Errorf("identity owned by a rule: %v", err)
	}
	ident.OwnerID, ident.Prefix = store.SystemPrincipalID, "me"
	if err := e.db.AddIdentity(ctx, ident, e.action("identity.add")); err != nil {
		t.Fatal(err)
	}
	err = e.db.SetGrant(ctx, store.Grant{IdentityID: ident.ID, PrincipalID: r.PrincipalID, Policy: "draft", RecipientScope: "thread"}, e.action("grant.set"))
	if !errors.Is(err, store.ErrRulePrincipal) {
		t.Errorf("grant to a rule: %v", err)
	}
}

// Proposals: an agent proposes, only a person decides; the same pending
// proposal is one; each proposer has a cap; accepting a rule proposal
// creates the rule and its principal in one go, linked to the proposal.
func TestProposals(t *testing.T) {
	t.Parallel()
	e := newRuleEnv(t)
	ctx := t.Context()
	arisu := e.principal("agent", "arisu")
	e.claim("bank")
	first := e.deliver("bank", letter("alerts@example.net", "bank@example.com", "Statement", "In-Reply-To: <none@example.net>\r\nMessage-ID: <s1@example.net>\r\n", "x")).MessageID
	second := e.deliver("bank", letter("alerts@example.net", "bank@example.com", "Re: Statement", "In-Reply-To: <s1@example.net>\r\n", "y")).MessageID

	spec := []byte(`{"v":1,"match":{"field":"to.prefix","op":"eq","value":"bank"},"actions":[{"kind":"label","label":"bank"}]}`)
	p, _, err := e.db.CreateProposal(ctx, arisu, store.NewProposal{
		Kind: store.ProposalRule, MessageID: first, Payload: spec,
		Rationale: "statements", Cites: []store.Cite{{URI: "yuuka://message/" + first, Quote: "Statement"}},
	}, e.action("proposal.create"))
	if err != nil || p.State != store.ProposalPending || p.Proposer != "agent:arisu" || !p.ExpiresAt.Equal(p.CreatedAt.Add(store.ProposalTTL)) {
		t.Fatalf("propose: %+v %v", p, err)
	}
	if _, _, err := e.db.CreateProposal(ctx, arisu, store.NewProposal{Kind: store.ProposalRule, MessageID: first, Payload: spec}, e.action("proposal.create")); !errors.Is(err, store.ErrDuplicate) {
		t.Errorf("same proposal again: %v", err)
	}
	if _, err := e.db.AcceptProposal(ctx, arisu, p.ID, nil, e.action("proposal.accept")); !errors.Is(err, store.ErrNotPermitted) {
		t.Errorf("agent accepting: %v", err)
	}
	acc, err := e.db.AcceptProposal(ctx, person, p.ID, nil, e.action("proposal.accept"))
	if err != nil || acc.Rule == nil || acc.Rule.ProposalID != p.ID || !acc.Rule.Enabled {
		t.Fatalf("accept: %+v %v", acc, err)
	}
	pr, err := e.db.Principal(ctx, acc.Rule.PrincipalID)
	if err != nil || pr.Kind != "rule" || pr.Name != acc.Rule.ID {
		t.Errorf("rule principal: %+v %v", pr, err)
	}
	if _, err := e.db.AcceptProposal(ctx, person, p.ID, nil, e.action("proposal.accept")); !errors.Is(err, store.ErrNotPending) {
		t.Errorf("accept twice: %v", err)
	}
	// The new rule runs on the next delivery; no retroactive run.
	if labels, _, _ := e.state(first); len(labels) != 0 {
		t.Errorf("accepting ran the rule on old mail: %q", labels)
	}
	if labels, _, _ := e.state(e.deliver("bank", letter("alerts@example.net", "bank@example.com", "Next", "", "z")).MessageID); !slices.Equal(labels, []string{"bank"}) {
		t.Errorf("new mail: %q", labels)
	}

	// An annotate proposal for the thread, accepted at the person's rank.
	ann := []byte(`{"v":1,"scope":"thread","actions":[{"kind":"label","label":"statements"},{"kind":"archive"}]}`)
	p, _, err = e.db.CreateProposal(ctx, arisu, store.NewProposal{Kind: store.ProposalAnnotate, MessageID: second, Payload: ann}, e.action("proposal.create"))
	if err != nil {
		t.Fatal(err)
	}
	other := e.principal("agent", "other")
	// Another agent cannot see it (§13.8); a person sees it but only
	// its proposer withdraws.
	if err := e.db.WithdrawProposal(ctx, other, p.ID, e.action("proposal.withdraw")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("withdraw by another agent: %v", err)
	}
	if err := e.db.WithdrawProposal(ctx, person, p.ID, e.action("proposal.withdraw")); !errors.Is(err, store.ErrNotPermitted) {
		t.Errorf("withdraw by a person: %v", err)
	}
	acc, err = e.db.AcceptProposal(ctx, person, p.ID, nil, e.action("proposal.accept"))
	if err != nil || len(acc.Annotations) != 2 || acc.Rule != nil {
		t.Fatalf("accept annotate: %+v %v", acc, err)
	}
	if got := outcomes(e.annotate(arisu, first, store.SetArchived(false))); got[0] != store.Overruled {
		t.Errorf("agent against an accepted proposal: %v", got)
	}
	for _, bad := range []string{
		`{"v":1,"scope":"message","actions":[{"kind":"notify","level":"normal"}]}`,
		`{"v":1,"scope":"message","actions":[{"kind":"send"}]}`,
		`{"v":1,"scope":"folder","actions":[{"kind":"archive"}]}`,
	} {
		if _, _, err := e.db.CreateProposal(ctx, arisu, store.NewProposal{Kind: store.ProposalAnnotate, MessageID: first, Payload: []byte(bad)}, e.action("proposal.create")); !errors.Is(err, rules.ErrInvalid) {
			t.Errorf("%s: %v", bad, err)
		}
	}

	// The cap.
	for i := range store.MaxPendingProposals {
		pl := fmt.Appendf(nil, `{"v":1,"scope":"message","actions":[{"kind":"label","label":"l%d"}]}`, i)
		if _, _, err := e.db.CreateProposal(ctx, other, store.NewProposal{Kind: store.ProposalAnnotate, MessageID: first, Payload: pl}, e.action("proposal.create")); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := e.db.CreateProposal(ctx, other, store.NewProposal{Kind: store.ProposalAnnotate, MessageID: first, Payload: ann}, e.action("proposal.create")); !errors.Is(err, store.ErrTooManyProposals) {
		t.Errorf("over the cap: %v", err)
	}
	if ps, err := e.db.Proposals(ctx, person, store.ProposalQuery{State: store.ProposalPending, ProposedBy: other.PrincipalID, Limit: 500}); err != nil || len(ps) != store.MaxPendingProposals {
		t.Errorf("pending list: %d %v", len(ps), err)
	}
	// Expired ones read as expired and free the slot.
	if _, err := e.pool.Exec(ctx, `UPDATE proposals SET expires_at = created_at WHERE proposed_by = $1`, other.PrincipalID); err != nil {
		t.Fatal(err)
	}
	if ps, err := e.db.Proposals(ctx, person, store.ProposalQuery{State: store.ProposalExpired}); err != nil || len(ps) != store.MaxPendingProposals {
		t.Errorf("expired list: %d %v", len(ps), err)
	}
	if _, _, err := e.db.CreateProposal(ctx, other, store.NewProposal{
		Kind: store.ProposalAnnotate, MessageID: first,
		Payload: []byte(`{"v":1,"scope":"message","actions":[{"kind":"label","label":"l0"}]}`),
	}, e.action("proposal.create")); err != nil {
		t.Errorf("re-proposing an expired one: %v", err)
	}
}
