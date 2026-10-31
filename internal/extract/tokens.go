package extract

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/width"
)

// The other direction: the exfiltration check of the review tier (§8.9)
// asks whether outgoing mail carries a value the extractor stored from
// incoming mail. It looks values up by equality (the hash index on
// extractions.value), so outgoing text is cut into every string that
// could equal a stored value: Tokens is the inverse of how the matcher
// writes Value, and changes with it. TestTokensCoverExtractions holds
// the two together over the corpus: a change to Value that Tokens does
// not follow fails there, before Version is bumped.
//
// Tokens is deliberately wider than the matcher. The matcher wants
// precision (a code only near words that say so); the check wants
// recall, because an agent that leaks a code will not introduce it the
// way the sender did, and an equality lookup of a string that was never
// extracted costs nothing. What it still cannot see is a value that was
// rewritten: spelled out ("seven four zero"), split by letters, encoded.
// That is for a reader, not for a lookup (§8.9).

// MaxTokens bounds the candidates of one outgoing message. Mail that
// yields more is not something a review by lookup can vouch for; the
// caller escalates it instead of checking a part.
const MaxTokens = 20000

// Tokens returns the strings of text that could be the Value of an otp
// or link extraction, without duplicates:
//
//   - every run of 4-10 ASCII letters and digits, as written and in
//     capitals (codes are stored as typed; an agent may change case);
//   - every run of 4-8 ASCII digits, also inside a longer run of letters
//     and digits ("x740219x");
//   - every chain of digit groups joined by one space, hyphen, dot or
//     no-break space, with the separators removed, for each stretch of
//     consecutive groups that makes 4-8 digits ("7-4-0-2-1-9" and
//     "740 219" both yield 740219);
//   - every http or https URL, as written and without the trailing
//     punctuation the matcher trims.
//
// Full-width letters and digits count as their ASCII forms.
func Tokens(text string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	scan := func(s string) {
		alnumRuns(s, add)
		digitChains(s, add)
		for _, u := range URLs(s) {
			add(u)
		}
	}
	scan(text)
	if folded := width.Fold.String(text); folded != text {
		scan(folded)
	}
	return out
}

// URLs returns the http and https URLs written in text: each match as
// the matcher finds it (plainURL), and without the trailing punctuation
// it trims when that differs.
func URLs(text string) []string {
	var out []string
	for _, u := range plainURL.FindAllString(text, -1) {
		out = append(out, u)
		if t := strings.TrimRight(u, ".,;:!?"); t != u && t != "" {
			out = append(out, t)
		}
	}
	return out
}

func alnumRuns(s string, add func(string)) {
	for i := 0; i < len(s); {
		if !isAlnum(s[i]) {
			i++
			continue
		}
		j := i
		for j < len(s) && isAlnum(s[j]) {
			j++
		}
		if run := s[i:j]; len(run) >= 4 && len(run) <= 10 {
			add(run)
			add(strings.ToUpper(run))
		}
		for k := i; k < j; {
			if !isDigit(s[k]) {
				k++
				continue
			}
			l := k
			for l < j && isDigit(s[l]) {
				l++
			}
			if n := l - k; n >= 4 && n <= 8 {
				add(s[k:l])
			}
			k = l
		}
		i = j
	}
}

// digitChains adds the joined stretches of digit groups (see Tokens).
func digitChains(s string, add func(string)) {
	var groups []string
	flush := func() {
		for a := range groups {
			n := 0
			for b := a; b < len(groups); b++ {
				n += len(groups[b])
				if n > 8 {
					break
				}
				if n >= 4 && b > a {
					add(strings.Join(groups[a:b+1], ""))
				}
			}
		}
		groups = groups[:0]
	}
	for i := 0; i < len(s); {
		if !isDigit(s[i]) {
			i++
			continue
		}
		j := i
		for j < len(s) && isDigit(s[j]) {
			j++
		}
		if i > 0 && isAlnum(s[i-1]) || j < len(s) && isAlnum(s[j]) {
			// Part of a word: alnumRuns has its digits.
			flush()
			i = j
			continue
		}
		// Groups stay in one chain while a separator joins them.
		groups = append(groups, s[i:j])
		i = j
		if separatorAt(s, j) == 0 {
			flush()
		}
	}
	flush()
}

// separatorAt returns the length of a chain separator at s[i:], 0 for
// none: one space, hyphen or dot, or a no-break space, followed by a
// digit.
func separatorAt(s string, i int) int {
	if i >= len(s) {
		return 0
	}
	n := 0
	switch s[i] {
	case ' ', '-', '.':
		n = 1
	default:
		if r, size := utf8.DecodeRuneInString(s[i:]); r == '\u00a0' || r == '\u202f' {
			n = size
		}
	}
	if n == 0 || i+n >= len(s) || !isDigit(s[i+n]) {
		return 0
	}
	return n
}
