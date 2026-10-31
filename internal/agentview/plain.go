package agentview

import (
	"regexp"
	"strings"
)

var (
	listItem = regexp.MustCompile(`^([ \t]*)(?:[-*+•]|\d{1,3}[.)])[ \t]+(\S.*)$`)
	// originalMessage starts the quoted message in clients that do not
	// prefix quoted lines with ">" (Outlook and its imitators).
	originalMessage = regexp.MustCompile(`(?i)^-{2,}\s*(original message|原始邮件|原始郵件)\s*-{2,}$`)
	outlookRule     = regexp.MustCompile(`^_{10,}$`)
	outlookFrom     = regexp.MustCompile(`(?i)^(from|发件人|寄件者)\s*[:：]`)
	bareURL         = regexp.MustCompile(`https?://[^\s<>"'\x60]+`)
)

// plain converts a text/plain body: paragraphs split at blank lines,
// list items, runs of ">" lines collapsed into quotes, bare URLs as links.
// Line breaks inside a paragraph are kept as the sender wrote them.
func (r *renderer) plain(src string) []Block {
	src = zeroWidth.Replace(strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(src))
	lines := strings.Split(src, "\n")
	var (
		blocks []Block
		cur    []string
		kind   = KindPara
		level  int
		quoted int
	)
	flush := func() {
		if len(cur) > 0 {
			blocks = append(blocks, Block{Kind: kind, Level: level, Spans: linkify(strings.Join(cur, "\n"))})
		}
		cur, kind, level = nil, KindPara, 0
	}
	endQuote := func() {
		if quoted > 0 {
			blocks = quote(blocks, nil, quoted)
			r.stripped.QuotedLines += quoted
			quoted = 0
		}
	}
	for i, line := range lines {
		line = strings.TrimRight(line, " \t")
		t := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(t, ">") {
			flush()
			if strings.TrimLeft(t, "> \t") != "" {
				quoted++
			}
			continue
		}
		endQuote()
		if originalMessage.MatchString(t) || outlookRule.MatchString(t) && outlookFrom.MatchString(nextLine(lines[i+1:])) {
			flush()
			n := 0
			for _, l := range lines[i:] {
				if strings.TrimSpace(l) != "" {
					n++
				}
			}
			r.stripped.QuotedLines += n
			return quote(blocks, nil, n)
		}
		if t == "" {
			flush()
			continue
		}
		if m := listItem.FindStringSubmatch(line); m != nil {
			flush()
			kind, level = KindItem, min(1+len(m[1])/2, 6)
			cur = []string{m[2]}
			continue
		}
		cur = append(cur, line)
	}
	endQuote()
	flush()
	return blocks
}

// nextLine is the first non-blank line of lines, trimmed.
func nextLine(lines []string) string {
	for _, l := range lines {
		if t := strings.TrimSpace(l); t != "" {
			return t
		}
	}
	return ""
}

// linkify turns the bare URLs of a text into link spans whose text is the
// URL itself.
func linkify(s string) []Span {
	var out []Span
	for {
		loc := bareURL.FindStringIndex(s)
		if loc == nil {
			break
		}
		url := trimURL(s[loc[0]:loc[1]])
		end := loc[0] + len(url)
		if loc[0] > 0 {
			out = append(out, Span{Text: s[:loc[0]]})
		}
		out = append(out, Span{Text: url, Href: url})
		s = s[end:]
	}
	if s != "" {
		out = append(out, Span{Text: s})
	}
	return out
}

// trimURL drops punctuation that ends the sentence rather than the URL,
// and a closing parenthesis the URL did not open.
func trimURL(u string) string {
	for u != "" {
		last := u[len(u)-1]
		switch {
		case strings.IndexByte(".,;:!?'\"", last) >= 0:
			u = u[:len(u)-1]
		case last == ')' && strings.Count(u, "(") < strings.Count(u, ")"):
			u = u[:len(u)-1]
		default:
			return u
		}
	}
	return u
}
