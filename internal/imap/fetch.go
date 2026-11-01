package imap

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
)

// Message is the data of one FETCH response.
type Message struct {
	// Seq is the message sequence number.
	Seq uint32
	// Attrs maps each data item name, in upper case and as the server
	// wrote it ("UID", "BODY[]", "BODY[HEADER]", "X-GM-LABELS"), to its
	// value. A BODY.PEEK[...] request comes back as BODY[...]. Items the
	// client does not know are kept here too.
	Attrs map[string]Value
}

// UID returns the message's UID. UIDFetch delivers only messages that
// have one.
func (m *Message) UID() (uint32, bool) {
	return m.Attrs["UID"].number32()
}

// Size returns RFC822.SIZE, the size of the message in octets.
func (m *Message) Size() (int64, bool) {
	n, ok := m.Attrs["RFC822.SIZE"].Number()
	if !ok || n > 1<<63-1 {
		return 0, false
	}
	return int64(n), true
}

// internalDate is RFC 3501's date-time, whose day may be space-padded.
const internalDate = "_2-Jan-2006 15:04:05 -0700"

// InternalDate returns INTERNALDATE: when the server says the message
// arrived. It is the server's claim, not a fact.
func (m *Message) InternalDate() (time.Time, bool) {
	v := m.Attrs["INTERNALDATE"]
	if v.Type != StringValue {
		return time.Time{}, false
	}
	t, err := time.Parse(internalDate, string(v.Bytes))
	return t, err == nil
}

// Flags returns FLAGS as sent: \Seen, \Draft, $Forwarded, ... Flags are
// case-insensitive (RFC 3501 §2.3.2) and servers differ in case, so
// compare them with strings.EqualFold.
func (m *Message) Flags() ([]string, bool) {
	return textList(m.Attrs["FLAGS"])
}

// ModSeq returns MODSEQ (RFC 7162).
func (m *Message) ModSeq() (uint64, bool) {
	v := m.Attrs["MODSEQ"]
	if v.Type != ListValue || len(v.List) != 1 {
		return 0, false
	}
	return v.List[0].Number()
}

// GmailMsgID returns X-GM-MSGID, Gmail's id of the message, which is the
// same in every label and survives UIDVALIDITY changes.
func (m *Message) GmailMsgID() (uint64, bool) {
	return m.Attrs["X-GM-MSGID"].Number()
}

// GmailLabels returns X-GM-LABELS. System labels keep their backslash
// (\Inbox, \Sent, \Important); user labels are decoded from modified
// UTF-7 where they are valid modified UTF-7.
func (m *Message) GmailLabels() ([]string, bool) {
	l, ok := textList(m.Attrs["X-GM-LABELS"])
	if !ok {
		return nil, false
	}
	for i, s := range l {
		if d, err := DecodeMailbox(s); err == nil {
			l[i] = d
		}
	}
	return l, true
}

// EmailID returns EMAILID (RFC 8474), the server's stable id of the
// message content.
func (m *Message) EmailID() (string, bool) {
	v := m.Attrs["EMAILID"]
	if v.Type != ListValue || len(v.List) != 1 || v.List[0].Type != AtomValue {
		return "", false
	}
	return v.List[0].Atom, true
}

// Body returns the octets of BODY[section]: Body("") for the whole message
// (BODY.PEEK[]), Body("HEADER") for the header (BODY.PEEK[HEADER]). It is
// false when the item is absent or NIL.
func (m *Message) Body(section string) ([]byte, bool) {
	v, ok := m.Attrs["BODY["+asciiUpper(section)+"]"]
	if !ok || v.Type != StringValue {
		return nil, false
	}
	return v.Bytes, true
}

func textList(v Value) ([]string, bool) {
	if v.Type != ListValue {
		return nil, false
	}
	out := make([]string, 0, len(v.List))
	for _, x := range v.List {
		t, ok := x.Text()
		if !ok {
			return nil, false
		}
		out = append(out, t)
	}
	return out, true
}

// uidSet is RFC 3501's sequence-set.
var uidSet = regexp.MustCompile(`^(?:[0-9]+|\*)(?::(?:[0-9]+|\*))?(?:,(?:[0-9]+|\*)(?::(?:[0-9]+|\*))?)*$`)

// UIDFetch runs UID FETCH set (items) in the open mailbox and calls fn
// with each message as its response arrives, so only one message is held
// at a time. set is a sequence set of UIDs such as "1:100" or "7,9"
// (RFC 3501 §9); items are fetch data items, sent as given, such as
// "UID", "RFC822.SIZE", "INTERNALDATE", "FLAGS", "X-GM-MSGID",
// "X-GM-LABELS", "EMAILID" and "BODY.PEEK[]" — always BODY.PEEK, since
// BODY[] would set \Seen on a mailbox opened read-write.
//
// maxLiteral bounds the literal octets of one FETCH response: with
// BODY.PEEK[], the size of a message. A message over it fails the command
// with KindTooLarge before any of it is read, and the connection is
// closed; fetch RFC822.SIZE first to avoid that. A limit under 64 KiB is
// raised to it, so that labels or names sent as literals still parse.
//
// FETCH responses without a UID are unsolicited (RFC 3501 §7.4.2) and
// skipped. An error from fn stops the command, closes the connection and
// is returned as is.
func (c *Client) UIDFetch(ctx context.Context, set string, items []string, maxLiteral int64, fn func(*Message) error) error {
	op := "UID FETCH"
	if !uidSet.MatchString(set) {
		return &Error{Kind: KindRefused, Op: op, Err: errors.New("bad UID set")}
	}
	if len(items) == 0 {
		return &Error{Kind: KindRefused, Op: op, Err: errors.New("no fetch items")}
	}
	for _, it := range items {
		for i := 0; i < len(it); i++ {
			if it[i] < ' ' || it[i] >= 0x7f {
				return &Error{Kind: KindRefused, Op: op, Err: errors.New("bad fetch item")}
			}
		}
	}
	maxLiteral = max(maxLiteral, smallLiteral)
	cmd := "UID FETCH " + set + " (" + strings.Join(items, " ") + ")"
	_, err := c.exec(ctx, op, cmd, false, maxLiteral, func(r *response) error {
		if r.name != "FETCH" || !r.hasNum {
			return nil
		}
		m, err := parseFetch(r)
		if err != nil {
			return err
		}
		if _, ok := m.UID(); !ok {
			return nil
		}
		if err := fn(m); err != nil {
			return &callerError{err: err}
		}
		return nil
	}, nil)
	return err
}

// parseFetch reads "(" name SP value *(SP name SP value) ")".
func parseFetch(r *response) (*Message, error) {
	if len(r.fields) != 1 || r.fields[0].Type != ListValue || len(r.fields[0].List)%2 != 0 {
		return nil, syntaxf("FETCH response")
	}
	l := r.fields[0].List
	m := &Message{Seq: r.num, Attrs: make(map[string]Value, len(l)/2)}
	for i := 0; i < len(l); i += 2 {
		if l[i].Type != AtomValue {
			return nil, syntaxf("FETCH item name")
		}
		m.Attrs[asciiUpper(l[i].Atom)] = l[i+1]
	}
	return m, nil
}
