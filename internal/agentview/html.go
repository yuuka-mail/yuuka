package agentview

import (
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// maxHTMLDepth bounds the element nesting the converter descends into.
// Hostile mail can nest thousands of elements; deeper content is dropped.
const maxHTMLDepth = 256

// html converts an HTML body. It parses with the HTML5 algorithm, so
// malformed markup yields the same tree a browser would build.
func (r *renderer) html(src string) []Block {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		// Only a failing reader makes Parse fail; a string never does.
		return r.plain(src)
	}
	c := htmlConv{reply: r.reply, stripped: &r.stripped}
	c.walk(doc, 0)
	return c.finish()
}

type blockCtx struct {
	kind  Kind
	level int
}

type htmlConv struct {
	reply    bool
	stripped *Stripped
	blocks   []Block
	in       inline
	ctx      []blockCtx
	lists    int
	pres     int
	href     string // target of the link being read
	linkText bool   // the current link got some text
	// cellSep defers the separator between table cells until the next
	// cell has text, so empty layout cells add nothing.
	cellSep bool
	// rest is set once an Outlook reply header starts: everything after
	// it is the quoted message, collected here and collapsed at the end.
	rest   bool
	before []Block
}

// skipped elements never contribute text.
var skipped = map[string]bool{
	"head": true, "title": true, "script": true, "style": true, "template": true,
	"noscript": true, "svg": true, "math": true, "iframe": true, "object": true,
	"embed": true, "canvas": true, "audio": true, "video": true, "input": true,
	"select": true, "textarea": true, "meta": true, "link": true, "base": true,
}

// blockTags end the current block where they open and close.
var blockTags = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true, "caption": true,
	"center": true, "dd": true, "details": true, "dialog": true, "div": true, "dl": true,
	"dt": true, "fieldset": true, "figcaption": true, "figure": true, "footer": true,
	"form": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"header": true, "li": true, "main": true, "nav": true, "ol": true, "p": true,
	"pre": true, "section": true, "summary": true, "table": true, "tr": true, "ul": true,
}

func (c *htmlConv) walk(n *html.Node, depth int) {
	switch n.Type {
	case html.TextNode:
		c.text(n.Data)
		return
	case html.DocumentNode:
		c.children(n, depth)
		return
	case html.ElementNode:
	default: // comments, doctype
		return
	}
	if depth > maxHTMLDepth {
		return
	}
	tag := n.Data
	switch {
	case skipped[tag]:
		return
	case tag == "img":
		c.img(n)
		return
	case hidden(n):
		c.stripped.Hidden++
		return
	case attr(n, "id") == "divRplyFwdMsg" && !c.rest:
		c.flush()
		c.rest, c.before, c.blocks = true, c.blocks, nil
	case c.isQuote(n):
		c.quote(n, depth)
		return
	}
	switch tag {
	case "br":
		if !c.in.newline() {
			c.flush()
		}
		return
	case "hr":
		c.flush()
		return
	case "a":
		c.link(n, depth)
		return
	case "td", "th":
		if !c.in.empty() {
			c.cellSep = true
		}
	}
	block := blockTags[tag]
	if block {
		c.flush()
	}
	pushed := true
	switch tag {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		c.ctx = append(c.ctx, blockCtx{KindHeading, int(tag[1] - '0')})
	case "li":
		c.ctx = append(c.ctx, blockCtx{KindItem, max(c.lists, 1)})
	case "pre":
		c.ctx = append(c.ctx, blockCtx{KindPre, 0})
		c.pres++
		c.in.pre = true
	default:
		pushed = false
	}
	if tag == "ul" || tag == "ol" {
		c.lists++
	}
	c.children(n, depth)
	if block {
		c.flush()
	}
	if tag == "ul" || tag == "ol" {
		c.lists--
	}
	if pushed {
		c.ctx = c.ctx[:len(c.ctx)-1]
	}
	if tag == "pre" {
		c.pres--
		c.in.pre = c.pres > 0
	}
}

func (c *htmlConv) children(n *html.Node, depth int) {
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		c.walk(ch, depth+1)
	}
}

func (c *htmlConv) text(s string) {
	if c.in.pre {
		s = strings.ReplaceAll(s, "\r\n", "\n")
	}
	if c.cellSep && strings.TrimSpace(s) != "" {
		c.in.text(" | ", "")
		c.cellSep = false
	}
	if c.href != "" && strings.TrimSpace(s) != "" {
		c.linkText = true
	}
	c.in.text(s, c.href)
}

