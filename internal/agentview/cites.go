package agentview

import (
	"fmt"
	"unicode/utf8"

	"github.com/yuuka-mail/yuuka/internal/store"
)

// Bounds on the cites of a proposal or a note (§8.5, §8.10). Enough for
// the passages one judgment rests on; past that a cite list is a copy
// of the mail, which is not what the rows are for.
const (
	MaxCites      = 32
	MaxQuoteChars = 1000
)

// CheckCites validates the cites of a proposal or a note: at most
// MaxCites, each with a yuuka:// message URI (ParseURI) and a quote of
// at most MaxQuoteChars characters. Whether any cite is required is the
// caller's: a note needs one, a proposal does not. REST and MCP call it
// for both, so the two kinds of row hold cites of one shape.
func CheckCites(cs []store.Cite) error {
	if len(cs) > MaxCites {
		return fmt.Errorf("cites: at most %d", MaxCites)
	}
	for i, c := range cs {
		if c.URI == "" {
			return fmt.Errorf("cites[%d].uri: required", i)
		}
		if _, _, err := ParseURI(c.URI); err != nil {
			return fmt.Errorf("cites[%d].uri: %w", i, err)
		}
		if utf8.RuneCountInString(c.Quote) > MaxQuoteChars {
			return fmt.Errorf("cites[%d].quote: at most %d characters", i, MaxQuoteChars)
		}
	}
	return nil
}
