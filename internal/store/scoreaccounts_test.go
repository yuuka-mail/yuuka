package store_test

import (
	"os"
	"slices"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// Only the accounts the host names are scored (§10.1.4); by default the
// Cloudflare accounts, never a pulled mailbox, whose whole history the
// person who added it did not mean to hand to a scorer.
func TestScoreAccounts(t *testing.T) {
	t.Parallel()
	e := newPullEnv(t)
	ctx := t.Context()
	src := e.add("gmail", "me@example.org")
	e.mailbox(src.AccountID, "All", store.KeyGmail, 7, 0)
	raw, err := os.ReadFile("../../testdata/eml/plain-otp.eml")
	if err != nil {
		t.Fatal(err)
	}
	p := e.pull(src.AccountID, store.GmailSourceKey(1), store.RemoteLocation{Mailbox: "All", UIDValidity: 7, UID: 1}, raw)
	if _, err := e.db.CommitPull(ctx, p); err != nil {
		t.Fatal(err)
	}
	f, err := e.db.Fold(ctx, store.RawRef{
		DeliveryID: p.DeliveryID, RawID: p.RawID, R2Key: p.R2Key, LocalPath: p.LocalPath, ContentHash: p.ContentHash[:],
		Zone: "example.org", Prefix: "me", ReceivedAt: p.InternalDate, AccountID: src.AccountID,
	}, mimeparse.Parse(raw), authres.Auth{})
	if err != nil {
		t.Fatal(err)
	}
	pulled := f.MessageID
	cf := deliver(t, e.db, "alternative-reply.eml", nil, "a", p.InternalDate)
	if _, err := e.pool.Exec(ctx, `UPDATE prefixes SET state = 1`); err != nil {
		t.Fatal(err)
	}
	since := p.InternalDate.Add(-time.Hour)
	for _, c := range []struct {
		accounts []string
		want     []string
	}{
		{nil, []string{cf}},
		{[]string{"gmail"}, []string{pulled}},
		{[]string{"gmail", "cf"}, []string{cf, pulled}},
		{[]string{}, nil},
	} {
		got, err := e.db.ScoreTargets(ctx, laya, c.accounts, since, 10)
		slices.Sort(got)
		slices.Sort(c.want)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("ScoreTargets(%q) = %q, %v; want %q", c.accounts, got, err, c.want)
		}
		for _, id := range []string{cf, pulled} {
			if ok, err := e.db.IsScoreTarget(ctx, id, c.accounts); err != nil || ok != slices.Contains(c.want, id) {
				t.Errorf("IsScoreTarget(%s, %q) = %v, %v", id, c.accounts, ok, err)
			}
		}
		if page, err := e.db.RescoreTargets(ctx, c.accounts, since, "", 10); err != nil || len(page) != len(c.want) {
			t.Errorf("RescoreTargets(%q) = %q, %v", c.accounts, page, err)
		}
		if n, err := e.db.ScoreCounts(ctx, &auth.Viewer{PrincipalID: systemID, PrincipalKind: "system", PrincipalName: "system"}, laya, c.accounts, since); err != nil || n.Unscored != len(c.want) {
			t.Errorf("ScoreCounts(%q) = %+v, %v", c.accounts, n, err)
		}
	}
}
