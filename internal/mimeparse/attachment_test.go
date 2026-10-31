package mimeparse

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// nested has attachments at several depths and encodings, a text
// attachment in a legacy charset, and parts that are not attachments.
const nested = "From: a@example.net\r\n" +
	"Subject: files\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"outer\"\r\n" +
	"\r\n" +
	"--outer\r\n" +
	"Content-Type: multipart/related; boundary=\"rel\"\r\n" +
	"\r\n" +
	"--rel\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<p>see attached</p><img src=\"cid:logo\">\r\n" +
	"--rel\r\n" +
	"Content-Type: image/png; name=\"logo.png\"\r\n" +
	"Content-ID: <logo>\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"iVBORw0KGgo=\r\n" +
	"--rel\r\n" +
	"Content-Type: application/octet-stream\r\n" +
	"Content-Disposition: attachment; filename=\"deep.bin\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"AAEC AwQF\r\n" +
	"Bgc\r\n" +
	"--rel--\r\n" +
	"--outer\r\n" +
	"Content-Type: text/csv; charset=gb2312\r\n" +
	"Content-Disposition: attachment; filename=\"table.csv\"\r\n" +
	"Content-Transfer-Encoding: quoted-printable\r\n" +
	"\r\n" +
	"=C4=E3=BA=C3,1\r\n" +
	"--outer\r\n" +
	"Content-Type: application/pdf; name=\"broken.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"!!!not base64!!!\r\n" +
	"--outer--\r\n"

func TestAttachment(t *testing.T) {
	raw := []byte(nested)
	for section, want := range map[string]string{
		"1.3": "\x00\x01\x02\x03\x04\x05\x06\x07",
		// The bytes as attached: GB2312, not transcoded to UTF-8.
		"2": "\xc4\xe3\xba\xc3,1",
	} {
		ent, body, err := Attachment(raw, section)
		if err != nil || !ent.Attachment || ent.Section != section {
			t.Errorf("%s: %+v %v", section, ent, err)
			continue
		}
		if !bytes.Equal(body, []byte(want)) {
			t.Errorf("%s: %q, want %q", section, body, want)
		}
	}
	if ent, _, _ := Attachment(raw, "2"); ent.Filename != "table.csv" || ent.MediaType != "text/csv" {
		t.Errorf("entity %+v", ent)
	}
	// Containers, the text, an inline image, made-up and malformed numbers.
	for _, section := range []string{"", "1", "1.1", "1.2", "1.1.1", "4", "0", "1.", "a", "-1", "1.4", "01.3"} {
		if _, _, err := Attachment(raw, section); !errors.Is(err, ErrNoAttachment) {
			t.Errorf("%q: %v", section, err)
		}
	}
	if _, _, err := Attachment(raw, "3"); err == nil || errors.Is(err, ErrNoAttachment) {
		t.Errorf("bad base64: %v", err)
	}
}

// A single-part message whose body is the attachment is section 1.
func TestAttachmentSinglePart(t *testing.T) {
	raw := []byte("From: a@example.net\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=x.pdf\r\n\r\n%PDF-1.4\r\n")
	_, body, err := Attachment(raw, "1")
	if err != nil || string(body) != "%PDF-1.4\r\n" {
		t.Errorf("%q %v", body, err)
	}
	if _, _, err := Attachment(raw, "2"); !errors.Is(err, ErrNoAttachment) {
		t.Errorf("2: %v", err)
	}
}

// Every attachment the parser lists in the corpus can be fetched, and
// nothing else can.
func TestAttachmentCorpus(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(corpusDir, "*.eml"))
	found := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		_, root := ParseTree(raw)
		var walk func(e Entity)
		walk = func(e Entity) {
			for _, c := range e.Children {
				walk(c)
			}
			if e.Section == "" {
				return
			}
			_, body, err := Attachment(raw, e.Section)
			switch {
			case e.Attachment && (err != nil || len(body) == 0):
				t.Errorf("%s %s: %v", filepath.Base(f), e.Section, err)
			case e.Attachment:
				found++
			case !errors.Is(err, ErrNoAttachment):
				t.Errorf("%s %s (%s) is not an attachment: %v", filepath.Base(f), e.Section, e.MediaType, err)
			}
		}
		walk(root)
	}
	if found < 3 {
		t.Errorf("%d attachments in the corpus", found)
	}
}
