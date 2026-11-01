package imap

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// Bounds on what one server response may make the client hold. Literals
// have their own, caller-given bound; these cover everything else.
const (
	// maxLine bounds the bytes of a response outside its literals. Real
	// responses (a LIST entry, a FETCH of flags and labels) are a few KiB
	// at most; the bound only has to keep a hostile server from making the
	// client buffer without end.
	maxLine = 256 << 10
	// maxSearchLine bounds a SEARCH response, whose numbers are kept as
	// uint32s rather than Values. 32 MiB is about four million seven-digit
	// UIDs.
	maxSearchLine = 32 << 20
	// maxDepth bounds parenthesis nesting.
	maxDepth = 64
	// smallLiteral bounds the literals of responses that carry no message
	// content: a mailbox name or an ID value sent as a literal.
	smallLiteral = 64 << 10
)

// ValueType is the syntactic kind of a Value.
type ValueType uint8

const (
	// AtomValue is a bare token: a number, a flag, a keyword.
	AtomValue ValueType = iota + 1
	// StringValue is a quoted string or a literal.
	StringValue
	// NilValue is NIL.
	NilValue
	// ListValue is a parenthesized list.
	ListValue
)

// Value is one data item of a server response, as the grammar delivers
// it. The client interprets the items it knows through typed accessors
// and leaves the rest to the caller.
type Value struct {
	Type ValueType
	// Atom is the token of an AtomValue, verbatim.
	Atom string
	// Bytes holds a StringValue: the unescaped quoted string or the
	// literal's octets.
	Bytes []byte
	// List holds the elements of a ListValue.
	List []Value
}

// Text returns an atom or a string as a string.
func (v Value) Text() (string, bool) {
	switch v.Type {
	case AtomValue:
		return v.Atom, true
	case StringValue:
		return string(v.Bytes), true
	}
	return "", false
}

