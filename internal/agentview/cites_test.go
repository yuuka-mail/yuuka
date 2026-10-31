package agentview

import (
	"strings"
	"testing"

	"github.com/yuuka-mail/yuuka/internal/store"
)

func TestCheckCites(t *testing.T) {
	const uri = "yuuka://message/01ARZ3NDEKTSV4RRFFQ69G5FAV"
	many := func(n int) []store.Cite {
		out := make([]store.Cite, n)
		for i := range out {
			out[i] = store.Cite{URI: uri}
		}
		return out
	}
	for name, c := range map[string]struct {
		cites []store.Cite
		want  string // "" is valid
	}{
		"none":          {nil, ""},
		"anchored":      {[]store.Cite{{URI: uri + "#part/1/para/2", Quote: "pay by friday"}}, ""},
		"at the limit":  {many(MaxCites), ""},
		"too many":      {many(MaxCites + 1), "at most 32"},
		"no uri":        {[]store.Cite{{Quote: "x"}}, "cites[0].uri: required"},
		"not a message": {[]store.Cite{{URI: uri}, {URI: "https://example.com/"}}, "cites[1].uri"},
		// Characters, not bytes.
		"quote at the limit": {[]store.Cite{{URI: uri, Quote: strings.Repeat("字", MaxQuoteChars)}}, ""},
		"quote too long":     {[]store.Cite{{URI: uri, Quote: strings.Repeat("q", MaxQuoteChars+1)}}, "cites[0].quote"},
	} {
		err := CheckCites(c.cites)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}
