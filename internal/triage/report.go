package triage

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/triage/score"
)

// Caveat goes with every report: what the counts are not.
const Caveat = "Counts are by each message's latest score of this profile, by the option given " +
	"the highest probability; that is no threshold, and no person has judged these. Only mail " +
	"that escapes the §4.3 downgrade is scored; what that hides is not in them."

// WriteReport renders the counts of profile p since then.
func WriteReport(w io.Writer, p score.Profile, since time.Time, c store.ScoreCounts) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	// A failed write shows in Flush, which writes everything.
	say := func(format string, a ...any) { _, _ = fmt.Fprintf(tw, format, a...) }
	say("scorer %s, questions v%d", p.Scorer, p.Questions)
	if !since.IsZero() {
		say(", mail since %s", since.UTC().Format(time.RFC3339))
	}
	say(": %d scored, %d to score\n", c.Scored, c.Unscored)
	// The questions of the set in their order, then any a score holds
	// that the set does not (a backend's extra, or a stale profile).
	var keys []string
	for _, q := range Questions {
		keys = append(keys, q.Key)
	}
	for _, k := range slices.Sorted(maps.Keys(c.Answers)) {
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	for _, q := range keys {
		counts := c.Answers[q]
		say("\n%s\n", q)
		opts := slices.SortedFunc(maps.Keys(counts), func(a, b string) int {
			// Most frequent first; an abstention last.
			if (a == "") != (b == "") {
				return cmp.Compare(b, a)
			}
			return cmp.Or(cmp.Compare(counts[b], counts[a]), cmp.Compare(a, b))
		})
		for _, o := range opts {
			label := o
			if o == "" {
				label = "(abstained)"
			}
			say("  %s\t%d\n", label, counts[o])
		}
		if len(opts) == 0 {
			say("  (no answers)\n")
		}
	}
	say("\n%s\n", Caveat)
	return tw.Flush()
}
