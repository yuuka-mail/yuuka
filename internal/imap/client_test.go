package imap

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

func dial(t *testing.T, cfg Config) *Client {
	t.Helper()
	c, err := Dial(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return c
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestGmailRound(t *testing.T) {
	runTranscript(t, "gmail", func(cfg Config) *Client {
		ctx := t.Context()
		c := dial(t, cfg)
		caps, err := c.Capability(ctx)
		must(t, err)
		if !caps.Has("x-gm-ext-1") || !caps.Has("ID") || caps.Has("LIST-STATUS") {
			t.Errorf("pre-login caps = %v", caps)
		}
		id, err := c.ID(ctx, map[string]string{"version": "test", "name": "yuuka"})
		must(t, err)
		if id["name"] != "GImap" || id["support-url"] != "" {
			t.Errorf("ID = %v", id)
		}
		must(t, c.AuthenticatePlain(ctx, "user@example.com", "app-password"))
		if !c.Caps().Has("LIST-STATUS") || !c.Caps().Has("SPECIAL-USE") {
			t.Errorf("post-login caps = %v", c.Caps())
		}

		boxes, err := c.List(ctx, ListOptions{SpecialUse: true, Status: []string{"MESSAGES", "UIDNEXT", "UIDVALIDITY"}})
		must(t, err)
		if len(boxes) != 5 {
			t.Fatalf("%d mailboxes", len(boxes))
		}
		all := boxes[2]
		if all.Name != "[Gmail]/All Mail" || !all.HasAttr(`\all`) || all.Delim != "/" ||
			*all.Status != (MailboxStatus{Messages: 3, UIDNext: 300, UIDValidity: 11}) {
			t.Errorf("All Mail = %+v %+v", all, all.Status)
		}
		if boxes[1].Selectable() || !boxes[0].Selectable() {
			t.Errorf("selectable: [Gmail] %v, INBOX %v", boxes[1].Selectable(), boxes[0].Selectable())
		}
		if j := boxes[4]; j.Name != "&ZeVnLIqe-" || j.DisplayName() != "日本語" || j.Status.UIDValidity != 23 {
			t.Errorf("literal name = %+v %+v", j, j.Status)
		}

		sel, err := c.Examine(ctx, all.Name)
		must(t, err)
		if *sel != (Selected{Name: all.Name, UIDValidity: 11, UIDNext: 300, Exists: 3, HighestModSeq: 4242, ReadOnly: true}) {
			t.Errorf("Examine = %+v", sel)
		}

		var msgs []*Message
		must(t, c.UIDFetch(ctx, "1:299", []string{"UID", "RFC822.SIZE", "INTERNALDATE", "FLAGS", "X-GM-MSGID", "X-GM-LABELS"}, 0, func(m *Message) error {
			msgs = append(msgs, m)
			return nil
		}))
		if len(msgs) != 3 {
			t.Fatalf("%d messages, want 3 (the unsolicited FETCH skipped)", len(msgs))
		}
		m := msgs[0]
		uid, _ := m.UID()
		size, _ := m.Size()
		date, _ := m.InternalDate()
		gid, _ := m.GmailMsgID()
		labels, _ := m.GmailLabels()
		flags, _ := m.Flags()
		if uid != 101 || size != 2048 || gid != 1278455344230334865 || m.Seq != 1 {
			t.Errorf("message 1: uid %d size %d gmid %d seq %d", uid, size, gid, m.Seq)
		}
		if !date.Equal(time.Date(2026, 7, 7, 2, 44, 25, 0, time.UTC)) {
			t.Errorf("INTERNALDATE = %v", date)
		}
		if !reflect.DeepEqual(labels, []string{`\Inbox`, `\Important`, "Receipts"}) || !reflect.DeepEqual(flags, []string{`\Seen`}) {
			t.Errorf("labels %q flags %q", labels, flags)
		}
		if l, _ := msgs[1].GmailLabels(); !reflect.DeepEqual(l, []string{`\Sent`, "日本語"}) {
			t.Errorf("message 2 labels = %q", l)
		}
		if d, _ := msgs[1].InternalDate(); !d.Equal(time.Date(2026, 7, 17, 17, 0, 0, 0, time.UTC)) {
			t.Errorf("message 2 INTERNALDATE = %v", d)
		}
		if l, ok := msgs[2].GmailLabels(); !ok || len(l) != 0 {
			t.Errorf("message 3 labels = %q %v", l, ok)
		}
		if f := msgs[2].Attrs["X-FUTURE"]; f.Type != ListValue || len(f.List) != 3 || f.List[2].Type != NilValue {
			t.Errorf("X-FUTURE = %+v", f)
		}
		if _, ok := msgs[2].EmailID(); ok {
			t.Error("EmailID present on Gmail")
		}

		var body []byte
		must(t, c.UIDFetch(ctx, "101", []string{"BODY.PEEK[]"}, 1<<20, func(m *Message) error {
			body, _ = m.Body("")
			return nil
		}))
		want := "From: Alice <alice@example.com>\r\nTo: user@example.com\r\nSubject: Hello\r\n\r\nHi there.\r\n"
		if string(body) != want {
			t.Errorf("body = %q", body)
		}

		uids, err := c.UIDSearchAll(ctx)
		must(t, err)
		if !reflect.DeepEqual(uids, []uint32{101, 205, 299}) {
			t.Errorf("SEARCH = %v", uids)
		}
		must(t, c.Unselect(ctx))
		must(t, c.Logout(ctx))
		return nil
	})
}

func TestDovecotRound(t *testing.T) {
	runTranscript(t, "dovecot", func(cfg Config) *Client {
		ctx := t.Context()
		c := dial(t, cfg)
		if !c.Caps().Has("AUTH=PLAIN") {
			t.Errorf("greeting caps = %v", c.Caps())
		}
		must(t, c.Login(ctx, "user@example.com", `pa"ss\word`))
		if !c.Caps().Has("OBJECTID") {
			t.Errorf("post-login caps = %v", c.Caps())
		}
		boxes, err := c.List(ctx, ListOptions{SpecialUse: true, Status: []string{"MESSAGES"}})
		must(t, err)
		var names []string
		for _, b := range boxes {
			names = append(names, b.Name)
			if b.Status != nil {
				t.Errorf("%s has a status without LIST-STATUS", b.Name)
			}
		}
		if !reflect.DeepEqual(names, []string{"INBOX", "Junk", `Work "old"`, "Shared"}) {
			t.Errorf("names = %q", names)
		}
		if !boxes[1].HasAttr(`\Junk`) || boxes[3].Delim != "" || boxes[3].Selectable() {
			t.Errorf("boxes = %+v", boxes)
		}

		st, err := c.Status(ctx, "INBOX", "MESSAGES", "UIDNEXT", "UIDVALIDITY", "HIGHESTMODSEQ")
		must(t, err)
		if *st != (MailboxStatus{Messages: 2, UIDNext: 3, UIDValidity: 1700000000, HighestModSeq: 9}) {
			t.Errorf("STATUS = %+v", st)
		}
		sel, err := c.Examine(ctx, "INBOX")
		must(t, err)
		if sel.UIDValidity != 1700000000 || sel.UIDNext != 3 || sel.Exists != 2 || sel.HighestModSeq != 9 {
			t.Errorf("Examine = %+v", sel)
		}

		ids := map[uint32]string{}
		sizes := map[uint32]int64{}
		must(t, c.UIDFetch(ctx, "1:2", []string{"UID", "RFC822.SIZE", "EMAILID"}, 0, func(m *Message) error {
			uid, _ := m.UID()
			ids[uid], _ = m.EmailID()
			sizes[uid], _ = m.Size()
			return nil
		}))
		if ids[1] != "M6d99ac3275bb4e" || ids[2] != "Mbd2d0a3b1c2e5f" || sizes[2] != 50000000 {
			t.Errorf("ids %v sizes %v", ids, sizes)
		}
		var header []byte
		must(t, c.UIDFetch(ctx, "2", []string{"BODY.PEEK[HEADER]"}, 0, func(m *Message) error {
			header, _ = m.Body("header")
			if _, ok := m.Body(""); ok {
				t.Error("BODY[] present in a header fetch")
			}
			return nil
		}))
		if string(header) != "From: Bob <bob@example.com>\r\nSubject: Big attachment\r\n\r\n" {
			t.Errorf("header = %q", header)
		}
		must(t, c.Noop(ctx))
		must(t, c.Unselect(ctx))
		must(t, c.Logout(ctx))
		return nil
	})
}

func TestPlainWithoutSASLIR(t *testing.T) {
	runTranscript(t, "plain_no_ir", func(cfg Config) *Client {
		c := dial(t, cfg)
		must(t, c.AuthenticatePlain(t.Context(), "user@example.com", "app-password"))
		if !c.Caps().Has("UNSELECT") {
			t.Errorf("caps = %v", c.Caps())
		}
		must(t, c.Logout(t.Context()))
		return nil
	})
}

func TestXOAUTH2Rejected(t *testing.T) {
	runTranscript(t, "xoauth2", func(cfg Config) *Client {
		c := dial(t, cfg)
		err := c.AuthenticateXOAUTH2(t.Context(), "user@example.com", "ya29.test-token")
		e := kindErr(t, err, KindAuth)
		if !strings.Contains(e.Error(), `\"status\":\"400\"`) || e.Resp.Code != "AUTHENTICATIONFAILED" {
			t.Errorf("error = %v", e)
		}
		if strings.Contains(e.Error(), "ya29") {
			t.Error("the error shows the token")
		}
		// A refused command leaves the connection usable, and the caps
		// from before the attempt are kept.
		if !c.Caps().Has("AUTH=PLAIN") {
			t.Errorf("caps lost: %v", c.Caps())
		}
		return c
	})
}

func TestLoginRefusals(t *testing.T) {
	runTranscript(t, "login_refusals", func(cfg Config) *Client {
		c := dial(t, cfg)
		login := func() error { return c.Login(t.Context(), "user@example.com", "app-password") }
		wantKind(t, login(), KindConnection)
		wantKind(t, login(), KindAuth)
		if a := c.Alerts(); !reflect.DeepEqual(a, []string{"Please log in via your web browser"}) {
			t.Errorf("alerts = %q", a)
		}
		e := kindErr(t, login(), KindAuth)
		if strings.Contains(e.Error(), "app-password") {
			t.Error("the error shows the password")
		}
		return c
	})
}

func TestLimits(t *testing.T) {
	runTranscript(t, "limits", func(cfg Config) *Client {
		c := dial(t, cfg)
		fetch := func() error {
			return c.UIDFetch(t.Context(), "1", []string{"BODY.PEEK[]"}, 1<<20, func(*Message) error { return nil })
		}
		e := kindErr(t, fetch(), KindLimit)
		if e.Resp.Code != "OVERQUOTA" {
			t.Errorf("resp = %v", e.Resp)
		}
		e = kindErr(t, fetch(), KindLimit)
		if e.Resp == nil || e.Resp.Type != "BYE" {
			t.Errorf("resp = %v", e.Resp)
		}
		return c
	})
}

func TestGreetingBye(t *testing.T) {
	runTranscript(t, "greeting_bye", func(cfg Config) *Client {
		_, err := Dial(t.Context(), cfg)
		wantKind(t, err, KindLimit)
		return nil
	})
}

func TestLiteralTooLarge(t *testing.T) {
	runTranscript(t, "too_large", func(cfg Config) *Client {
		c := dial(t, cfg)
		called := false
		err := c.UIDFetch(t.Context(), "1", []string{"BODY.PEEK[]"}, 50<<20, func(*Message) error {
			called = true
			return nil
		})
		wantKind(t, err, KindTooLarge)
		if called {
			t.Error("callback ran")
		}
		// The connection is gone; later commands fail without I/O.
		if err2 := c.Noop(t.Context()); !errors.Is(err2, err) {
			t.Errorf("after: %v", err2)
		}
		return c
	})
}

func TestProtocolErrors(t *testing.T) {
	for _, name := range []string{"protocol_error", "wrong_tag"} {
		t.Run(name, func(t *testing.T) {
			runTranscript(t, name, func(cfg Config) *Client {
				c := dial(t, cfg)
				wantKind(t, c.Noop(t.Context()), KindProtocol)
				return c
			})
		})
	}
}

func TestDeadlines(t *testing.T) {
	t.Run("context", func(t *testing.T) {
		runTranscript(t, "hang", func(cfg Config) *Client {
			c := dial(t, cfg)
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			err := c.Noop(ctx)
			wantKind(t, err, KindConnection)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("err = %v, want the context's error", err)
			}
			return c
		})
	})
	t.Run("cancel", func(t *testing.T) {
		runTranscript(t, "hang", func(cfg Config) *Client {
			c := dial(t, cfg)
			ctx, cancel := context.WithCancel(t.Context())
			time.AfterFunc(100*time.Millisecond, cancel)
			err := c.Noop(ctx)
			if !errors.Is(err, context.Canceled) {
				t.Errorf("err = %v, want context.Canceled", err)
			}
			return c
		})
	})
	t.Run("idle", func(t *testing.T) {
		runTranscript(t, "hang", func(cfg Config) *Client {
			cfg.IdleTimeout = 100 * time.Millisecond
			c := dial(t, cfg)
			err := c.Noop(t.Context())
			wantKind(t, err, KindConnection)
			var ne net.Error
			if !errors.As(err, &ne) || !ne.Timeout() {
				t.Errorf("err = %v, want a timeout", err)
			}
			return c
		})
	})
}

func TestCallbackError(t *testing.T) {
	runTranscript(t, "callback_error", func(cfg Config) *Client {
		c := dial(t, cfg)
		stop := errors.New("disk full")
		calls := 0
		err := c.UIDFetch(t.Context(), "1:2", []string{"UID"}, 0, func(*Message) error {
			calls++
			return stop
		})
		if !errors.Is(err, stop) || calls != 1 {
			t.Errorf("err = %v after %d calls, want the callback's error as is after 1", err, calls)
		}
		wantKind(t, c.Noop(t.Context()), KindConnection)
		return c
	})
}

func TestLocalRefusals(t *testing.T) {
	runTranscript(t, "hang", func(cfg Config) *Client {
		ctx := t.Context()
		c := dial(t, cfg)
		nop := func(*Message) error { return nil }
		for name, err := range map[string]error{
			"set":         c.UIDFetch(ctx, "1:*)", []string{"UID"}, 0, nop),
			"no items":    c.UIDFetch(ctx, "1", nil, 0, nop),
			"item CRLF":   c.UIDFetch(ctx, "1", []string{"UID\r\nA2 LOGOUT"}, 0, nop),
			"login 8-bit": c.Login(ctx, "user@example.com", "pässword"),
			"no PLAIN":    c.AuthenticatePlain(ctx, "user@example.com", "x"),
			"status item": func() error { _, err := c.Status(ctx, "INBOX", "MESSAGES)"); return err }(),
			"mailbox LF":  func() error { _, err := c.Examine(ctx, "a\nb"); return err }(),
		} {
			var e *Error
			if !errors.As(err, &e) || e.Kind != KindRefused {
				t.Errorf("%s: %v", name, err)
			}
		}
		// Nothing above reached the server; this does.
		ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		_ = c.Noop(ctx)
		return c
	})
}
