package submit

import (
	"bytes"
	"fmt"
	"mime"
	"net/mail"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/transport"
)

// maxLine is the longest line RFC 5322 §2.1.1 allows, without its CRLF.
const maxLine = 998

// rawMessage is a submitted message as the §13.6 rules read it.
type rawMessage struct {
	// data has CRLF line endings and ends with one.
	data   []byte
	header mail.Header
	from   *mail.Address
	sender *mail.Address // nil without a Sender field
	// to and cc map the folded header addresses to the names the header
	// gives them: the kind of an envelope recipient (outbox_recipients.kind)
	// is where the header names it, and a draft the submission stops at
	// shows the name.
	to, cc    map[string]string
	subject   string
	inReplyTo string // the first id of In-Reply-To, without brackets
}

// readRaw reads a submission. Every way it can fail is the client's
// message breaking a rule, so the error wraps ErrRejected.
func readRaw(raw []byte) (*rawMessage, error) {
	data := crlf(raw)
	if err := checkData(data); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRejected, err)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: header: %w", ErrRejected, err)
	}
	m := &rawMessage{data: data, header: msg.Header}
	if n := len(m.header["From"]); n != 1 {
		return nil, fmt.Errorf("%w: want one From field, got %d", ErrRejected, n)
	}
	from, err := m.header.AddressList("From")
	if err != nil || len(from) != 1 {
		return nil, fmt.Errorf("%w: header From must hold exactly one address", ErrRejected)
	}
	m.from = from[0]
	switch n := len(m.header["Sender"]); {
	case n > 1:
		return nil, fmt.Errorf("%w: want at most one Sender field, got %d", ErrRejected, n)
	case n == 1:
		if m.sender, err = mail.ParseAddress(m.header.Get("Sender")); err != nil {
			return nil, fmt.Errorf("%w: header Sender must hold exactly one address", ErrRejected)
		}
	}
	// Recipient kinds only label the envelope; a header that does not
	// parse labels nobody, and those recipients count as Bcc.
	m.to, m.cc = folded(m.header, "To"), folded(m.header, "Cc")
	if s, err := new(mime.WordDecoder).DecodeHeader(m.header.Get("Subject")); err == nil {
		m.subject = s
	} else {
		m.subject = m.header.Get("Subject")
	}
	if irt := m.header.Get("In-Reply-To"); irt != "" {
		if i, j := strings.IndexByte(irt, '<'), strings.IndexByte(irt, '>'); i >= 0 && j > i+1 {
			m.inReplyTo = irt[i+1 : j]
		}
	}
	return m, nil
}

func folded(h mail.Header, name string) map[string]string {
	out := map[string]string{}
	list, _ := h.AddressList(name)
	for _, a := range list {
		f := store.FoldAddr(a.Address)
		if _, ok := out[f]; !ok {
			out[f] = shownName(a.Name)
		}
	}
	return out
}

// shownName is a header name as a draft may hold it: "" for one that
// is not one short line of text. It is only shown; the bytes sent are
// the submission's, name and all.
func shownName(n string) string {
	n = strings.TrimSpace(n)
	if !utf8.ValidString(n) || strings.ContainsFunc(n, unicode.IsControl) || utf8.RuneCountInString(n) > store.MaxRecipientName {
		return ""
	}
	return n
}

// crlf makes every line end in CRLF, the last one too. Submission
// clients and SMTP libraries differ in what they hand over; the stored
// copy and the exit both want CRLF (transport.Send refuses bare LF).
func crlf(raw []byte) []byte {
	b := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	b = bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n"))
	if !bytes.HasSuffix(b, []byte("\r\n")) {
		b = append(b, '\r', '\n')
	}
	return b
}

// checkData refuses what no exit takes and no repair would keep
// faithful: a bare CR, NUL, a line over 998 octets.
func checkData(b []byte) error {
	for n, line := range bytes.Split(bytes.TrimSuffix(b, []byte("\r\n")), []byte("\r\n")) {
		switch {
		case len(line) > maxLine:
			return fmt.Errorf("line %d: %d octets, limit %d", n+1, len(line), maxLine)
		case bytes.IndexByte(line, '\r') >= 0:
			return fmt.Errorf("line %d: bare CR", n+1)
		case bytes.IndexByte(line, 0) >= 0:
			return fmt.Errorf("line %d: NUL", n+1)
		}
	}
	return nil
}

