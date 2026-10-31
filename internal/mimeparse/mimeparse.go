// Package mimeparse is the one MIME parser of yuuka (§2, §5): it turns a
// raw message into the derived columns of messages. Everything it returns
// is a pure function of the raw bytes and Version, so reparse can rebuild
// it at any time (§4.2).
//
// It never fails. Mail that cannot be parsed cleanly still yields whatever
// could be read, plus a Problem; the caller keeps the message in a
// quarantine state instead of dropping it (§3 principle 5).
package mimeparse

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/emersion/go-message"
	// Registers legacy charsets (GB2312, GBK, Big5, Shift_JIS...) with
	// go-message; Chinese notification mail commonly uses them (§5).
	"github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/mail"
)

// Version is messages.parser_version. Bump it whenever the output for the
// same input changes, so rows parsed by an older parser can be found.
const Version = 1

// Limits keep hostile input from costing unbounded work. Hitting one is a
// Problem, not a failure: what was read so far is kept.
const (
	maxDepth = 16
	maxParts = 500
	// MaxBodyText bounds messages.body_text. The raw message is the source
	// of truth; the column only has to serve search and snippets.
	MaxBodyText = 1 << 20
	snippetLen  = 200
)

// Message is what parsing yields for one raw message.
type Message struct {
	Subject    string    `json:"subject"`
	FromAddr   string    `json:"from_addr"`
	FromName   string    `json:"from_name"`
	Date       time.Time `json:"date"` // zero if missing or unparseable
	MessageID  string    `json:"message_id"`
	InReplyTo  string    `json:"in_reply_to"`
	References []string  `json:"references"`
	Snippet    string    `json:"snippet"`
	BodyText   string    `json:"body_text"`
	HasAttach  bool      `json:"has_attach"`

	// Header signals for classification (§3 principle 4).
	MediaType       string `json:"media_type"`
	ListID          string `json:"list_id"`
	ListUnsubscribe bool   `json:"list_unsubscribe"`
	AutoSubmitted   string `json:"auto_submitted"`
	Precedence      string `json:"precedence"`

	// Problem is non-empty when the message could not be parsed cleanly;
	// such a message is quarantined with its raw kept.
	Problem string `json:"problem"`
}

// Parse parses raw.
func Parse(raw []byte) Message {
	m, _ := ParseTree(raw)
	return m
}

// Entity is one node of the MIME tree as far as the parser reads it: the
// structure that views of the body need (the agent view of §8.1), not the
// whole MIME model. Parts beyond the limits, and the insides of attached
// messages, are not in it.
type Entity struct {
	// Section is the IMAP section number (RFC 3501 §6.4.5): "1" for the
	// body of a single-part message, "1", "2", "2.1" ... for the parts of
	// multiparts, "" for a top-level multipart. It follows from the MIME
	// structure alone, so the same raw always numbers the same way; the
	// yuuka:// anchors of §8.1 are built on it.
	Section   string `json:"section"`
	MediaType string `json:"media_type"`
	Filename  string `json:"filename,omitempty"`
	// Attachment is the has_attach decision for this part (isAttachment).
	Attachment bool `json:"attachment,omitempty"`
	// Text is the decoded body of a text/plain or text/html part, as valid
	// UTF-8. A multipart whose parts cannot be read becomes a text/plain
	// entity holding its undecoded body, so its words are not lost.
	Text     string   `json:"-"`
	Children []Entity `json:"children,omitempty"`
}

