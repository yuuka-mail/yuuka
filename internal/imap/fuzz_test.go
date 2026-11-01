package imap

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// serverBytes returns what the server sends in each transcript, the seed
// corpus of the fuzz tests.
func serverBytes(f *testing.F) [][]byte {
	f.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "transcripts", "*.txt"))
	if err != nil || len(paths) == 0 {
		f.Fatalf("no transcripts: %v", err)
	}
	var out [][]byte
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		var b bytes.Buffer
		for _, l := range strings.Split(string(data), "\n") {
			if t, ok := strings.CutPrefix(l, "S:"); ok && t != " "+hang {
				b.WriteString(strings.TrimPrefix(t, " ") + "\r\n")
			}
		}
		out = append(out, b.Bytes())
	}
	return out
}

const fuzzLiteral = 1 << 10

// FuzzReadResponse feeds arbitrary bytes to the response parser and to the
// parsers of the responses the client understands. It must never panic,
// and what it keeps of a response must stay within the bounds.
func FuzzReadResponse(f *testing.F) {
	for _, b := range serverBytes(f) {
		f.Add(b)
	}
	f.Add([]byte("* 1 FETCH (BODY[] ~{3}\r\n\x00\r\n UID 4294967295 X-GM-LABELS (\"\\\\Inbox\" &AOk-))\r\n"))
	f.Add([]byte("* SEARCH 1 2 3 (MODSEQ 4)\r\n* OK [UIDVALIDITY 4294967296] x\r\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		rd := newReader(bytes.NewReader(data))
		for range 64 {
			r, err := rd.readResponse(fuzzLiteral)
			if err != nil {
				return
			}
			if n := size(r.fields); n > maxLine+fuzzLiteral {
				t.Fatalf("response holds %d bytes", n)
			}
			exercise(r)
		}
	})
}

// size counts the string bytes a response holds.
func size(vs []Value) int {
	n := 0
	for _, v := range vs {
		n += len(v.Atom) + len(v.Bytes) + size(v.List)
	}
	return n
}

// exercise runs every interpretation the client applies to a response.
func exercise(r *response) {
	if r.status != nil {
		_ = selectCode(&Selected{}, r.status)
		_ = classify("FUZZ", r.status, true)
		_ = r.status.String()
	}
	switch r.name {
	case "FETCH":
		if m, err := parseFetch(r); err == nil {
			exerciseMessage(m)
		}
	case "LIST":
		if m, err := parseList(r.fields); err == nil {
			_ = m.DisplayName()
			_ = m.Selectable()
		}
	case "STATUS":
		_, _, _ = parseStatus(r.fields)
	}
}

func exerciseMessage(m *Message) {
	_, _ = m.UID()
	_, _ = m.Size()
	_, _ = m.InternalDate()
	_, _ = m.Flags()
	_, _ = m.ModSeq()
	_, _ = m.GmailMsgID()
	_, _ = m.GmailLabels()
	_, _ = m.EmailID()
	_, _ = m.Body("")
	_, _ = m.Body("HEADER")
}

// FuzzClient plays arbitrary server output against a client running a
// whole sync round. Whatever the server sends, every call must return,
// without a panic, within the deadlines.
func FuzzClient(f *testing.F) {
	for _, b := range serverBytes(f) {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		cli, srv := net.Pipe()
		var wg sync.WaitGroup
		wg.Go(func() { _, _ = io.Copy(io.Discard, srv) })
		wg.Go(func() {
			_, _ = srv.Write(data)
			_ = srv.Close()
		})
		defer wg.Wait()
		defer func() { _ = cli.Close() }()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, err := newClient(ctx, cli, time.Second)
		if err != nil {
			return
		}
		_, _ = c.Capability(ctx)
		_, _ = c.ID(ctx, map[string]string{"name": "yuuka"})
		_ = c.Login(ctx, "user@example.com", "app-password")
		_ = c.AuthenticateXOAUTH2(ctx, "user@example.com", "token")
		_, _ = c.List(ctx, ListOptions{SpecialUse: true, Status: []string{"MESSAGES", "UIDNEXT"}})
		_, _ = c.Status(ctx, "INBOX", "MESSAGES", "UIDNEXT", "UIDVALIDITY", "HIGHESTMODSEQ")
		_, _ = c.Examine(ctx, "INBOX")
		_ = c.UIDFetch(ctx, "1:*", []string{"UID", "BODY.PEEK[]"}, fuzzLiteral, func(m *Message) error {
			exerciseMessage(m)
			if b, ok := m.Body(""); ok && len(b) > smallLiteral {
				t.Fatalf("body of %d bytes over the limit", len(b))
			}
			return nil
		})
		_, _ = c.UIDSearchAll(ctx)
		_ = c.Noop(ctx)
		_ = c.Unselect(ctx)
		_ = c.Alerts()
		_ = c.Logout(ctx)
	})
}

// FuzzMailboxName checks that modified UTF-7 round-trips.
func FuzzMailboxName(f *testing.F) {
	for _, s := range []string{"INBOX", "&ZeVnLIqe-", "Re&AOc-us", "&-", "&2DzfiQ-", "日本語", "a&b", "&AOk-&AOk-"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if enc, err := EncodeMailbox(s); err == nil {
			for i := 0; i < len(enc); i++ {
				if enc[i] < 0x20 || enc[i] > 0x7e {
					t.Fatalf("Encode(%q) = %q is not printable ASCII", s, enc)
				}
			}
			if dec, err := DecodeMailbox(enc); err != nil || dec != s {
				t.Fatalf("Decode(Encode(%q)) = %q, %v", s, dec, err)
			}
		} else if utf8.ValidString(s) {
			t.Fatalf("Encode(%q): %v", s, err)
		}
		if dec, err := DecodeMailbox(s); err == nil {
			if !utf8.ValidString(dec) {
				t.Fatalf("Decode(%q) = %q is not UTF-8", s, dec)
			}
			enc, err := EncodeMailbox(dec)
			if err != nil {
				t.Fatalf("Encode(Decode(%q)): %v", s, err)
			}
			if again, err := DecodeMailbox(enc); err != nil || again != dec {
				t.Fatalf("Decode(Encode(Decode(%q))) = %q, %v", s, again, err)
			}
		}
	})
}
