package imap

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// dump renders a response compactly for comparison.
func dump(r *response) string {
	var b strings.Builder
	b.WriteString(r.tag)
	if r.hasNum {
		fmt.Fprintf(&b, " #%d", r.num)
	}
	if r.name != "" {
		b.WriteString(" " + r.name)
	}
	if s := r.status; s != nil {
		fmt.Fprintf(&b, " code=%q arg=%q text=%q", s.Code, s.Arg, s.Text)
	}
	if r.tag == "+" {
		fmt.Fprintf(&b, " text=%q", r.text)
	}
	if r.uids != nil {
		fmt.Fprintf(&b, " uids=%v", r.uids)
	}
	for _, v := range r.fields {
		b.WriteString(" " + dumpValue(v))
	}
	return b.String()
}

func dumpValue(v Value) string {
	switch v.Type {
	case AtomValue:
		return v.Atom
	case StringValue:
		return fmt.Sprintf("%q", v.Bytes)
	case NilValue:
		return "NIL"
	case ListValue:
		parts := make([]string, len(v.List))
		for i, x := range v.List {
			parts[i] = dumpValue(x)
		}
		return "(" + strings.Join(parts, " ") + ")"
	}
	return "?"
}

func TestReadResponse(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"* OK [ALERT] mind the gap\r\n", `* OK code="ALERT" arg="" text="mind the gap"`},
		{"* OK [HIGHESTMODSEQ 1]\r\n", `* OK code="HIGHESTMODSEQ" arg="1" text=""`},
		{"* ok\r\n", `* OK code="" arg="" text=""`},
		{"* OK [PERMANENTFLAGS (\\Seen \\*)] Limited\r\n", `* OK code="PERMANENTFLAGS" arg="(\\Seen \\*)" text="Limited"`},
		{"* BYE (unbalanced [text\r\n", `* BYE code="" arg="" text="(unbalanced [text"`},
		{"A12 OK [READ-ONLY] Done\r\n", `A12 OK code="READ-ONLY" arg="" text="Done"`},
		{"+ \r\n", `+ text=""`},
		{"+\r\n", `+ text=""`},
		{"+ eyJ9\r\n", `+ text="eyJ9"`},
		{"* 3 EXISTS\r\n", `* #3 EXISTS`},
		{"* SEARCH\r\n", `* SEARCH`},
		{"* SEARCH 1 4294967295 (MODSEQ 9)\r\n", `* SEARCH uids=[1 4294967295]`},
		{"* CAPABILITY IMAP4rev1 AUTH=PLAIN\r\n", `* CAPABILITY IMAP4rev1 AUTH=PLAIN`},
		{
			"* 1 FETCH (BODY[HEADER.FIELDS (FROM TO)]<0> {3}\r\nabc BINARY[] ~{2}\r\n\x00\x01 X nil Y \"a\\\\\\\"b\")\r\n",
			`* #1 FETCH (BODY[HEADER.FIELDS (FROM TO)]<0> "abc" BINARY[] "\x00\x01" X NIL Y "a\\\"b")`,
		},
		{"* LIST () NIL {0}\r\n\r\n", `* LIST () NIL ""`},
		{"* 1 FETCH (FLAGS (\\Seen $Label ~x))\r\n", `* #1 FETCH (FLAGS (\Seen $Label ~x))`},
		{"* ID NIL\r\n", `* ID NIL`},
	} {
		rd := newReader(strings.NewReader(tc.in))
		r, err := rd.readResponse(16)
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if got := dump(r); got != tc.want {
			t.Errorf("%q:\n got %s\nwant %s", tc.in, got, tc.want)
		}
		if _, err := rd.readResponse(16); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("%q: trailing input: %v", tc.in, err)
		}
	}
}

func TestReadResponseErrors(t *testing.T) {
	var syn *errSyntax
	var big *errTooLarge
	for _, tc := range []struct {
		in   string
		want any
	}{
		{"* 1 FETCH (BODY[] {17}\r\n", &big},
		{"* 1 FETCH (BODY[] {99999999999999999999999}\r\n", &big},
		{"* 1 FETCH (A {8}\r\n12345678 B {9}\r\n", &big}, // the budget is per response
		{"* X " + strings.Repeat("a", maxLine) + "\r\n", &big},
		{"* X " + strings.Repeat("(", maxDepth+1) + "\r\n", &big},
		{"* X \"a\rb\"\r\n", &syn},
		{" OK\r\n", &syn},
		{"A1 FETCH (UID 1)\r\n", &syn},
		{"* X {}\r\n", &syn},
		{"* X {3+}\r\nabc\r\n", &syn},
		{"* X (a\r\n", &syn},
		{"* X a)\r\n", &syn},
		{"* SEARCH 1 x\r\n", &syn},
		{"* SEARCH 4294967296\r\n", &syn},
		{"* 99999999999 EXISTS\r\n", &syn},
		{"* OK [\r\n", &syn},
		{"* OK [ALERT\r\n", &syn},
		{"* X\n", &syn},
		{"* X {5}\r\nab", io.ErrUnexpectedEOF},
		{"* X", io.ErrUnexpectedEOF},
	} {
		_, err := newReader(strings.NewReader(tc.in)).readResponse(16)
		ok := false
		switch w := tc.want.(type) {
		case error:
			ok = errors.Is(err, w)
		default:
			ok = err != nil && errors.As(err, w)
		}
		if !ok {
			t.Errorf("%.40q: err = %v, want %T", tc.in, err, tc.want)
		}
	}
}

func TestMailboxUTF7(t *testing.T) {
	for _, tc := range []struct{ utf8, wire string }{
		{"INBOX", "INBOX"},
		{"A&B", "A&-B"},
		{"&", "&-"},
		{"日本語", "&ZeVnLIqe-"},
		{"Reçus/2026", "Re&AOc-us/2026"},
		{"~peter/mail/台北/日本語", "~peter/mail/&U,BTFw-/&ZeVnLIqe-"},
		{"🎉 party", "&2DzfiQ- party"},
		{"tab\there", "tab&AAk-here"},
	} {
		got, err := EncodeMailbox(tc.utf8)
		if err != nil || got != tc.wire {
			t.Errorf("Encode(%q) = %q, %v; want %q", tc.utf8, got, err, tc.wire)
		}
		back, err := DecodeMailbox(tc.wire)
		if err != nil || back != tc.utf8 {
			t.Errorf("Decode(%q) = %q, %v; want %q", tc.wire, back, err, tc.utf8)
		}
	}
	for _, bad := range []string{
		"&",           // unterminated
		"&Jjo",        // unterminated
		"&Jjo!-",      // not base64
		"&AGE-",       // printable ASCII encoded
		"&2Dw-",       // lone high surrogate
		"&3+k-",       // lone low surrogate
		"&AO-",        // odd number of octets
		"&AOd-",       // non-zero padding bits
		"Reçus",       // raw 8-bit
		"a\x00b",      // control
		"&ZeVnLIqe=-", // padding
	} {
		if s, err := DecodeMailbox(bad); err == nil {
			t.Errorf("Decode(%q) = %q, want an error", bad, s)
		}
	}
	if _, err := EncodeMailbox("\xff"); err == nil {
		t.Error("Encode accepted invalid UTF-8")
	}
	// Adjacent encoded runs are not canonical but decode.
	if s, err := DecodeMailbox("&AOk-&AOk-"); err != nil || s != "éé" {
		t.Errorf("adjacent runs = %q, %v", s, err)
	}
}