// ParseTree parses raw like Parse and also returns its MIME tree.
func ParseTree(raw []byte) (Message, Entity) {
	var p parser
	e, err := message.Read(bytes.NewReader(raw))
	if err != nil && e == nil {
		// The header block itself is malformed. Drop the lines that are
		// not fields and try again, so the message still gets a subject
		// and a sender in the quarantine view.
		p.problem("header: %v", err)
		e, err = message.Read(bytes.NewReader(sanitizeHeader(raw)))
		if err != nil && e == nil {
			p.problem("header after cleanup: %v", err)
			return p.fallback(raw)
		}
	}
	// A top-level unknown charset or transfer encoding is not fatal:
	// go-message hands back the undecoded body.
	p.charset = e.Header.Get("Content-Type")
	p.headers(mail.Header{Header: e.Header})
	root, _ := p.walk(e, 0, "")
	var plain []string
	for _, t := range root.texts() {
		s := t.text
		if t.html {
			s = htmlToText(s)
		}
		if s = strings.TrimSpace(s); s != "" {
			plain = append(plain, s)
		}
	}
	p.m.BodyText = truncate(strings.Join(plain, "\n\n"), MaxBodyText)
	p.m.Snippet = snippet(p.m.BodyText)
	p.m.Problem = strings.Join(p.problems, "; ")
	return p.m, root
}

type parser struct {
	m        Message
	problems []string
	parts    int
	charset  string // top-level Content-Type, for undeclared 8-bit headers
}

func (p *parser) problem(format string, args ...any) {
	p.problems = append(p.problems, fmt.Sprintf(format, args...))
}

func (p *parser) headers(h mail.Header) {
	p.m.Subject = clean(p.text(h, "Subject"))
	p.from(h)
	if d, err := h.Date(); err == nil {
		p.m.Date = d.UTC()
	}
	p.m.MessageID = msgID(h, "Message-Id")
	if ids := msgIDs(h, "In-Reply-To"); len(ids) > 0 {
		p.m.InReplyTo = ids[0]
	}
	p.m.References = msgIDs(h, "References")
	if p.m.References == nil {
		p.m.References = []string{}
	}
	mt, _, _ := h.ContentType()
	p.m.MediaType = strings.ToLower(mt)
	p.m.ListID = clean(p.text(h, "List-Id"))
	p.m.ListUnsubscribe = h.Has("List-Unsubscribe")
	p.m.AutoSubmitted = strings.ToLower(clean(h.Get("Auto-Submitted")))
	p.m.Precedence = strings.ToLower(clean(h.Get("Precedence")))
}

// text decodes an unstructured header field. Encoded words in any charset
// go-message knows are decoded; raw 8-bit bytes that are not UTF-8 are
// read in the charset the message body declares, which is what the
// mailers that send them mean.
func (p *parser) text(h mail.Header, key string) string {
	v, err := h.Text(key)
	if err != nil {
		v = h.Get(key)
	}
	if !utf8.ValidString(v) {
		if _, params, err := mime.ParseMediaType(p.charset); err == nil && params["charset"] != "" {
			if r, err := charset.Reader(params["charset"], strings.NewReader(v)); err == nil {
				if b, err := io.ReadAll(r); err == nil {
					v = string(b)
				}
			}
		}
	}
	return v
}

var addrPattern = regexp.MustCompile(`[^\s<>"(),;:]+@[^\s<>"(),;:]+`)

func (p *parser) from(h mail.Header) {
	raw := h.Get("From")
	if raw == "" {
		return
	}
	if l, err := h.AddressList("From"); err == nil && len(l) > 0 {
		// Lowercased: from_addr is compared for threading and grouping,
		// and nobody relies on case in the local part.
		p.m.FromAddr = strings.ToLower(l[0].Address)
		p.m.FromName = clean(l[0].Name)
		return
	}
	// Spam and broken mailers write From in every way RFC 5322 forbids;
	// salvage an address and keep the rest as the name.
	text := clean(p.text(h, "From"))
	if a := addrPattern.FindString(text); a != "" {
		p.m.FromAddr = strings.ToLower(a)
		name := strings.TrimSpace(strings.ReplaceAll(text, a, ""))
		p.m.FromName = strings.Trim(name, ` "<>`)
		return
	}
	p.m.FromName = text
}

