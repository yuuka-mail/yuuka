package mimeparse

import (
	"html"
	"strings"
	"unicode/utf8"
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

// Link is one <a href> of an HTML part as a reader meets it: where it
// goes, what it says, and the text just before it. It is input for the
// extractor (internal/extract), which decides from the words whether a
// link signs in or verifies; nothing here follows or judges it.
type Link struct {
	// Href is the target as written, entities decoded, never resolved.
	Href string `json:"href"`
	// Text is the anchor's text, with the alt text of images in it.
	Text string `json:"text,omitempty"`
	// Context is the visible text between the previous link and this
	// one, its last linkContextLen characters: the sentence that points
	// at this link, and not one an earlier link already answered.
	Context string `json:"context,omitempty"`
}

// Bounds of the links read from one message. Mail that needs more is a
// newsletter, not a sign-in mail; an href longer than maxHref is not one
// a person is asked to open.
const (
	maxLinks       = 200
	maxHref        = 4096
	linkTextLen    = 200
	linkContextLen = 160
)

// htmlLinks appends the links of the HTML document s to out, up to
// maxLinks, skipping what htmlToText skips. Visible text is tracked with
// the same tokenizer, so Context is what a reader saw before the link.
func htmlLinks(s string, out []Link) []Link {
	var seen, anchor strings.Builder // visible text; text of the open anchor
	href, before, open := "", "", false
	closeAnchor := func() {
		if open && href != "" && len(href) <= maxHref && len(out) < maxLinks {
			out = append(out, Link{Href: href, Text: cutRunes(clean(anchor.String()), linkTextLen, false), Context: before})
		}
		if open {
			seen.Reset()
		}
		open, href = false, ""
		anchor.Reset()
	}
	text := func(t string) {
		seen.WriteString(t)
		if open {
			anchor.WriteString(t)
		}
		// Only the tail is ever read; keep the buffer small.
		if seen.Len() > 8*linkContextLen*utf8.UTFMax {
			tail := seen.String()[seen.Len()-2*linkContextLen*utf8.UTFMax:]
			seen.Reset()
			seen.WriteString(tail)
		}
	}
	skip := ""
	for len(s) > 0 {
		lt := strings.IndexByte(s, '<')
		if lt < 0 {
			if skip == "" {
				text(html.UnescapeString(s))
			}
			break
		}
		if skip == "" {
			text(html.UnescapeString(s[:lt]))
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
			if skip == "" {
				text("<")
			}
			s = s[1:]
			continue
		}
		tag := s[1:gt]
		name, closing := tagName(tag)
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
		case "a":
			closeAnchor()
			if !closing {
				v, _ := attr(tag, "href")
				href, open = strings.TrimSpace(html.UnescapeString(v)), true
				before = cutRunes(clean(seen.String()), linkContextLen, true)
			}
		case "img":
			if alt, ok := attr(tag, "alt"); ok {
				text(" " + html.UnescapeString(alt) + " ")
			}
		case "br", "p", "div", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6",
			"table", "ul", "ol", "blockquote", "hr", "section", "article", "td", "th":
			text("\n")
		}
	}
	closeAnchor()
	return out
}

// attr returns the value of attribute name (lowercase) in tag, the text
// between "<" and ">", and whether the attribute is there at all.
func attr(tag, name string) (string, bool) {
	i := strings.IndexAny(tag, " \t\r\n\f/") // past the tag name
	if i < 0 {
		return "", false
	}
	t := tag[i:]
	const space = " \t\r\n\f"
	for {
		t = strings.TrimLeft(t, space+"/")
		if t == "" {
			return "", false
		}
		end := strings.IndexAny(t, space+"/=")
		if end < 0 {
			end = len(t)
		}
		if end == 0 {
			t = t[1:] // a stray "="
			continue
		}
		key := strings.ToLower(t[:end])
		t = strings.TrimLeft(t[end:], space)
		val, has := "", false
		if strings.HasPrefix(t, "=") {
			has = true
			t = strings.TrimLeft(t[1:], space)
			switch {
			case t == "":
			case t[0] == '"' || t[0] == '\'':
				if q := strings.IndexByte(t[1:], t[0]); q < 0 {
					val, t = t[1:], ""
				} else {
					val, t = t[1:1+q], t[2+q:]
				}
			default:
				e := strings.IndexAny(t, space)
				if e < 0 {
					e = len(t)
				}
				val, t = t[:e], t[e:]
			}
		}
		if key == name {
			return val, has
		}
	}
}

// cutRunes keeps n characters of s: the first n, or the last n if tail.
func cutRunes(s string, n int, tail bool) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if tail {
		return string(r[len(r)-n:])
	}
	return string(r[:n])
}

// HTMLToText is the text a reader sees of an HTML document, as search
// and snippets read it: what the review tier of outgoing mail (§8.9)
// checks besides the markup itself.
func HTMLToText(s string) string { return htmlToText(s) }
