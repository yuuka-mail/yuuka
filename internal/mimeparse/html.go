package mimeparse

import (
	"html"
	"strings"
)

// htmlToText reduces an HTML body to the words a reader sees, for search
// and snippets of HTML-only mail. It is not a renderer and does not need
// to be one: the WebUI shows the HTML itself in a sandbox (§12.2), and the
// structured agent view (§8.1) is a separate concern. Tags are dropped,
// block-level tags become line breaks, script/style/head content and
// comments are skipped, entities are decoded.
func htmlToText(s string) string {
	var b strings.Builder
	skip := "" // closing tag whose content is being skipped
	for len(s) > 0 {
		lt := strings.IndexByte(s, '<')
		if lt < 0 {
			if skip == "" {
				b.WriteString(html.UnescapeString(s))
			}
			break
		}
		if skip == "" {
			b.WriteString(html.UnescapeString(s[:lt]))
		}
		s = s[lt:]
		if strings.HasPrefix(s, "<!--") {
			end := strings.Index(s, "-->")
			if end < 0 {
				break
			}
			s = s[end+3:]
			continue
		}
		gt := tagEnd(s)
		if gt < 0 {
			// A stray "<" in text, not a tag.
			if skip == "" {
				b.WriteByte('<')
			}
			s = s[1:]
			continue
		}
		name, closing := tagName(s[1:gt])
		s = s[gt+1:]
		if skip != "" {
			if closing && name == skip {
				skip = ""
			}
			continue
		}
		switch name {
		case "script", "style", "head", "title":
			if !closing {
				skip = name
			}
		case "br", "p", "div", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6",
			"table", "ul", "ol", "blockquote", "hr", "section", "article":
			b.WriteByte('\n')
		case "td", "th":
			b.WriteByte(' ')
		}
	}
	// Collapse the whitespace HTML source is full of, keeping line breaks.
	var out strings.Builder
	blank := 0
	for line := range strings.Lines(b.String()) {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			blank++
			continue
		}
		if out.Len() > 0 {
			if blank > 1 {
				out.WriteString("\n\n")
			} else {
				out.WriteByte('\n')
			}
		}
		blank = 0
		out.WriteString(line)
	}
	return out.String()
}

// tagEnd returns the index of the ">" closing the tag that s starts with,
// skipping quoted attribute values, or -1 if s does not start a tag.
func tagEnd(s string) int {
	if len(s) < 2 || s[1] != '/' && s[1] != '!' && !isLetter(s[1]) {
		return -1
	}
	var quote byte
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return i
		}
	}
	return -1
}

func tagName(t string) (name string, closing bool) {
	if strings.HasPrefix(t, "/") {
		closing, t = true, t[1:]
	}
	end := strings.IndexFunc(t, func(r rune) bool { return r >= 128 || !isLetter(byte(r)) && (r < '0' || r > '9') })
	if end >= 0 {
		t = t[:end]
	}
	return strings.ToLower(t), closing
}

func isLetter(c byte) bool { return c|0x20 >= 'a' && c|0x20 <= 'z' }