// msgID reads one msg-id without angle brackets. Malformed values are
// kept trimmed rather than dropped: they still thread with each other.
func msgID(h mail.Header, key string) string {
	ids := msgIDs(h, key)
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func msgIDs(h mail.Header, key string) []string {
	v := h.Get(key)
	if strings.TrimSpace(v) == "" {
		return nil
	}
	if ids, err := h.MsgIDList(key); err == nil {
		return ids
	}
	var out []string
	for f := range strings.FieldsSeq(strings.NewReplacer(",", " ", "<", " ", ">", " ").Replace(v)) {
		if utf8.ValidString(f) && !strings.HasPrefix(f, "(") {
			out = append(out, f)
		}
	}
	return out
}

type textPart struct {
	text string
	html bool
}

// walk reads e into an Entity numbered sec; "" only for the top level,
// where a single part is numbered "1". ok is false for a part beyond the
// limits, which is left out of the tree.
func (p *parser) walk(e *message.Entity, depth int, sec string) (ent Entity, ok bool) {
	if depth > maxDepth {
		p.problem("nesting deeper than %d", maxDepth)
		return ent, false
	}
	if p.parts++; p.parts > maxParts {
		if p.parts == maxParts+1 {
			p.problem("more than %d parts", maxParts)
		}
		return ent, false
	}
	mt, params, err := e.Header.ContentType()
	if err != nil {
		// Unparseable Content-Type: RFC 2045 says treat as text/plain.
		mt, params = "text/plain", nil
	}
	mt = strings.ToLower(mt)
	if mr := e.MultipartReader(); mr != nil {
		return p.multipart(e, mr, mt, depth, sec), true
	}
	disp, dparams, _ := e.Header.ContentDisposition()
	disp = strings.ToLower(disp)
	ent = Entity{Section: leafSection(sec), MediaType: mt, Filename: clean(dparams["filename"])}
	if ent.Filename == "" {
		ent.Filename = clean(params["name"])
	}
	if isAttachment(mt, disp, params["name"] != "" || dparams["filename"] != "", e.Header.Has("Content-Id")) {
		p.m.HasAttach = true
		ent.Attachment = true
		return ent, true
	}
	if mt != "text/plain" && mt != "text/html" {
		return ent, true
	}
	body, err := io.ReadAll(io.LimitReader(e.Body, 4*MaxBodyText))
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		// A missing closing boundary shows up here as ErrUnexpectedEOF;
		// it is common and harmless. Anything else (bad base64, say) is
		// worth a look.
		p.problem("%s body: %v", mt, err)
	}
	ent.Text = strings.ToValidUTF8(string(body), "�")
	return ent, true
}

func (p *parser) multipart(e *message.Entity, mr message.MultipartReader, mt string, depth int, sec string) Entity {
	ent := Entity{Section: sec, MediaType: mt}
	for i := 1; ; i++ {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil && part == nil {
			if i == 1 {
				p.problem("%s: %v", mt, err)
				return Entity{Section: leafSection(sec), MediaType: "text/plain", Text: rawText(e)}
			}
			// The parts before the error are kept; a body that just
			// stops without a closing boundary lands here.
			if !strings.HasSuffix(err.Error(), "EOF") {
				p.problem("%s: %v", mt, err)
			}
			break
		}
		// err with a part: unknown charset or encoding of that part,
		// which is still readable.
		if c, ok := p.walk(part, depth+1, childSection(sec, i)); ok {
			ent.Children = append(ent.Children, c)
		}
	}
	return ent
}

// leafSection numbers a non-multipart entity: the body of a single-part
// message is "1".
func leafSection(sec string) string {
	if sec == "" {
		return "1"
	}
	return sec
}

// childSection numbers the i-th (1-based) part of the multipart sec.
func childSection(sec string, i int) string {
	if sec == "" {
		return strconv.Itoa(i)
	}
	return sec + "." + strconv.Itoa(i)
}

