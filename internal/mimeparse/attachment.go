package mimeparse

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime/quotedprintable"
	"strconv"
	"strings"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/textproto"
)

// ErrNoAttachment: the message has no attachment with that section
// number. The section may not exist, be a multipart container, the text
// of the message, or an inline part.
var ErrNoAttachment = errors.New("no such attachment")

// maxAttachment bounds what Attachment decodes; a raw message is at most
// 25 MiB (§2.2), so a decoded part cannot honestly be larger.
const maxAttachment = 32 << 20

// Attachment returns the attachment numbered section (an IMAP section
// number, as Entity.Section) and its body with the transfer encoding
// decoded and nothing else: the bytes the sender attached. Unlike the
// text ParseTree reads, a text/* attachment is not converted to UTF-8.
//
// What counts as an attachment is ParseTree's decision (Entity.Attachment)
// and nothing else, so the download and the attachment list cannot
// disagree; the bytes are then located by walking the same MIME
// structure without go-message's decoding, which would transcode text.
func Attachment(raw []byte, section string) (Entity, []byte, error) {
	_, root := ParseTree(raw)
	ent, ok := findSection(root, section)
	if !ok || !ent.Attachment || len(ent.Children) > 0 {
		return Entity{}, nil, ErrNoAttachment
	}
	h, body, err := locate(raw, section)
	if err != nil {
		return Entity{}, nil, err
	}
	b, err := decodeTransfer(h.Get("Content-Transfer-Encoding"), body)
	if err != nil {
		return Entity{}, nil, fmt.Errorf("attachment %s: %w", section, err)
	}
	return ent, b, nil
}

func findSection(e Entity, section string) (Entity, bool) {
	if e.Section == section && !strings.HasPrefix(e.MediaType, "multipart/") {
		return e, true
	}
	for _, c := range e.Children {
		if f, ok := findSection(c, section); ok {
			return f, true
		}
	}
	return Entity{}, false
}

// locate finds the header and undecoded body of section the way ParseTree
// numbers it: the header block it read (sanitized if the original did
// not parse), then one multipart level per number.
func locate(raw []byte, section string) (textproto.Header, io.Reader, error) {
	if e, err := message.Read(bytes.NewReader(raw)); err != nil && e == nil {
		raw = sanitizeHeader(raw)
	}
	br := bufio.NewReader(bytes.NewReader(raw))
	h, err := textproto.ReadHeader(br)
	if err != nil {
		return h, nil, fmt.Errorf("attachment %s: header: %w", section, err)
	}
	var body io.Reader = br
	if !isMultipart(h) {
		// The body of a single-part message is section 1.
		if section != "1" {
			return h, nil, ErrNoAttachment
		}
		return h, body, nil
	}
	for num := range strings.SplitSeq(section, ".") {
		n, err := strconv.Atoi(num)
		if err != nil || n < 1 || !isMultipart(h) {
			return h, nil, ErrNoAttachment
		}
		mh := message.Header{Header: h}
		_, params, _ := mh.ContentType()
		mr := textproto.NewMultipartReader(body, params["boundary"])
		for i := 1; i <= n; i++ {
			p, err := mr.NextPart()
			if err != nil {
				return h, nil, fmt.Errorf("attachment %s: %w", section, err)
			}
			h, body = p.Header, p
		}
	}
	if isMultipart(h) {
		return h, nil, ErrNoAttachment
	}
	return h, body, nil
}

// isMultipart is go-message's test for a multipart entity.
func isMultipart(h textproto.Header) bool {
	mh := message.Header{Header: h}
	mt, _, _ := mh.ContentType()
	return strings.HasPrefix(mt, "multipart/")
}

// decodeTransfer undoes the Content-Transfer-Encoding. An encoding it does
// not know is served as it is, as go-message reads it. A body cut off by
// a missing closing boundary is what the raw message holds and is kept;
// base64 that does not decode is an error, not a silently shorter file.
func decodeTransfer(enc string, body io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(body, maxAttachment+1))
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	if len(b) > maxAttachment {
		return nil, fmt.Errorf("larger than %d bytes", maxAttachment)
	}
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "base64":
		clean := bytes.Map(func(r rune) rune {
			if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
				return -1
			}
			return r
		}, b)
		// Some mailers drop the padding.
		out, err := base64.RawStdEncoding.DecodeString(string(bytes.TrimRight(clean, "=")))
		if err != nil {
			return nil, fmt.Errorf("base64: %w", err)
		}
		return out, nil
	case "quoted-printable":
		out, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(b)))
		if err != nil {
			return nil, fmt.Errorf("quoted-printable: %w", err)
		}
		return out, nil
	}
	return b, nil
}
