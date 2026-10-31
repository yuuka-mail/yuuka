package transport

import (
	"errors"
	"strings"
	"testing"
)

func TestInspect(t *testing.T) {
	long := strings.Repeat("x", maxLine)
	tests := []struct {
		name string
		raw  string
		want message
		err  bool
	}{
		{name: "plain", raw: "Subject: x\r\n\r\nbody\r\n", want: message{size: 20}},
		{name: "no trailing CRLF", raw: "Subject: x\r\n\r\nbody", want: message{size: 20}},
		{name: "998-octet line", raw: "Subject: x\r\n\r\n" + long + "\r\n", want: message{size: 14 + maxLine + 2}},
		{name: "8-bit header", raw: "Subject: é\r\n\r\nbody\r\n", want: message{header8bit: true, size: 21}},
		{name: "8-bit body", raw: "Subject: x\r\n\r\né\r\n", want: message{body8bit: true, size: 18}},
		{name: "no header section", raw: "\r\né\r\n", want: message{body8bit: true, size: 6}},
		{name: "empty", raw: "", err: true},
		{name: "bare LF", raw: "Subject: x\r\n\r\nbody\nmore\r\n", err: true},
		{name: "bare LF at the start", raw: "\n", err: true},
		{name: "bare CR", raw: "Subject: x\r\n\r\nbo\rdy\r\n", err: true},
		{name: "CR at the end", raw: "Subject: x\r\n\r\nbody\r", err: true},
		{name: "NUL", raw: "Subject: x\r\n\r\nbo\x00dy\r\n", err: true},
		{name: "999-octet line", raw: "Subject: x\r\n\r\n" + long + "x\r\n", err: true},
		{name: "999-octet last line", raw: "Subject: x\r\n\r\n" + long + "x", err: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := inspect([]byte(tt.raw))
			if tt.err {
				if !errors.Is(err, ErrInvalidMessage) {
					t.Errorf("error %v, want ErrInvalidMessage", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("got %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

func TestStripHeaders(t *testing.T) {
	raw := "Return-Path: <bounce@example.com>\r\n" +
		"From: a@example.com\r\n" +
		"message-id: <1@example.com>\r\n" +
		"DKIM-Signature: v=1; a=rsa-sha256; d=example.com;\r\n" +
		"\tb=abc\r\n" +
		" def\r\n" +
		"Subject: Message-ID: is in this subject\r\n" +
		"Message-IDs: <not the same field>\r\n" +
		"\r\n" +
		"Message-ID: <in the body, untouched>\r\n"
	tests := []struct {
		name string
		caps Caps
		want string
	}{
		{
			name: "cloudflare",
			caps: CloudflareCaps(),
			want: "From: a@example.com\r\n" +
				"Subject: Message-ID: is in this subject\r\n" +
				"Message-IDs: <not the same field>\r\n" +
				"\r\n" +
				"Message-ID: <in the body, untouched>\r\n",
		},
		{
			name: "everything allowed but Return-Path",
			caps: Caps{CallerMessageID: true, CallerReturnPath: true, CallerDKIM: true},
			want: strings.TrimPrefix(raw, "Return-Path: <bounce@example.com>\r\n"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StripHeaders([]byte(raw), tt.caps)
			if string(got) != tt.want {
				t.Errorf("got\n%q\nwant\n%q", got, tt.want)
			}
			if err := checkHeaders(got, tt.caps); err != nil {
				t.Errorf("stripped message fails the check: %v", err)
			}
		})
	}
	if err := checkHeaders([]byte(raw), CloudflareCaps()); !errors.Is(err, ErrHeaderNotAllowed) {
		t.Errorf("check passed the original: %v", err)
	}
}

func TestStripHeadersBareLF(t *testing.T) {
	// Input from the submission listener may not be validated yet.
	got := StripHeaders([]byte("Message-ID: <1@example.com>\nSubject: x\n\nbody\n"), CloudflareCaps())
	if want := "Subject: x\n\nbody\n"; string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
