package store_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/leak"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

func testAction() store.Action {
	now := time.Now()
	return store.Action{ID: ulid.New(now), Actor: store.SystemPrincipalID, Tool: "prefix.test", At: now}
}

// Leak detection (§3 principle 3) over one claimed prefix given to
// example.net: each kind of sender, judged on read.
func TestLeakCheck(t *testing.T) {
	t.Parallel()
	db, pool := storetest.New(t)
	ctx := t.Context()
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	tick := func() time.Time { at = at.Add(time.Minute); return at }
	mail := map[string]string{
		// DMARC passed for example.net.
		"ok": deliver(t, db, "plain-otp.eml", nil, "github", tick()),
		// DMARC passed for example.org: a leak to a known party.
		"leak": deliver(t, db, "alternative-reply.eml", nil, "github", tick()),
		// Claims example.net with no trusted result at all.
		"spoof": deliver(t, db, "untrusted-receiver.eml", nil, "github", tick()),
		// dmarc=none from example.org: a leak, to nobody in particular.
		"unverified": deliver(t, db, "lf-no-mime.eml", strings.NewReplacer("From: cron@example.net", "From: cron@example.org"), "github", tick()),
		// dmarc=fail is downgraded; from a stranger the downgrade is all
		// there is to say.
		"downgraded": deliver(t, db, "phish-dmarc-fail.eml", nil, "github", tick()),
		// dmarc=fail forging the expected sender is the typical spoof,
		// and judged as one although downgraded.
		"forged": deliver(t, db, "phish-dmarc-fail.eml", strings.NewReplacer(
			"security@bank.example.com", "security@example.net", "header.from=bank.example.com", "header.from=example.net"), "github", tick()),
	}
	lastOffense := at
	// The same OTP to an unclaimed prefix is not judged there.
	deliver(t, db, "plain-otp.eml", nil, "random", tick())

	if _, err := db.SetPrefixState(ctx, "example.com", "github", store.PrefixClaimed, testAction()); err != nil {
		t.Fatal(err)
	}
	github := func() store.PrefixStat {
		t.Helper()
		p, ok := prefixStat(t, db, "github")
		if !ok {
			t.Fatal("github is not registered")
		}
		return p
	}
	verdicts := func() map[string]leak.Judgement {
		t.Helper()
		out := map[string]leak.Judgement{}
		for name, id := range mail {
			m, err := db.MessageByID(ctx, operator, id)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range m.Deliveries {
				if d.Prefix == "github" {
					out[name] = d.Leak
				}
			}
		}
		return out
	}

	// Claimed but expecting nothing: what DMARC proved is observed, and
	// nothing is judged.
	p := github()
	observed := []store.ObservedDomain{
		{Domain: "example.net", Messages: 1, LastSeen: at.Add(-6 * time.Minute)},
		{Domain: "example.org", Messages: 1, LastSeen: at.Add(-5 * time.Minute)},
	}
	if !equalObserved(p.Observed, observed) || p.Suspect() || p.Unverified != 0 || p.LastOffense != nil {
		t.Errorf("expecting nothing: %+v", p.LeakStats)
	}
	for name, j := range verdicts() {
		if j != (leak.Judgement{}) {
			t.Errorf("expecting nothing, %s: %+v", name, j)
		}
	}

	stored, changed, err := db.SetPrefixExpected(ctx, "example.com", "github", []string{"mail.example.net", "Example.NET"}, testAction())
	if err != nil || !changed || !slices.Equal(stored, []string{"example.net"}) {
		t.Fatalf("expect: %q %v %v", stored, changed, err)
	}
	var changes int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM changes WHERE kind = 'prefix' AND target_id = 'github@example.com'`).Scan(&changes); err != nil {
		t.Fatal(err)
	}
	p = github()
	observed[0].Expected = true
	if !equalObserved(p.Observed, observed) || p.Leaks != 2 || p.Unverified != 1 || p.Spoofs != 2 ||
		p.LastOffense == nil || !p.LastOffense.Equal(lastOffense) || !p.Suspect() {
		t.Errorf("expecting example.net: %+v", p.LeakStats)
	}
	want := map[string]leak.Judgement{
		"ok":         {Verdict: leak.OK, Domain: "example.net"},
		"leak":       {Verdict: leak.Leak, Domain: "example.org"},
		"spoof":      {Verdict: leak.Spoof, Domain: "example.net"},
		"unverified": {Verdict: leak.Leak},
		"downgraded": {},
		"forged":     {Verdict: leak.Spoof, Domain: "example.net"},
	}
	for name, j := range verdicts() {
		if j != want[name] {
			t.Errorf("%s: %+v, want %+v", name, j, want[name])
		}
	}
	if r, ok := prefixStat(t, db, "random"); !ok || r.Suspect() || len(r.Observed) != 0 {
		t.Errorf("unclaimed prefix: %+v", r.LeakStats)
	}

	// The detail lists the offending mail, newest first.
	d, err := db.PrefixByAddress(ctx, operator, "example.com", "github", 3)
	if err != nil {
		t.Fatal(err)
	}
	// The forgery is listed, and stays downgraded.
	if len(d.Offenses) != 3 || d.Offenses[0].Message.ID != mail["forged"] || d.Offenses[0].Verdict != leak.Spoof ||
		!d.Offenses[0].Message.Downgraded ||
		d.Offenses[1].Message.ID != mail["unverified"] || d.Offenses[1].Verdict != leak.Leak ||
		d.Offenses[2].Message.ID != mail["spoof"] || d.Offenses[2].Verdict != leak.Spoof || d.Offenses[2].Message.Subject == "" ||
		d.Leaks != 2 {
		t.Errorf("detail: %+v", d)
	}
	if _, err := db.PrefixByAddress(ctx, operator, "example.com", "nobody", 2); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown prefix: %v", err)
	}

	yes := true
	sus, err := db.PrefixStats(ctx, operator, store.PrefixQuery{LeakSuspect: &yes})
	if err != nil || sus.Total != 1 || len(sus.Prefixes) != 1 || sus.Prefixes[0].Prefix != "github" {
		t.Errorf("suspects: %+v %v", sus, err)
	}

	// The decision is checked and recorded once.
	if _, changed, err := db.SetPrefixExpected(ctx, "example.com", "github", []string{"example.net"}, testAction()); err != nil || changed {
		t.Errorf("same again: %v %v", changed, err)
	}
	for prefix, want := range map[string]error{"random": store.ErrNotClaimed, "nobody": store.ErrNotFound} {
		if _, _, err := db.SetPrefixExpected(ctx, "example.com", prefix, []string{"example.net"}, testAction()); !errors.Is(err, want) {
			t.Errorf("expect on %s: %v, want %v", prefix, err, want)
		}
	}
	if _, _, err := db.SetPrefixExpected(ctx, "example.com", "github", []string{"com"}, testAction()); err == nil {
		t.Error("a public suffix was accepted")
	}
	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM changes WHERE kind = 'prefix' AND target_id = 'github@example.com'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	var actions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM actions WHERE tool = 'prefix.test' AND args->'expected_domains' IS NOT NULL`).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if after != changes || actions != 1 {
		t.Errorf("recorded %d more changes and %d actions, want 0 and 1", after-changes, actions)
	}

	// Unclaiming takes the prefix out of the check; its expectation stays.
	if _, err := db.SetPrefixState(ctx, "example.com", "github", store.PrefixDiscovered, testAction()); err != nil {
		t.Fatal(err)
	}
	if p := github(); p.Suspect() || len(p.Observed) != 0 || !slices.Equal(p.ExpectedDomains, []string{"example.net"}) {
		t.Errorf("unclaimed: %+v", p)
	}
	for name, j := range verdicts() {
		if j != (leak.Judgement{}) {
			t.Errorf("unclaimed, %s: %+v", name, j)
		}
	}
}