// checkEnvelope applies the envelope rules of §13.6: MAIL FROM is the
// header From, and every recipient is a bare address.
func (m *rawMessage) checkEnvelope(mailFrom string, rcpts []string) error {
	if !strings.EqualFold(mailFrom, m.from.Address) {
		return fmt.Errorf("%w: MAIL FROM <%s> is not the header From <%s>", ErrRejected, mailFrom, m.from.Address)
	}
	if len(rcpts) == 0 {
		return fmt.Errorf("%w: no recipients", ErrRejected)
	}
	for _, r := range rcpts {
		at := strings.LastIndexByte(r, '@')
		if at <= 0 || at == len(r)-1 || strings.ContainsFunc(r, func(c rune) bool {
			return unicode.IsSpace(c) || unicode.IsControl(c) || c == '<' || c == '>'
		}) {
			return fmt.Errorf("%w: recipient %q is not a bare address", ErrRejected, r)
		}
	}
	return nil
}

// recipients labels the envelope recipients by the header field that
// names them: To, Cc, or neither (Bcc).
func (m *rawMessage) recipients(rcpts []string) []store.OutboxRecipient {
	out := make([]store.OutboxRecipient, 0, len(rcpts))
	for _, r := range rcpts {
		kind := store.RecipientBcc
		f := store.FoldAddr(r)
		if _, ok := m.to[f]; ok {
			kind = store.RecipientTo
		} else if _, ok := m.cc[f]; ok {
			kind = store.RecipientCc
		}
		out = append(out, store.OutboxRecipient{Addr: r, Kind: kind})
	}
	return out
}

// normalization says how to rewrite a submission.
type normalization struct {
	// From replaces the From field: the identity's display name and the
	// decided address, so a client cannot borrow someone's name for an
	// address it may use (§13.6). Sender, when not nil, replaces the
	// Sender field the same way.
	From   mail.Address
	Sender *mail.Address
	// AutoSubmitted is the decision's Auto-Submitted value. It is added
	// unless the message already says it is automatic in its own words
	// (any value but "no", which it replaces).
	AutoSubmitted string
	// Date is written when the message has none (RFC 6409 §8.2).
	Date time.Time
	// MessageID is written when the message has none and the exit
	// delivers ours (RFC 6409 §8.3); "" writes none.
	MessageID string
	Caps      transport.Caps
}

// normalize returns the bytes to store and send. It is idempotent: a
// held submission normalized again under the same decision is the same
// bytes, so confirming it sends what was stored.
func (m *rawMessage) normalize(n normalization) []byte {
	own := strings.ToLower(strings.TrimSpace(m.header.Get("Auto-Submitted")))
	addAuto := n.AutoSubmitted != "" && (own == "" || own == "no")
	var out bytes.Buffer
	prev := 0
	for _, f := range headerFields(m.data) {
		out.Write(m.data[prev:f.start])
		prev = f.end
		switch strings.ToLower(f.name) {
		case "bcc":
			// Bcc recipients are in the envelope; the header would show
			// them to everyone (RFC 5322 §3.6.3).
		case "from":
			out.WriteString("From: " + n.From.String() + "\r\n")
		case "sender":
			if n.Sender == nil {
				out.Write(m.data[f.start:f.end])
			} else {
				out.WriteString("Sender: " + n.Sender.String() + "\r\n")
			}
		case "auto-submitted":
			if !addAuto {
				out.Write(m.data[f.start:f.end])
			}
		default:
			out.Write(m.data[f.start:f.end])
		}
	}
	if m.header.Get("Date") == "" {
		out.WriteString("Date: " + n.Date.Format(time.RFC1123Z) + "\r\n")
	}
	if n.MessageID != "" && m.header.Get("Message-ID") == "" {
		out.WriteString("Message-ID: <" + n.MessageID + ">\r\n")
	}
	if addAuto {
		out.WriteString("Auto-Submitted: " + n.AutoSubmitted + "\r\n")
	}
	out.Write(m.data[prev:])
	return transport.StripHeaders(out.Bytes(), n.Caps)
}

// field is one header field: its name and the span of all its lines.
type field struct {
	name       string
	start, end int
}

// headerFields splits the header section of b (CRLF lines) into fields;
// the last one ends where the empty line, or b, begins.
func headerFields(b []byte) []field {
	var fs []field
	for off := 0; off < len(b); {
		end := bytes.Index(b[off:], []byte("\r\n"))
		if end < 0 {
			end = len(b)
		} else {
			end += off + 2
		}
		line := b[off:end]
		if len(bytes.TrimRight(line, "\r\n")) == 0 {
			break
		}
		if (line[0] == ' ' || line[0] == '\t') && len(fs) > 0 {
			fs[len(fs)-1].end = end
		} else {
			name, _, _ := bytes.Cut(line, []byte(":"))
			fs = append(fs, field{name: string(bytes.TrimRight(name, " \t")), start: off, end: end})
		}
		off = end
	}
	return fs
}

func domainOf(addr string) string {
	return addr[strings.LastIndexByte(addr, '@')+1:]
}
