package store_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

func prefixStat(t *testing.T, db *store.DB, prefix string) (store.PrefixStat, bool) {
	t.Helper()
	sum, err := db.PrefixStats(t.Context(), operator, store.PrefixQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range sum.Prefixes {
		if p.Prefix == prefix {
			return p, true
		}
	}
	return store.PrefixStat{}, false
}

func visible(t *testing.T, db *store.DB, id string) bool {
	t.Helper()
	ms, err := db.ListMessages(t.Context(), operator, store.MessageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.ID == id {
			return true
		}
	}
	return false
}

// Claiming is the operator's decision of §3.3; the downgrade follows it
// at once because it is computed on read (§4.3).
func TestSetPrefixState(t *testing.T) {
	t.Parallel()
	db, pool := load(t)
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	shop := deliver(t, db, "plain-otp.eml", shipped, "shop", at)
	if visible(t, db, shop) {
		t.Fatal("mail to a discovered prefix is not downgraded")
	}
	set := func(prefix string, state int) (store.PrefixChange, error) {
		now := time.Now()
		return db.SetPrefixState(t.Context(), "example.com", prefix, state, store.Action{
			ID: ulid.New(now), Actor: store.SystemPrincipalID, Tool: "prefix.test", At: now, Args: map[string]any{"via": "test"},
		})
	}
	actions := func() int {
		var n int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM actions WHERE tool = 'prefix.test'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if got, err := set("shop", store.PrefixClaimed); err != nil || got != store.PrefixUpdated {
		t.Fatalf("claim shop: %v %v", got, err)
	}
	if !visible(t, db, shop) {
		t.Error("claimed, still downgraded")
	}
	var args map[string]any
	if err := pool.QueryRow(t.Context(), `SELECT args FROM actions WHERE tool = 'prefix.test'`).Scan(&args); err != nil {
		t.Fatal(err)
	}
	if args["prefix"] != "shop" || args["zone"] != "example.com" || args["state"] != "claimed" || args["via"] != "test" {
		t.Errorf("recorded %v", args)
	}
	var changed bool
	if err := pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM changes WHERE kind = 'prefix' AND target_id = 'shop@example.com')`).Scan(&changed); err != nil || !changed {
		t.Errorf("no change logged: %v", err)
	}
	if got, err := set("shop", store.PrefixClaimed); err != nil || got != store.PrefixUnchanged || actions() != 1 {
		t.Errorf("claim twice: %v %v, %d actions", got, err, actions())
	}

	// An address is claimed when it is handed out, before mail.
	if got, err := set("fresh", store.PrefixClaimed); err != nil || got != store.PrefixCreated {
		t.Fatalf("claim fresh: %v %v", got, err)
	}
	if p, ok := prefixStat(t, db, "fresh"); !ok || p.FirstSeen != nil || p.LastSeen != nil || p.Deliveries != 0 || p.State != store.PrefixClaimed {
		t.Errorf("fresh before mail: %+v", p)
	}
	f := deliverFolded(t, db, "plain-otp.eml", nil, "fresh", at.Add(time.Hour))
	if p, _ := prefixStat(t, db, "fresh"); p.FirstSeen == nil || !p.FirstSeen.Equal(at.Add(time.Hour)) || p.Deliveries != 1 || p.State != store.PrefixClaimed {
		t.Errorf("fresh after mail: %+v", p)
	}
	// The OTP already reached the claimed prefix a, so it is not new.
	if f.Notify {
		t.Error("a second delivery of known mail notified")
	}
	if f := deliverFolded(t, db, "plain-otp.eml", shipped, "fresh", at.Add(2*time.Hour)); f.Notify {
		t.Error("shop's message notified again")
	}
	// New mail notifies once its prefix is claimed, not before.
	welcome := strings.NewReplacer("Subject: Your verification code is 482913", "Subject: Welcome aboard")
	if f := deliverFolded(t, db, "plain-otp.eml", welcome, "fresh", at.Add(3*time.Hour)); !f.Notify {
		t.Error("new mail to a claimed prefix did not notify")
	}
	stray := strings.NewReplacer("Subject: Your verification code is 482913", "Subject: Stray")
	if f := deliverFolded(t, db, "plain-otp.eml", stray, "stray", at.Add(3*time.Hour)); f.Notify {
		t.Error("new mail to a discovered prefix notified")
	}

	if got, err := set("shop", store.PrefixDiscovered); err != nil || got != store.PrefixUpdated {
		t.Fatalf("unclaim shop: %v %v", got, err)
	}
	if got, err := set("shop", store.PrefixDiscovered); err != nil || got != store.PrefixUnchanged {
		t.Errorf("unclaim twice: %v %v", got, err)
	}
	if got, err := set("never", store.PrefixClaimed); err != nil || got != store.PrefixCreated {
		t.Fatalf("claim never: %v %v", got, err)
	}
	if got, err := set("never", store.PrefixDiscovered); err != nil || got != store.PrefixRemoved {
		t.Errorf("unclaim never: %v %v", got, err)
	}
	if _, ok := prefixStat(t, db, "never"); ok {
		t.Error("a claim undone before any mail left a row")
	}
	if _, err := set("absent", store.PrefixDiscovered); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unclaim absent: %v", err)
	}

	claimed, err := db.PrefixStats(t.Context(), operator, store.PrefixQuery{State: store.PrefixClaimed, Zone: "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Total != 2 || len(claimed.Prefixes) != 2 {
		t.Errorf("claimed: %+v", claimed.Prefixes)
	}
	other, err := db.PrefixStats(t.Context(), operator, store.PrefixQuery{Zone: "example.org"})
	if err != nil || other.Total != 0 {
		t.Errorf("other zone: %+v %v", other, err)
	}
}