// texts collects the readable text of e for search and snippets. For
// multipart/alternative it picks one representation (plain over HTML);
// for every other multipart it concatenates the parts in order.
func (e Entity) texts() []textPart {
	if !strings.HasPrefix(e.MediaType, "multipart/") {
		if e.Attachment || e.MediaType != "text/plain" && e.MediaType != "text/html" {
			return nil
		}
		return []textPart{{text: e.Text, html: e.MediaType == "text/html"}}
	}
	if e.MediaType != "multipart/alternative" {
		var out []textPart
		for _, c := range e.Children {
			out = append(out, c.texts()...)
		}
		return out
	}
	var html []textPart
	for _, c := range e.Children {
		t := c.texts()
		if len(t) > 0 && !t[0].html {
			return t
		}
		if html == nil && len(t) > 0 {
			html = t
		}
	}
	return html
}

// rawText is the undecoded body of an entity whose structure could not be
// read, so its words still reach search.
func rawText(e *message.Entity) string {
	b, _ := io.ReadAll(io.LimitReader(e.Body, MaxBodyText))
	return strings.ToValidUTF8(string(b), "�")
}

// isAttachment decides has_attach. Inline images referenced by Content-ID
// are part of the HTML, not attachments; a forwarded or bounced message
// (message/rfc822) is.
func isAttachment(mt, disp string, named, hasCID bool) bool {
	switch {
	case disp == "attachment":
		return true
	case disp == "inline":
		return false
	case mt == "message/rfc822":
		return true
	case strings.HasPrefix(mt, "text/"), strings.HasPrefix(mt, "message/"):
		return false
	case hasCID:
		return false
	}
	return named || mt == "application/octet-stream" || mt == "application/pdf"
}

// sanitizeHeader drops header lines that are not fields (no colon, a bad
// field name, or a continuation with nothing to continue), keeping the
// body untouched.
func sanitizeHeader(raw []byte) []byte {
	var out bytes.Buffer
	r := bufio.NewReader(bytes.NewReader(raw))
	inField := false
	for {
		line, err := r.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			break
		}
		trimmed := bytes.TrimRight(line, "\r\n")
		if len(trimmed) == 0 {
			out.Write(line)
			rest, _ := io.ReadAll(r)
			out.Write(rest)
			break
		}
		switch {
		case line[0] == ' ' || line[0] == '\t':
			if inField {
				out.Write(line)
			}
		case validField(trimmed):
			inField = true
			out.Write(line)
		default:
			inField = false
		}
		if err != nil {
			break
		}
	}
	return out.Bytes()
}

func validField(line []byte) bool {
	name, _, ok := bytes.Cut(line, []byte(":"))
	name = bytes.TrimRight(name, " \t")
	if !ok || len(name) == 0 {
		return false
	}
	for _, c := range name {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

// fallback is the result when not even a cleaned header can be read: the
// whole raw message becomes the body so it is at least searchable.
func (p *parser) fallback(raw []byte) (Message, Entity) {
	p.m.References = []string{}
	p.m.BodyText = truncate(strings.TrimSpace(strings.ToValidUTF8(string(raw), "�")), MaxBodyText)
	p.m.Snippet = snippet(p.m.BodyText)
	p.m.Problem = strings.Join(p.problems, "; ")
	return p.m, Entity{Section: "1", MediaType: "text/plain", Text: p.m.BodyText}
}

// clean makes a header value safe to store and show: valid UTF-8, control
// characters and runs of whitespace collapsed to one space.
func clean(s string) string {
	s = strings.ToValidUTF8(s, "�")
	var b strings.Builder
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// snippet is the first snippetLen characters of the body, skipping quoted
// lines so a reply shows what is new in it.
func snippet(body string) string {
	var b strings.Builder
	for line := range strings.Lines(body) {
		if strings.HasPrefix(strings.TrimSpace(line), ">") {
			continue
		}
		b.WriteString(line)
		b.WriteByte(' ')
		if b.Len() > 4*snippetLen*utf8.UTFMax {
			break
		}
	}
	s := []rune(clean(b.String()))
	if len(s) > snippetLen {
		s = s[:snippetLen]
	}
	return string(s)
}

// truncate cuts s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
