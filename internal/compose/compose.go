// Package compose assembles outgoing mail: it turns a draft written in
// Markdown into the complete RFC 5322 bytes a transport delivers (§10.2).
//
// Compose is a pure function of its input. There is no clock and no
// randomness inside: the Date and Message-ID are inputs and multipart
// boundaries are derived from the parts they separate. The same draft
// therefore always yields the same bytes, so a retry resends exactly what
// was committed, and tests compare against golden files directly.
package compose

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Address is a mailbox: Addr is the bare addr-spec, Name the optional
// display name.
type Address struct {
	Name string
	Addr string
}

// Attachment is a file carried by the message.
type Attachment struct {
	Filename string
	// ContentType is a MIME type such as "image/png"; empty means
	// application/octet-stream.
	ContentType string
	// ContentID, when set, makes this an inline part of the HTML body,
	// referenced from the Markdown as ![alt](cid:<ContentID>). Without it
	// the part is a regular attachment.
	ContentID string
	Data      []byte
}

// Parent is the message being replied to, as its headers say (mimeparse
// fields, ids without angle brackets). It came from the outside, so ids
// that are not well formed are dropped rather than written.
type Parent struct {
	MessageID  string
	InReplyTo  string
	References []string
}

// Message describes one outgoing message.
type Message struct {
	From    Address
	ReplyTo []Address
	To      []Address
	Cc      []Address
	// Bcc never appears in the header. It only joins Recipients, which
	// the caller hands to the transport as the envelope.
	Bcc     []Address
	Subject string
	Date    time.Time
	// MessageID is written as the Message-ID header, without angle
	// brackets (NewMessageID makes one). Leave it empty for a transport
	// that sets its own (CallerMessageID=false, §10.4.1): then no
	// Message-ID is written at all.
	MessageID string
	// Parent makes this a reply: In-Reply-To and References follow from
	// it (RFC 5322 §3.6.4). Nil starts a new thread.
	Parent *Parent
	// AutoSubmitted is the RFC 3834 Auto-Submitted value (§8.9 邮件循环),
	// such as "auto-generated"; empty writes no header.
	AutoSubmitted string
	// Body is Markdown. It is the text/plain part verbatim and is rendered
	// into the text/html part (§10.2.1).
	Body string
	// StripMarkup renders the text/plain part from the Markdown with the
	// markup removed instead of using the source (the per-identity
	// option of §10.2.1).
	StripMarkup bool
	// Signature is the identity's signature, Markdown like the body. It
	// goes below the RFC 3676 §4.3 delimiter "-- " in the plain part and
	// into a muted block in the HTML part. A delimiter line it already
	// starts with is dropped rather than written twice.
	Signature   string
	Attachments []Attachment
}

const (
	// maxIDLen bounds a msg-id we write. RFC 5322 sets none; this keeps
	// one id well inside a header line and rejects junk from hostile mail.
	maxIDLen = 250
	// maxReferences caps the References chain of a reply. Mail threading
	// (RFC 5256 REFERENCES, JWZ) needs the root and the most recent ids;
	// RFC 5537 §3.4.4 trims the middle of the chain the same way.
	maxReferences = 20
	// maxFilename is in bytes, the common filesystem limit; it also keeps
	// the RFC 2231 encoded parameter, which cannot be folded, on one line
	// under 998 octets.
	maxFilename = 255
	// dateLayout is RFC 5322 §3.3 without the optional comment.
	dateLayout = "Mon, 02 Jan 2006 15:04:05 -0700"
)

// NewMessageID returns a Message-ID for a message sent at t from domain,
// without angle brackets: ULID@domain (§11.2). The local part carries no
// project name.
func NewMessageID(t time.Time, domain string) string {
	return ulid.New(t) + "@" + domain
}

// Recipients returns the envelope recipients: To, Cc and Bcc, each address
// once (compared case-insensitively), in that order.
func (m *Message) Recipients() []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range [][]Address{m.To, m.Cc, m.Bcc} {
		for _, a := range l {
			k := strings.ToLower(a.Addr)
			if !seen[k] {
				seen[k] = true
				out = append(out, a.Addr)
			}
		}
	}
	return out
}

