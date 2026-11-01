package notify

import (
	"strings"
	"testing"
	"time"
)

// Every kind names the account and says what to do, on one card; the
// account's name is ours, cleaned all the same.
func TestTroubleText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		card TroubleCard
		want string
	}{
		{TroubleCard{Kind: TroubleRefused}, "yuuka account resume gmail"},
		{TroubleCard{Kind: TroubleFailures, New: 2, Open: 5}, "2 more message(s)"},
		{TroubleCard{Kind: TroubleFailures, New: 2, Open: 5}, "yuuka account sync gmail --retry-failed"},
		{TroubleCard{Kind: TroubleStalled, For: 7*time.Hour + time.Minute, Why: "connection"}, "7 hours (last error: connection"},
		{TroubleCard{Kind: TroubleStalled, For: 7 * time.Hour}, "7 hours (see serve's log)"},
		{TroubleCard{Kind: TroubleBudget}, "--daily-budget"},
		{TroubleCard{Kind: TroubleLimit}, "tomorrow (UTC)"},
		{TroubleCard{Kind: TroubleLocked}, "`yuuka secret ls`"},
		{TroubleCard{Kind: TroubleLocked}, "check the server in `yuuka account ls`"},
		{TroubleCard{Kind: TroubleAltered}, "altered or corrupted"},
		{TroubleCard{Kind: TroubleAltered}, "check the server in `yuuka account ls`"},
	} {
		tc.card.Account = "gmail"
		got := TroubleText(tc.card)
		if !strings.HasPrefix(got, "yuuka · account trouble\ngmail: ") || !strings.Contains(got, tc.want) {
			t.Errorf("kind %d: want %q in %q", tc.card.Kind, tc.want, got)
		}
		tc.card.Account = "gmail\r\nfake"
		if got := TroubleText(tc.card); strings.Contains(got, "\r") || strings.Count(got, "\n") != 1 {
			t.Errorf("kind %d: a control character came through: %q", tc.card.Kind, got)
		}
	}
}
