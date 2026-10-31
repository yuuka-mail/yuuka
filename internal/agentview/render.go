package agentview

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/yuuka-mail/yuuka/internal/mimeparse"
)

// renderer turns the readable parts of a MIME tree into blocks.
type renderer struct {
	// reply is true for a message with In-Reply-To or References: then
	// every blockquote is a quoted reply, not only those marked as one.
	reply    bool
	stripped Stripped
	parts    []Part
	atts     []Attachment
}

func (r *renderer) collect(e mimeparse.Entity) {
	switch {
	case e.MediaType == "multipart/alternative":
		// The last representation that yields any text: RFC 2046 puts
		// the most faithful one last, and HTML keeps headings, lists and
		// link targets that the plain version flattens.
		for i := len(e.Children) - 1; i >= 0; i-- {
			sub := renderer{reply: r.reply}
			sub.collect(e.Children[i])
			if len(sub.parts) > 0 {
				r.parts = append(r.parts, sub.parts...)
				r.stripped.add(sub.stripped)
				return
			}
		}
	case strings.HasPrefix(e.MediaType, "multipart/"):
		for _, c := range e.Children {
			r.collect(c)
		}
	case e.Attachment:
	case e.MediaType == "text/html":
		r.add(e, r.html(e.Text))
	case e.MediaType == "text/plain":
		r.add(e, r.plain(e.Text))
	}
}

// add numbers the blocks of one part.
func (r *renderer) add(e mimeparse.Entity, blocks []Block) {
	if len(blocks) == 0 {
		return
	}
	p := Part{Anchor: Anchor{Part: e.Section}.String(), MediaType: e.MediaType, Blocks: blocks}
	for i := range p.Blocks {
		p.Blocks[i].Anchor = Anchor{Part: e.Section, Para: i + 1}.String()
	}
	r.parts = append(r.parts, p)
}

// attachments lists every attachment of the tree, in alternatives too.
func (r *renderer) attachments(e mimeparse.Entity) {
	if e.Attachment {
		r.atts = append(r.atts, Attachment{Section: e.Section, Filename: e.Filename, MediaType: e.MediaType})
	}
	for _, c := range e.Children {
		r.attachments(c)
	}
}

func (s *Stripped) add(o Stripped) {
	s.TrackingPixels += o.TrackingPixels
	s.Images += o.Images
	s.Hidden += o.Hidden
	s.UnsafeLinks += o.UnsafeLinks
	s.QuotedLines += o.QuotedLines
}

// attribution matches the line a mail client writes above a quote.
var attribution = regexp.MustCompile(`(?i)(wrote|writes|schrieb|a écrit|escribió|scrisse|написал|写道|寫道)\s*[:：]\s*$`)

// quote appends a collapsed quote of lines lines. An attribution line
// right before it moves into the quote block, so the marker reads
// "On ..., X wrote: (n lines)".
func quote(blocks []Block, attr []Span, lines int) []Block {
	if attr == nil && len(blocks) > 0 {
		last := blocks[len(blocks)-1]
		if last.Kind == KindQuote {
			// Quoted runs split only by blank lines are one quote.
			blocks[len(blocks)-1].Lines += lines
			return blocks
		}
		if last.Kind == KindPara && attribution.MatchString(last.Text()) {
			attr = last.Spans
			blocks = blocks[:len(blocks)-1]
		}
	}
	return append(blocks, Block{Kind: KindQuote, Spans: attr, Lines: lines})
}

// inline accumulates the spans of the block being built, collapsing
// whitespace as HTML does.
type inline struct {
	spans []Span
	pre   bool
}

// zeroWidth are dropped: newsletters pad preheaders with them, and they
// can hide text from a human while a model still reads it.
var zeroWidth = strings.NewReplacer(
	"\u200b", "", "\u200c", "", "\u200d", "", "\u2060", "", "\ufeff", "", "\u00ad", "", "\u034f", "",
)

func (in *inline) text(s, href string) {
	s = zeroWidth.Replace(s)
	if !in.pre {
		var b strings.Builder
		space := in.endsInSpace()
		for _, r := range s {
			if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == '\u00a0' {
				if !space {
					b.WriteByte(' ')
				}
				space = true
				continue
			}
			space = false
			b.WriteRune(r)
		}
		s = b.String()
	}
	if s == "" {
		return
	}
	if n := len(in.spans); n > 0 && in.spans[n-1].Href == href {
		in.spans[n-1].Text += s
		return
	}
	in.spans = append(in.spans, Span{Text: s, Href: href})
}

// link adds an empty link, for a link with no text (an image button
// without alt text): its target is still real and worth keeping.
func (in *inline) link(href string) {
	in.spans = append(in.spans, Span{Href: href})
}

// newline adds a line break. It reports false if the block already ends
// in one, which makes a second <br> a paragraph break.
func (in *inline) newline() bool {
	in.trimRight(" ")
	if in.empty() || in.endsWith('\n') {
		return false
	}
	in.spans[len(in.spans)-1].Text += "\n"
	return true
}

func (in *inline) endsWith(c byte) bool {
	for i := len(in.spans) - 1; i >= 0; i-- {
		if t := in.spans[i].Text; t != "" {
			return t[len(t)-1] == c
		}
	}
	return false
}

func (in *inline) endsInSpace() bool {
	return in.empty() || in.endsWith(' ') || in.endsWith('\n')
}

func (in *inline) empty() bool {
	for _, s := range in.spans {
		if s.Text != "" || s.Href != "" {
			return false
		}
	}
	return true
}

func (in *inline) trimRight(cut string) {
	for len(in.spans) > 0 {
		last := &in.spans[len(in.spans)-1]
		last.Text = strings.TrimRight(last.Text, cut)
		if last.Text != "" || last.Href != "" {
			return
		}
		in.spans = in.spans[:len(in.spans)-1]
	}
}

// take returns the finished spans and resets.
func (in *inline) take() []Span {
	in.trimRight(" \n")
	if len(in.spans) > 0 && !in.pre {
		in.spans[0].Text = strings.TrimLeft(in.spans[0].Text, " \n")
	}
	var out []Span
	for _, s := range in.spans {
		if s.Text != "" || s.Href != "" {
			out = append(out, s)
		}
	}
	in.spans = nil
	if len(out) == 0 {
		return nil
	}
	return out
}

// countLines counts the non-empty lines of blocks.
func countLines(blocks []Block) int {
	n := 0
	for _, b := range blocks {
		for line := range strings.SplitSeq(b.Text(), "\n") {
			if strings.TrimSpace(line) != "" {
				n++
			}
		}
		n += b.Lines
	}
	return n
}

// safeScheme keeps link targets a reader can sensibly be shown;
// javascript:, data:, cid: and relative references are not targets.
func safeScheme(href string) bool {
	scheme, _, ok := strings.Cut(href, ":")
	if !ok {
		return false
	}
	switch strings.ToLower(scheme) {
	case "http", "https", "mailto", "tel":
		return true
	}
	return false
}

// pixelSize reports whether an HTML length is at most one pixel.
func pixelSize(v string) bool {
	v = strings.TrimSuffix(strings.TrimSpace(strings.ToLower(v)), "px")
	n, err := strconv.ParseFloat(v, 64)
	return err == nil && n <= 1
}