// Compose returns the complete message with CRLF line endings.
//
// The MIME tree is that of §10.2.1 with empty levels left out:
// multipart/related only when there are inline parts, multipart/mixed
// only when there are attachments. A level holding a single child carries
// no information and some clients render it as an empty attachment.
func Compose(m *Message) ([]byte, error) {
	if err := m.validate(); err != nil {
		return nil, err
	}
	plain, html, err := render(m.Body, m.Signature, m.StripMarkup)
	if err != nil {
		return nil, err
	}

	var b builder
	root := b.multipart("alternative", nil,
		b.text("text/plain", plain),
		b.text("text/html", html),
	)
	var inline, attached []entity
	for _, a := range m.Attachments {
		e := b.file(a)
		if a.ContentID != "" {
			inline = append(inline, e)
		} else {
			attached = append(attached, e)
		}
	}
	if len(inline) > 0 {
		// RFC 2387 requires the type parameter: the media type of the root.
		root = b.multipart("related", map[string]string{"type": "multipart/alternative"},
			append([]entity{root}, inline...)...)
	}
	if len(attached) > 0 {
		root = b.multipart("mixed", nil, append([]entity{root}, attached...)...)
	}
	root.header = append(m.header(), append([]field{{"MIME-Version", "1.0"}}, root.header...)...)
	out := b.bytes(root)
	if b.err != nil {
		return nil, b.err
	}
	return out, nil
}

func (m *Message) validate() error {
	if err := checkAddr(m.From.Addr); err != nil {
		return fmt.Errorf("from: %w", err)
	}
	texts := []string{m.Subject, m.Body, m.Signature, m.From.Name}
	for _, l := range []struct {
		name  string
		addrs []Address
	}{{"reply-to", m.ReplyTo}, {"to", m.To}, {"cc", m.Cc}, {"bcc", m.Bcc}} {
		for _, a := range l.addrs {
			if err := checkAddr(a.Addr); err != nil {
				return fmt.Errorf("%s: %w", l.name, err)
			}
			texts = append(texts, a.Name)
		}
	}
	for _, s := range texts {
		if !utf8.ValidString(s) {
			return errors.New("text is not valid UTF-8")
		}
	}
	if len(m.Recipients()) == 0 {
		return errors.New("no recipients")
	}
	if m.Date.IsZero() {
		return errors.New("no date")
	}
	if m.MessageID != "" && !validID(m.MessageID) {
		return fmt.Errorf("message id %q is not a valid msg-id", m.MessageID)
	}
	if m.AutoSubmitted != "" && !isToken(m.AutoSubmitted) {
		return fmt.Errorf("auto-submitted %q is not a token", m.AutoSubmitted)
	}
	for _, a := range m.Attachments {
		if a.Filename == "" || len(a.Filename) > maxFilename || !utf8.ValidString(a.Filename) {
			return fmt.Errorf("attachment filename %q: empty, longer than %d bytes or not UTF-8", a.Filename, maxFilename)
		}
		if a.ContentID != "" && !validID(a.ContentID) {
			return fmt.Errorf("attachment %q: content id %q is not a valid msg-id", a.Filename, a.ContentID)
		}
		if a.ContentType != "" {
			mt, _, err := mime.ParseMediaType(a.ContentType)
			if err != nil {
				return fmt.Errorf("attachment %q: content type: %w", a.Filename, err)
			}
			// A multipart or message/* body would have to be nested MIME,
			// not base64 (RFC 2046 §5.2.1); forwarding as an attachment is
			// not supported yet.
			if strings.HasPrefix(mt, "multipart/") || strings.HasPrefix(mt, "message/") {
				return fmt.Errorf("attachment %q: content type %s is not supported", a.Filename, mt)
			}
		}
	}
	return nil
}

// checkAddr accepts an ASCII addr-spec. Non-ASCII local parts need
// SMTPUTF8, which no transport negotiates yet; IDN domains are expected
// in their A-label form.
func checkAddr(addr string) error {
	for i := range len(addr) {
		if addr[i] >= utf8.RuneSelf {
			return fmt.Errorf("address %q is not ASCII", addr)
		}
	}
	if len(addr) > 254 {
		return fmt.Errorf("address %q is longer than 254 octets", addr)
	}
	a, err := mail.ParseAddress("<" + addr + ">")
	if err != nil || a.Address != addr {
		return fmt.Errorf("address %q is not an addr-spec", addr)
	}
	return nil
}

// validID accepts what we are willing to write between angle brackets.
// It is looser than RFC 5322's msg-id because real mail is, but nothing
// in it can break out of the header field.
func validID(id string) bool {
	if id == "" || len(id) > maxIDLen {
		return false
	}
	for i := range len(id) {
		if c := id[i]; c <= ' ' || c >= 0x7f || c == '<' || c == '>' {
			return false
		}
	}
	return true
}