func equalObserved(a, b []store.ObservedDomain) bool {
	return slices.EqualFunc(a, b, func(x, y store.ObservedDomain) bool {
		return x.Domain == y.Domain && x.Messages == y.Messages && x.LastSeen.Equal(y.LastSeen) && x.Expected == y.Expected
	})
}

// "Verified sender" has one definition: the summary's SenderVerified and
// the rules' from.verified are both leak.SenderOf, a DMARC pass about
// the From the reader sees. A pass the receiver reports for another
// organization's header.from proves nothing about this From, so the
// list must not call it verified when a from.verified rule would not
// match it.
func TestSenderVerifiedIsFromVerified(t *testing.T) {
	t.Parallel()
	db, _ := storetest.New(t)
	ctx := t.Context()
	v := &auth.Viewer{PrincipalID: store.SystemPrincipalID, PrincipalKind: "human", PrincipalName: "operator"}
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		edit []string
		want bool
	}{
		{"pass for the From", nil, true},
		{"pass for another organization", []string{"header.from=example.net", "header.from=example.org"}, false},
		{"no trusted result", []string{"mx.cloudflare.net;", "mx.example.org;"}, false},
	} {
		at = at.Add(time.Minute)
		// Authentication headers are not content (§4.1): the subject
		// makes each its own message.
		edit := strings.NewReplacer(append(c.edit, "Subject: Your", "Subject: "+c.name+": your")...)
		id := deliver(t, db, "plain-otp.eml", edit, "github", at)
		d, err := db.MessageByID(ctx, v, id)
		if err != nil {
			t.Fatal(err)
		}
		fs, err := db.FactsFor(ctx, v, id)
		if err != nil || len(fs) != 1 {
			t.Fatalf("%s: facts %+v %v", c.name, fs, err)
		}
		if d.SenderVerified == nil || *d.SenderVerified != c.want || fs[0].FromVerified != c.want {
			t.Errorf("%s: sender verified %v, from.verified %v, want %v", c.name, d.SenderVerified, fs[0].FromVerified, c.want)
		}
		res, err := db.Search(ctx, v, store.SearchQuery{Filter: storetest.Query(t, `subject:"`+d.Subject+`" after:`+at.Format(time.RFC3339)), IncludeDowngraded: true})
		if err != nil || len(res.Hits) != 1 || res.Hits[0].SenderVerified == nil || *res.Hits[0].SenderVerified != c.want {
			t.Errorf("%s: search hit %+v %v", c.name, res.Hits, err)
		}
	}
}