// Number returns an atom made only of decimal digits.
func (v Value) Number() (uint64, bool) {
	if v.Type != AtomValue || v.Atom == "" || len(v.Atom) > 20 {
		return 0, false
	}
	for i := 0; i < len(v.Atom); i++ {
		if v.Atom[i] < '0' || v.Atom[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(v.Atom, 10, 64)
	return n, err == nil
}

func (v Value) number32() (uint32, bool) {
	n, ok := v.Number()
	if !ok || n > 1<<32-1 {
		return 0, false
	}
	return uint32(n), true
}

// StatusResponse is a status response: OK, NO, BAD, BYE or PREAUTH, with
// its optional response code (RFC 3501 §7.1).
type StatusResponse struct {
	// Type is OK, NO, BAD, BYE or PREAUTH.
	Type string
	// Code is the response code's name in upper case, such as
	// AUTHENTICATIONFAILED or UIDVALIDITY, or "" when there is none.
	Code string
	// Arg is the rest of the response code, verbatim.
	Arg string
	// Text is the human-readable text. It comes from the server and is
	// for logs only.
	Text string
}

func (s *StatusResponse) String() string {
	out := s.Type
	if s.Code != "" {
		out += " [" + s.Code
		if s.Arg != "" {
			out += " " + s.Arg
		}
		out += "]"
	}
	if s.Text != "" {
		out += " " + strconv.Quote(s.Text)
	}
	return out
}

// response is one server response line, literals included.
type response struct {
	// tag is "*" for untagged data, "+" for a continuation request, or the
	// tag of the command it completes.
	tag string
	// status is set for status responses.
	status *StatusResponse
	// num is the number in front of numeric responses ("* 3 EXISTS").
	num    uint32
	hasNum bool
	// name is the upper-cased response name: EXISTS, FETCH, LIST, ...
	name string
	// fields are the data items after the name.
	fields []Value
	// uids holds the numbers of a SEARCH response.
	uids []uint32
	// text is the text of a continuation request.
	text string
}

// errSyntax marks a response that breaks the grammar.
type errSyntax struct{ msg string }

func (e *errSyntax) Error() string { return "malformed response: " + e.msg }

func syntaxf(format string, args ...any) error {
	return &errSyntax{msg: fmt.Sprintf(format, args...)}
}

// errTooLarge marks a response over one of the bounds.
type errTooLarge struct{ msg string }

func (e *errTooLarge) Error() string { return e.msg }

// reader parses server responses from a buffered stream.
type reader struct {
	r *bufio.Reader
	// line is the budget of non-literal bytes left for this response.
	line int
	// lit is the budget of literal bytes left for this response.
	lit int64
}

func newReader(r io.Reader) *reader {
	return &reader{r: bufio.NewReader(r)}
}

// readResponse reads one complete response. Its literals may add up to at
// most maxLit bytes; a literal over the remaining budget fails before any
// of it is read, leaving the stream unusable.
func (rd *reader) readResponse(maxLit int64) (*response, error) {
	rd.line, rd.lit = maxLine, maxLit
	tag, err := rd.atom()
	if err != nil {
		return nil, err
	}
	if tag == "" {
		return nil, syntaxf("no tag")
	}
	resp := &response{tag: tag}
	if tag == "+" {
		// "+" SP text, but a bare "+" is common enough to accept.
		if ok, err := rd.skip(' '); err != nil {
			return nil, err
		} else if !ok {
			return resp, rd.crlf()
		}
		resp.text, err = rd.text()
		return resp, err
	}
	if err := rd.want(' '); err != nil {
		return nil, err
	}

	c, err := rd.peek()
	if err != nil {
		return nil, err
	}
	if tag == "*" && c >= '0' && c <= '9' {
		v, err := rd.atom()
		if err != nil {
			return nil, err
		}
		n, ok := Value{Type: AtomValue, Atom: v}.number32()
		if !ok {
			return nil, syntaxf("bad number %q", v)
		}
		resp.num, resp.hasNum = n, true
		if err := rd.want(' '); err != nil {
			return nil, err
		}
	}
	name, err := rd.atom()
	if err != nil {
		return nil, err
	}
	resp.name = asciiUpper(name)
	switch {
	case !resp.hasNum && isStatus(resp.name):
		resp.status, err = rd.status(resp.name)
		return resp, err
	case tag != "*":
		return nil, syntaxf("tagged response %q is not a status response", resp.name)
	case resp.name == "SEARCH" && !resp.hasNum:
		resp.uids, err = rd.search()
		return resp, err
	}
	resp.fields, err = rd.values(0, 0)
	return resp, err
}

func isStatus(name string) bool {
	switch name {
	case "OK", "NO", "BAD", "BYE", "PREAUTH":
		return true
	}
	return false
}

// status reads the rest of a status response: [SP "[" code [SP arg] "]"]
// [SP text] CRLF. Servers omit the SP before an empty text, so both are
// optional.
func (rd *reader) status(typ string) (*StatusResponse, error) {
	s := &StatusResponse{Type: typ}
	ok, err := rd.skip(' ')
	if err != nil || !ok {
		return s, firstErr(err, rd.crlf())
	}
	c, err := rd.peek()
	if err != nil {
		return nil, err
	}
	if c == '[' {
		_, _ = rd.next()
		code, err := rd.until(func(c byte) bool { return c == ' ' || c == ']' })
		if err != nil {
			return nil, err
		}
		if code == "" {
			return nil, syntaxf("empty response code")
		}
		s.Code = asciiUpper(code)
		if ok, err := rd.skip(' '); err != nil {
			return nil, err
		} else if ok {
			if s.Arg, err = rd.until(func(c byte) bool { return c == ']' }); err != nil {
				return nil, err
			}
		}
		if err := rd.want(']'); err != nil {
			return nil, err
		}
		if ok, err := rd.skip(' '); err != nil || !ok {
			return s, firstErr(err, rd.crlf())
		}
	}
	s.Text, err = rd.text()
	return s, err
}

// search reads the numbers of a SEARCH response. A trailing "(MODSEQ n)"
// (RFC 7162) is read and dropped.
func (rd *reader) search() ([]uint32, error) {
	rd.line = maxSearchLine
	var uids []uint32
	for {
		c, err := rd.peek()
		if err != nil {
			return nil, err
		}
		switch c {
		case '\r':
			return uids, rd.crlf()
		case ' ':
			_, _ = rd.next()
		case '(':
			if _, err := rd.value(0); err != nil {
				return nil, err
			}
		default:
			a, err := rd.atom()
			if err != nil {
				return nil, err
			}
			n, ok := Value{Type: AtomValue, Atom: a}.number32()
			if !ok {
				return nil, syntaxf("bad number %q in SEARCH", a)
			}
			uids = append(uids, n)
		}
	}
}

// values reads values separated by spaces up to the closing parenthesis
// (end ')') or the end of the line (end 0).
func (rd *reader) values(end byte, depth int) ([]Value, error) {
	var vs []Value
	for {
		c, err := rd.peek()
		if err != nil {
			return nil, err
		}
		switch {
		case c == ' ':
			_, _ = rd.next()
		case c == '\r' && end == 0:
			return vs, rd.crlf()
		case c == ')' && end == ')':
			_, _ = rd.next()
			return vs, nil
		default:
			v, err := rd.value(depth)
			if err != nil {
				return nil, err
			}
			vs = append(vs, v)
		}
	}
}

func (rd *reader) value(depth int) (Value, error) {
	c, err := rd.peek()
	if err != nil {
		return Value{}, err
	}
	switch c {
	case '(':
		if depth >= maxDepth {
			return Value{}, &errTooLarge{msg: "lists nested too deeply"}
		}
		_, _ = rd.next()
		l, err := rd.values(')', depth+1)
		return Value{Type: ListValue, List: l}, err
	case '"':
		b, err := rd.quoted()
		return Value{Type: StringValue, Bytes: b}, err
	case '{':
		b, err := rd.literal()
		return Value{Type: StringValue, Bytes: b}, err
	case '~':
		// literal8 (RFC 3516); "~" followed by anything else is an atom.
		if p, err := rd.r.Peek(2); err == nil && p[1] == '{' {
			_, _ = rd.next()
			b, err := rd.literal()
			return Value{Type: StringValue, Bytes: b}, err
		}
	}
	a, err := rd.atom()
	if err != nil {
		return Value{}, err
	}
	if a == "" {
		return Value{}, syntaxf("unexpected %q", c)
	}
	if len(a) == 3 && asciiUpper(a) == "NIL" {
		return Value{Type: NilValue}, nil
	}
	return Value{Type: AtomValue, Atom: a}, nil
}

// atom reads a bare token. It is laxer than RFC 3501's ATOM-CHAR because
// servers send flags (\Seen), section specifiers (BODY[HEADER.FIELDS
// (FROM)]<0>) and 8-bit names in this position: it stops at a space, a
// parenthesis, a quote or the end of the line, except that a bracket runs
// to its closing bracket, spaces and parentheses included.
func (rd *reader) atom() (string, error) {
	var b []byte
	for {
		c, err := rd.peek()
		if err != nil {
			return "", err
		}
		if c == ' ' || c == '(' || c == ')' || c == '"' || c < ' ' || c == 0x7f {
			return string(b), nil
		}
		if c == '{' && len(b) == 0 {
			return "", nil
		}
		_, _ = rd.next()
		b = append(b, c)
		if c == '[' {
			rest, err := rd.until(func(c byte) bool { return c == ']' })
			if err != nil {
				return "", err
			}
			if err := rd.want(']'); err != nil {
				return "", err
			}
			b = append(append(b, rest...), ']')
		}
	}
}

// quoted reads a quoted string and returns its unescaped contents.
func (rd *reader) quoted() ([]byte, error) {
	if err := rd.want('"'); err != nil {
		return nil, err
	}
	var b []byte
	for {
		c, err := rd.next()
		if err != nil {
			return nil, err
		}
		switch c {
		case '"':
			return b, nil
		case '\\':
			if c, err = rd.next(); err != nil {
				return nil, err
			}
		}
		if c == '\r' || c == '\n' {
			return nil, syntaxf("line break in a quoted string")
		}
		b = append(b, c)
	}
}

// literal reads "{" number "}" CRLF and then the octets, after checking
// the announced size against the remaining budget.
func (rd *reader) literal() ([]byte, error) {
	if err := rd.want('{'); err != nil {
		return nil, err
	}
	digits, err := rd.until(func(c byte) bool { return c < '0' || c > '9' })
	if err != nil {
		return nil, err
	}
	if digits == "" {
		return nil, syntaxf("literal without a size")
	}
	if err := rd.want('}'); err != nil {
		return nil, err
	}
	if err := rd.crlf(); err != nil {
		return nil, err
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n > rd.lit {
		return nil, &errTooLarge{msg: fmt.Sprintf("literal of %s octets over the limit of %d", digits, rd.lit)}
	}
	rd.lit -= n
	b := make([]byte, n)
	if _, err := io.ReadFull(rd.r, b); err != nil {
		return nil, noEOF(err)
	}
	return b, nil
}

// text reads the rest of the line as text.
func (rd *reader) text() (string, error) {
	s, err := rd.until(func(c byte) bool { return c == '\r' || c == '\n' })
	if err != nil {
		return "", err
	}
	return s, rd.crlf()
}

// until reads bytes up to, not including, the first one for which stop
// is true. A line break always stops it, so that it never crosses a line.
func (rd *reader) until(stop func(byte) bool) (string, error) {
	var b []byte
	for {
		c, err := rd.peek()
		if err != nil {
			return "", err
		}
		if stop(c) || c == '\r' || c == '\n' {
			return string(b), nil
		}
		_, _ = rd.next()
		b = append(b, c)
	}
}

func (rd *reader) crlf() error {
	if err := rd.want('\r'); err != nil {
		return err
	}
	return rd.want('\n')
}

func (rd *reader) want(c byte) error {
	got, err := rd.next()
	if err != nil {
		return err
	}
	if got != c {
		return syntaxf("want %q, got %q", c, got)
	}
	return nil
}

// skip consumes c if it is the next byte.
func (rd *reader) skip(c byte) (bool, error) {
	got, err := rd.peek()
	if err != nil || got != c {
		return false, err
	}
	_, _ = rd.next()
	return true, nil
}

// peek returns the next byte without consuming it. It enforces the line
// budget, so that a next after a successful peek cannot fail and callers
// may ignore its error.
func (rd *reader) peek() (byte, error) {
	if rd.line <= 0 {
		return 0, &errTooLarge{msg: "response line too long"}
	}
	p, err := rd.r.Peek(1)
	if err != nil {
		return 0, noEOF(err)
	}
	return p[0], nil
}

// next consumes one byte of the line budget.
func (rd *reader) next() (byte, error) {
	if rd.line <= 0 {
		return 0, &errTooLarge{msg: "response line too long"}
	}
	c, err := rd.r.ReadByte()
	if err != nil {
		return 0, noEOF(err)
	}
	rd.line--
	return c, nil
}

// noEOF turns a clean EOF in the middle of a response into
// io.ErrUnexpectedEOF; a response ends at its CRLF, never at EOF.
func noEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// asciiUpper upper-cases ASCII letters only, so 8-bit bytes in a name
// stay as they are.
func asciiUpper(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'a' && c <= 'z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'a' && b[j] <= 'z' {
					b[j] -= 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}