func isToken(s string) bool {
	for i := range len(s) {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

// header returns the message header fields in the order they are written.
// There is deliberately no X-Mailer or any field naming the software
// (§11.2).
func (m *Message) header() []field {
	fs := []field{
		{"Date", m.Date.Format(dateLayout)},
		{"From", formatAddrs([]Address{m.From})},
	}
	if len(m.ReplyTo) > 0 {
		fs = append(fs, field{"Reply-To", formatAddrs(m.ReplyTo)})
	}
	if len(m.To) > 0 {
		fs = append(fs, field{"To", formatAddrs(m.To)})
	}
	if len(m.Cc) > 0 {
		fs = append(fs, field{"Cc", formatAddrs(m.Cc)})
	}
	fs = append(fs, field{"Subject", encodeText(oneLine(m.Subject))})
	if m.MessageID != "" {
		fs = append(fs, field{"Message-ID", "<" + m.MessageID + ">"})
	}
	if parent, refs := thread(m.Parent); parent != "" {
		fs = append(fs,
			field{"In-Reply-To", "<" + parent + ">"},
			field{"References", "<" + strings.Join(refs, "> <") + ">"},
		)
	}
	if m.AutoSubmitted != "" {
		fs = append(fs, field{"Auto-Submitted", m.AutoSubmitted})
	}
	return fs
}

// thread returns the In-Reply-To id and the References chain of a reply
// to p (RFC 5322 §3.6.4): the parent's References, or failing that its
// single In-Reply-To, followed by the parent's own id. Malformed ids are
// dropped, duplicates kept once, and a long chain loses its middle.
func thread(p *Parent) (string, []string) {
	if p == nil || !validID(p.MessageID) {
		return "", nil
	}
	chain := p.References
	if len(chain) == 0 && p.InReplyTo != "" {
		chain = []string{p.InReplyTo}
	}
	seen := map[string]bool{p.MessageID: true}
	var refs []string
	for _, id := range chain {
		if validID(id) && !seen[id] {
			seen[id] = true
			refs = append(refs, id)
		}
	}
	refs = append(refs, p.MessageID)
	if n := len(refs); n > maxReferences {
		refs = append(refs[:1], refs[n-(maxReferences-1):]...)
	}
	return p.MessageID, refs
}

func formatAddrs(l []Address) string {
	s := make([]string, len(l))
	for i, a := range l {
		// net/mail quotes an ASCII name and RFC 2047-encodes any other.
		s[i] = (&mail.Address{Name: oneLine(a.Name), Address: a.Addr}).String()
	}
	return strings.Join(s, ", ")
}

// oneLine replaces control characters, line breaks included, with spaces:
// header text comes from drafts that agents may have written under the
// influence of inbound mail (§8.3), and must not start a new field.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

// signature returns sig without a leading delimiter line ("-- ", or "--"
// as people type it) and without the blank lines around it, so the
// delimiter render writes is the only one. Empty means no signature.
func signature(sig string) string {
	lines := strings.Split(strings.TrimRight(normalize(sig), "\n"), "\n")
	blank := func() bool { return len(lines) > 0 && strings.TrimSpace(lines[0]) == "" }
	for blank() {
		lines = lines[1:]
	}
	if len(lines) > 0 && strings.TrimRight(lines[0], " \t") == "--" {
		lines = lines[1:]
	}
	for blank() {
		lines = lines[1:]
	}
	return normalize(strings.Join(lines, "\n"))
}

// normalize gives the body LF line endings and a final newline; the
// encoders write them as CRLF.
func normalize(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s
}

type field struct{ k, v string }

// entity is a MIME entity whose body is already transfer-encoded.
type entity struct {
	header []field
	body   []byte
}

// builder assembles entities, keeping the first error.
type builder struct{ err error }

func (b *builder) fail(err error) {
	if b.err == nil {
		b.err = err
	}
}

// text is a UTF-8 text part in whichever of quoted-printable and base64
// is shorter: quoted-printable keeps mostly-ASCII text readable in the raw
// message (the Sent copy), base64 keeps CJK text from tripling in size.
// Both keep every line under 77 octets whatever the Markdown holds.
func (b *builder) text(mediaType, s string) entity {
	var qp bytes.Buffer
	w := quotedprintable.NewWriter(&qp)
	_, _ = w.Write([]byte(s))
	_ = w.Close()
	cte, body := "quoted-printable", qp.Bytes()
	if b64 := base64Lines([]byte(strings.ReplaceAll(s, "\n", "\r\n"))); len(b64) < len(body) {
		cte, body = "base64", b64
	}
	return entity{
		header: []field{
			{"Content-Type", mime.FormatMediaType(mediaType, map[string]string{"charset": "utf-8"})},
			{"Content-Transfer-Encoding", cte},
		},
		body: body,
	}
}

func base64Lines(data []byte) []byte {
	enc := base64.StdEncoding.EncodeToString(data)
	var out bytes.Buffer
	for len(enc) > 76 {
		out.WriteString(enc[:76])
		out.WriteString("\r\n")
		enc = enc[76:]
	}
	out.WriteString(enc)
	return out.Bytes()
}

// encodeText returns s as RFC 2047 encoded words where it needs them, Q or
// B by the same shorter-wins rule as text parts.
func encodeText(s string) string {
	q := mime.QEncoding.Encode("utf-8", s)
	if b := mime.BEncoding.Encode("utf-8", s); len(b) < len(q) {
		return b
	}
	return q
}

// file is an attachment or inline part, always base64 so its bytes come
// out exactly as they went in. The filename goes in both places clients
// look: Content-Disposition in RFC 2231 form (the standard), and the
// Content-Type name parameter RFC 2047-encoded (what older clients read).
func (b *builder) file(a Attachment) entity {
	name := oneLine(a.Filename)
	ct := a.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	mt, params, _ := mime.ParseMediaType(ct) // validated
	params["name"] = encodeText(name)
	disp := "attachment"
	if a.ContentID != "" {
		disp = "inline"
	}
	ctype := mime.FormatMediaType(mt, params)
	cdisp := mime.FormatMediaType(disp, map[string]string{"filename": name})
	if ctype == "" || cdisp == "" {
		b.fail(fmt.Errorf("attachment %q: cannot format its header", a.Filename))
	}
	h := []field{{"Content-Type", ctype}, {"Content-Disposition", cdisp}}
	if a.ContentID != "" {
		h = append(h, field{"Content-ID", "<" + a.ContentID + ">"})
	}
	h = append(h, field{"Content-Transfer-Encoding", "base64"})
	return entity{header: h, body: base64Lines(a.Data)}
}

// multipart joins parts under a multipart/<subtype> entity.
//
// The boundary is "=_" followed by a hash of the parts. "=_" cannot occur
// in quoted-printable or base64 output, so the boundary cannot collide
// with any body we write; the hash only has to tell nested levels apart.
// Deriving it from the content instead of drawing it at random keeps
// Compose deterministic; the parts are what every recipient sees anyway,
// so the hash discloses nothing (Bcc is never part of it).
func (b *builder) multipart(subtype string, params map[string]string, parts ...entity) entity {
	raw := make([][]byte, len(parts))
	h := sha256.New()
	for i, p := range parts {
		raw[i] = b.bytes(p)
		_, _ = h.Write(raw[i])
	}
	boundary := "=_" + hex.EncodeToString(h.Sum(nil))[:24]

	var body bytes.Buffer
	for _, r := range raw {
		body.WriteString("--" + boundary + "\r\n")
		body.Write(r)
		body.WriteString("\r\n")
	}
	body.WriteString("--" + boundary + "--\r\n")

	if params == nil {
		params = map[string]string{}
	}
	params["boundary"] = boundary
	return entity{
		header: []field{{"Content-Type", mime.FormatMediaType("multipart/"+subtype, params)}},
		body:   body.Bytes(),
	}
}

// bytes serializes e: header, blank line, body.
func (b *builder) bytes(e entity) []byte {
	var out bytes.Buffer
	for _, f := range e.header {
		out.Write(b.field(f))
	}
	out.WriteString("\r\n")
	out.Write(e.body)
	return out.Bytes()
}

// field formats one header field, folded before a space wherever a line
// would pass 76 octets (RFC 5322 §2.2.3). Every word we write is bounded
// (encoded words, ids, addresses, filenames), so a line past the hard
// limit of 998 octets means a bug or an absurd input, and fails.
func (b *builder) field(f field) []byte {
	if strings.ContainsAny(f.v, "\r\n") {
		b.fail(fmt.Errorf("header %s: line break in value", f.k))
		return nil
	}
	var out bytes.Buffer
	out.WriteString(f.k + ":")
	if f.v == "" {
		out.WriteString("\r\n")
		return out.Bytes()
	}
	n := len(f.k) + 1
	for i, w := range strings.Split(f.v, " ") {
		if i > 0 && w != "" && n+1+len(w) > 76 {
			out.WriteString("\r\n")
			n = 0
		}
		out.WriteString(" " + w)
		n += 1 + len(w)
		if n > 998 {
			b.fail(fmt.Errorf("header %s: line longer than 998 octets", f.k))
			return nil
		}
	}
	out.WriteString("\r\n")
	return out.Bytes()
}