func (c *htmlConv) link(n *html.Node, depth int) {
	href := strings.TrimSpace(attr(n, "href"))
	if href != "" && !safeScheme(href) {
		if !strings.HasPrefix(href, "#") {
			c.stripped.UnsafeLinks++
		}
		href = ""
	}
	outerHref, outerText := c.href, c.linkText
	c.href, c.linkText = href, false
	c.children(n, depth)
	if href != "" && !c.linkText {
		c.in.link(href)
	}
	c.href, c.linkText = outerHref, outerText
}

func (c *htmlConv) img(n *html.Node) {
	if hidden(n) || pixelSize(attr(n, "width")) || pixelSize(attr(n, "height")) || stylePixel(n) {
		c.stripped.TrackingPixels++
		return
	}
	alt := strings.Join(strings.Fields(attr(n, "alt")), " ")
	if alt == "" {
		c.stripped.Images++
		return
	}
	c.text("[image: " + alt + "]")
}

// quoteClasses mark the quoted part of a reply in the HTML of common
// mail clients.
var quoteClasses = []string{"gmail_quote", "gmail_quote_container", "yahoo_quoted", "protonmail_quote"}

func (c *htmlConv) isQuote(n *html.Node) bool {
	if n.Data == "blockquote" && (c.reply || strings.EqualFold(attr(n, "type"), "cite")) {
		return true
	}
	for class := range strings.FieldsSeq(attr(n, "class")) {
		for _, q := range quoteClasses {
			if class == q {
				return true
			}
		}
	}
	return false
}

// quote collapses n into one quote block.
func (c *htmlConv) quote(n *html.Node, depth int) {
	c.flush()
	sub := htmlConv{reply: c.reply, stripped: &Stripped{}}
	sub.children(n, depth)
	inner := sub.finish()
	var attr []Span
	if len(inner) > 0 {
		switch b := inner[0]; {
		case b.Kind == KindPara && attribution.MatchString(b.Text()):
			attr, inner = b.Spans, inner[1:]
		case b.Kind == KindQuote && b.Spans != nil:
			// Gmail nests the attribution and the quote in one
			// container; the inner quote already claimed the line.
			attr = b.Spans
			inner = append([]Block{{Kind: KindQuote, Lines: b.Lines}}, inner[1:]...)
		}
	}
	lines := countLines(inner)
	c.stripped.QuotedLines += lines
	c.blocks = quote(c.blocks, attr, lines)
}

func (c *htmlConv) flush() {
	c.cellSep = false
	spans := c.in.take()
	if spans == nil {
		return
	}
	b := Block{Kind: KindPara, Spans: spans}
	if n := len(c.ctx); n > 0 {
		b.Kind, b.Level = c.ctx[n-1].kind, c.ctx[n-1].level
	}
	c.blocks = append(c.blocks, b)
}

func (c *htmlConv) finish() []Block {
	c.flush()
	if !c.rest {
		return c.blocks
	}
	lines := countLines(c.blocks)
	c.stripped.QuotedLines += lines
	return quote(c.before, nil, lines)
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Namespace == "" && a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Namespace == "" && a.Key == key {
			return true
		}
	}
	return false
}

// compactStyle lowercases a style attribute and drops its whitespace, so
// "Display : None" and "display:none" compare equal.
func compactStyle(n *html.Node) string {
	return strings.Join(strings.Fields(strings.ToLower(attr(n, "style"))), "")
}

var (
	hiddenStyle = regexp.MustCompile(`(^|;)(display:none|visibility:hidden|mso-hide:all|opacity:0(\.0+)?(;|!|$)|font-size:0(\.0+)?(px|pt|em|rem|%)?(;|!|$))`)
	zeroHeight  = regexp.MustCompile(`(^|;)max-height:0(px)?(;|!|$)`)
	pixelStyle  = regexp.MustCompile(`(^|;)(width|height):(\d+(\.\d+)?)(px)?(;|!|$)`)
)

// hidden reports an element a reader does not see: the trick behind
// preheaders, and behind text meant only for a model.
func hidden(n *html.Node) bool {
	if hasAttr(n, "hidden") {
		return true
	}
	s := compactStyle(n)
	return hiddenStyle.MatchString(s) || zeroHeight.MatchString(s) && strings.Contains(s, "overflow:hidden")
}

func stylePixel(n *html.Node) bool {
	for _, m := range pixelStyle.FindAllStringSubmatch(compactStyle(n), -1) {
		if pixelSize(m[3]) {
			return true
		}
	}
	return false
}
