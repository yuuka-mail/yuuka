package compose

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/yuin/goldmark/ast"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/util"
)

// plainText renders doc as plain text without Markdown markup, for
// identities that send formal mail (§10.2.1): **bold** becomes bold, a
// link becomes its text and the URL in angle brackets, a code block is
// indented. What has a natural plain-text form keeps it: list markers,
// "> " quoting. Strikethrough keeps its ~~ because dropping it would
// reverse the meaning of the text.
func plainText(doc ast.Node, src []byte) string {
	p := plain{src: src}
	s := p.block(doc)
	if s == "" {
		return ""
	}
	return s + "\n"
}

type plain struct{ src []byte }

func (p plain) blocks(n ast.Node, sep string) string {
	var parts []string
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if s := p.block(c); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, sep)
}

func (p plain) block(n ast.Node) string {
	switch n := n.(type) {
	case *ast.Paragraph, *ast.TextBlock, *ast.Heading:
		return strings.TrimRight(p.inline(n), "\n")
	case *ast.ThematicBreak:
		return "---"
	case *ast.CodeBlock, *ast.FencedCodeBlock:
		return indent(strings.TrimRight(p.lines(n), "\n"), "    ", "    ")
	case *ast.HTMLBlock:
		s := p.lines(n)
		if n.HasClosure() {
			s += string(n.ClosureLine.Value(p.src))
		}
		return strings.TrimRight(s, "\n")
	case *ast.Blockquote:
		return indent(p.blocks(n, "\n\n"), "> ", "> ")
	case *ast.List:
		sep := "\n\n"
		if n.IsTight {
			sep = "\n"
		}
		var items []string
		i := n.Start
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			marker := "- "
			if n.IsOrdered() {
				marker = strconv.Itoa(i) + ". "
				i++
			}
			items = append(items, indent(p.blocks(c, sep), marker, strings.Repeat(" ", len(marker))))
		}
		return strings.Join(items, sep)
	case *east.Table:
		var rows []string
		for r := n.FirstChild(); r != nil; r = r.NextSibling() {
			var cells []string
			for c := r.FirstChild(); c != nil; c = c.NextSibling() {
				cells = append(cells, p.inline(c))
			}
			rows = append(rows, strings.Join(cells, " | "))
		}
		return strings.Join(rows, "\n")
	default:
		return p.blocks(n, "\n\n")
	}
}

func (p plain) lines(n ast.Node) string {
	var b strings.Builder
	lines := n.Lines()
	for i := range lines.Len() {
		seg := lines.At(i)
		b.Write(seg.Value(p.src))
	}
	return b.String()
}

func (p plain) inline(n ast.Node) string {
	var b strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		p.inlineNode(&b, c)
	}
	return b.String()
}

func (p plain) inlineNode(b *strings.Builder, n ast.Node) {
	switch n := n.(type) {
	case *ast.Text:
		v := n.Segment.Value(p.src)
		if !n.IsRaw() {
			v = unescape(v)
		}
		b.Write(v)
		if n.SoftLineBreak() || n.HardLineBreak() {
			b.WriteByte('\n')
		}
	case *ast.String:
		v := n.Value
		if !n.IsRaw() && !n.IsCode() {
			v = unescape(v)
		}
		b.Write(v)
	case *ast.CodeSpan:
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			if t, ok := c.(*ast.Text); ok {
				b.Write(bytes.ReplaceAll(t.Segment.Value(p.src), []byte("\n"), []byte(" ")))
			}
		}
	case *ast.AutoLink:
		b.Write(n.Label(p.src))
	case *ast.Link:
		label := p.inline(n)
		b.WriteString(label)
		if dest := string(n.Destination); dest != "" && dest != label && dest != "mailto:"+label {
			b.WriteString(" <" + dest + ">")
		}
	case *ast.Image:
		alt := p.inline(n)
		dest := string(n.Destination)
		switch {
		case dest == "":
			b.WriteString(alt)
		case strings.HasPrefix(dest, "cid:"):
			// The image travels with the message; only its name means
			// anything in text.
			b.WriteString("[" + alt + "]")
		default:
			b.WriteString(alt + " <" + dest + ">")
		}
	case *ast.RawHTML:
		segs := n.Segments
		for i := range segs.Len() {
			seg := segs.At(i)
			b.Write(seg.Value(p.src))
		}
	case *east.Strikethrough:
		b.WriteString("~~" + p.inline(n) + "~~")
	default:
		// Emphasis and anything else: the text without the markup.
		b.WriteString(p.inline(n))
	}
}

// unescape resolves what goldmark's HTML writer resolves in text:
// backslash escapes and character references.
func unescape(v []byte) []byte {
	return util.ResolveEntityNames(util.ResolveNumericReferences(util.UnescapePunctuations(v)))
}

// indent prefixes the first line of s with first and the others with
// rest; empty lines get rest without trailing blanks.
func indent(s, first, rest string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		pre := rest
		if i == 0 {
			pre = first
		}
		if l == "" {
			pre = strings.TrimRight(pre, " ")
		}
		lines[i] = pre + l
	}
	return strings.Join(lines, "\n")
}
