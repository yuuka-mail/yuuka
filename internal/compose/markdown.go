package compose

import (
	"bytes"
	"fmt"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Inline styles (§10.2.1). Mail clients drop <head> and <style>, so every
// element that needs a look carries it in a style attribute. They are set
// on the AST before rendering rather than by a CSS inliner run over the
// HTML afterwards: goldmark renders node attributes itself, and an inliner
// would mean parsing our own output again plus another dependency.
const (
	styleBlockquote = "margin:0 0 0 0.8ex;border-left:2px solid #ccc;padding-left:1ex;color:#555"
	stylePre        = "background-color:#f6f8fa;padding:8px 12px;border-radius:4px;" +
		"font-family:monospace;white-space:pre-wrap;word-wrap:break-word"
	styleCode  = "background-color:#f6f8fa;padding:1px 4px;border-radius:3px;font-family:monospace"
	styleTable = "border-collapse:collapse"
	styleCell  = "border:1px solid #ccc;padding:4px 8px"
	styleImg   = "max-width:100%"
	styleHR    = "border:none;border-top:1px solid #ccc"
	styleRaw   = "white-space:pre-wrap"
)

// md is the one Markdown dialect of drafts: CommonMark plus the GFM parts
// that make sense in mail (tables, strikethrough, bare links). Task lists
// would render form controls, and there is no syntax highlighting.
//
// Raw HTML is never passed through: a draft may have been written by an
// agent reading untrusted mail (§8.3). It is shown escaped instead of
// goldmark's default of dropping it, so the HTML part says what the plain
// part says.
//
// Hard wraps: a newline in the draft is a line break in the HTML, as in
// GitHub comments. Drafts are typed in a text box where signatures and
// addresses rely on line breaks, and the plain part keeps them anyway.
var md = goldmark.New(
	goldmark.WithExtensions(
		extension.NewTable(extension.WithTableCellAlignMethod(extension.TableCellAlignStyle)),
		extension.Strikethrough,
		extension.Linkify,
	),
	goldmark.WithParserOptions(
		parser.WithASTTransformers(util.Prioritized(styler{}, 100)),
	),
	goldmark.WithRendererOptions(
		html.WithHardWraps(),
		renderer.WithNodeRenderers(util.Prioritized(mailRenderer{}, 100)),
	),
)

func parse(body string) (ast.Node, []byte) {
	src := []byte(body)
	return md.Parser().Parse(text.NewReader(src)), src
}

// renderHTML renders doc as a complete HTML document.
func renderHTML(doc ast.Node, src []byte) (string, error) {
	var b bytes.Buffer
	b.WriteString("<!DOCTYPE html>\n<html>\n<head>\n" +
		`<meta http-equiv="Content-Type" content="text/html; charset=utf-8">` +
		"\n</head>\n<body>\n")
	if err := md.Renderer().Render(&b, src, doc); err != nil {
		return "", fmt.Errorf("render markdown: %w", err)
	}
	b.WriteString("</body>\n</html>\n")
	return b.String(), nil
}

// styler sets the inline styles, and the table attributes old clients
// need, on the nodes goldmark renders with their attributes.
type styler struct{}

func (styler) Transform(doc *ast.Document, _ text.Reader, _ parser.Context) {
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		set := func(k, v string) { n.SetAttributeString(k, []byte(v)) }
		switch n.Kind() {
		case ast.KindBlockquote:
			set("style", styleBlockquote)
		case ast.KindCodeSpan:
			set("style", styleCode)
		case ast.KindImage:
			set("style", styleImg)
		case ast.KindThematicBreak:
			set("style", styleHR)
		case east.KindTable:
			set("border", "1")
			set("cellpadding", "6")
			set("cellspacing", "0")
			set("style", styleTable)
		case east.KindTableCell:
			// The table extension appends text-align to this.
			set("style", styleCell)
		}
		return ast.WalkContinue, nil
	})
}

// mailRenderer replaces goldmark's renderers where mail needs something
// else: code blocks are a styled <pre> with no language class, raw HTML is
// shown as text.
type mailRenderer struct{}

func (mailRenderer) RegisterFuncs(r renderer.NodeRendererFuncRegisterer) {
	r.Register(ast.KindCodeBlock, renderCodeBlock)
	r.Register(ast.KindFencedCodeBlock, renderCodeBlock)
	r.Register(ast.KindHTMLBlock, renderHTMLBlock)
	r.Register(ast.KindRawHTML, renderRawHTML)
}

func renderCodeBlock(w util.BufWriter, src []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		_, _ = w.WriteString(`<pre style="` + stylePre + `"><code>`)
		lines := n.Lines()
		for i := range lines.Len() {
			seg := lines.At(i)
			_, _ = w.Write(util.EscapeHTML(seg.Value(src)))
		}
	} else {
		_, _ = w.WriteString("</code></pre>\n")
	}
	return ast.WalkContinue, nil
}

func renderHTMLBlock(w util.BufWriter, src []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	n := node.(*ast.HTMLBlock)
	var b []byte
	lines := n.Lines()
	for i := range lines.Len() {
		seg := lines.At(i)
		b = append(b, seg.Value(src)...)
	}
	if n.HasClosure() {
		b = append(b, n.ClosureLine.Value(src)...)
	}
	_, _ = w.WriteString(`<p style="` + styleRaw + `">`)
	_, _ = w.Write(util.EscapeHTML(bytes.TrimRight(b, "\n")))
	_, _ = w.WriteString("</p>\n")
	return ast.WalkContinue, nil
}

func renderRawHTML(w util.BufWriter, src []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		segs := node.(*ast.RawHTML).Segments
		for i := range segs.Len() {
			seg := segs.At(i)
			_, _ = w.Write(util.EscapeHTML(seg.Value(src)))
		}
	}
	return ast.WalkSkipChildren, nil
}
