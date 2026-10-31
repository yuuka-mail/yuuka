package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testRaw has dot-stuffing to round-trip and ends with CRLF.
const testRaw = "From: sender@example.com\r\n" +
	"To: rcpt@example.com\r\n" +
	"Subject: test\r\n" +
	"\r\n" +
	"Hello.\r\n" +
	".a line that starts with a dot\r\n" +
	"..and two\r\n" +
	".\r\n" +
	"bye\r\n"

func send(t *testing.T, cfg Config, ctx context.Context, to []string, raw string, commit func(context.Context) error) (*SendResult, error) {
	t.Helper()
	tr, err := NewSMTP(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tr.Send(ctx, Envelope{From: "sender@example.com", To: to}, []byte(raw), commit)
	if res == nil {
		t.Fatal("Send returned a nil result")
	}
	if len(res.Recipients) != len(to) {
		t.Fatalf("got %d recipient results for %d recipients", len(res.Recipients), len(to))
	}
	for i, r := range res.Recipients {
		if r.Addr != to[i] {
			t.Errorf("recipient %d is %q, want %q", i, r.Addr, to[i])
		}
		if r.Status == statusPending {
			t.Errorf("recipient %s left pending", r.Addr)
		}
	}
	if ok := res.Outcome == OutcomeAccepted || res.Outcome == OutcomePartial; ok != (err == nil) {
		t.Errorf("outcome %v with error %v", res.Outcome, err)
	}
	if !res.Committed && res.Outcome == OutcomeUnknown {
		t.Error("Unknown without crossing the commit point")
	}
	return res, err
}

func TestSendAccepted(t *testing.T) {
	srv := newServer(t, behavior{})
	var calls int
	commit := func(context.Context) error {
		calls++
		// Every body byte must have reached the server, and the
		// end-of-data must not have.
		deadline := time.Now().Add(5 * time.Second)
		for {
			n, dot := srv.progress()
			if dot {
				t.Error("end-of-data sent before commit")
				return nil
			}
			if n == len(testRaw) {
				return nil
			}
			if time.Now().After(deadline) {
				t.Errorf("at commit the server had %d of %d body bytes", n, len(testRaw))
				return nil
			}
			time.Sleep(time.Millisecond)
		}
	}
	res, err := send(t, srv.config(), t.Context(), []string{"rcpt@example.com"}, testRaw, commit)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("commit called %d times", calls)
	}
	want := RecipientResult{Addr: "rcpt@example.com", Status: StatusAccepted, Code: 250, Text: "2.0.0 Ok: queued as ABC123"}
	if res.Outcome != OutcomeAccepted || !res.Committed || res.Recipients[0] != want {
		t.Errorf("result %+v", res)
	}
	if res.Final != (Reply{250, "2.0.0 Ok: queued as ABC123"}) || res.ProviderID != "ABC123" || res.MessageID != "" {
		t.Errorf("final %+v, provider id %q, message id %q", res.Final, res.ProviderID, res.MessageID)
	}

	s := srv.next(t)
	if s.auth != "\x00user\x00secret" {
		t.Errorf("AUTH PLAIN response %q", s.auth)
	}
	if want := "MAIL FROM:<sender@example.com> SIZE=" + strconv.Itoa(len(testRaw)); s.mail != want {
		t.Errorf("MAIL %q, want %q", s.mail, want)
	}
	if !s.dot || string(s.data) != testRaw {
		t.Errorf("server got dot=%v data %q", s.dot, s.data)
	}
}

func TestSendOutcomes(t *testing.T) {
	to := []string{"ok@example.com", "later@example.com", "bad@example.com"}
	rcptMix := map[string]string{
		"later@example.com": "450 4.2.1 Mailbox busy",
		"bad@example.com":   "550 5.1.1 No such user",
	}
	allRefused := map[string]string{
		"ok@example.com":    "451 4.3.0 Try again",
		"later@example.com": "450 4.2.1 Mailbox busy",
		"bad@example.com":   "550 5.1.1 No such user",
	}
	const (
		A = StatusAccepted
		D = StatusDeferred
		R = StatusRejected
		U = StatusUnknown
	)
	tests := []struct {
		name      string
		b         behavior
		timeout   time.Duration
		commitErr error
		outcome   Outcome
		status    []Status
		codes     []int
		committed bool
		commits   int32
		sawData   bool // the server got a DATA command
		dot       bool // the server got the end-of-data
	}{
		{
			name: "all accepted", outcome: OutcomeAccepted,
			status: []Status{A, A, A}, codes: []int{250, 250, 250},
			committed: true, commits: 1, sawData: true, dot: true,
		},
		{
			name: "greeting 421", b: behavior{greeting: "421 4.3.2 Service not available"},
			outcome: OutcomeFailed, status: []Status{D, D, D}, codes: []int{421, 421, 421},
		},
		{
			name: "greeting 554", b: behavior{greeting: "554 5.7.1 No SMTP service here"},
			outcome: OutcomeRejected, status: []Status{R, R, R}, codes: []int{554, 554, 554},
		},
		{
			name: "greeting never comes", b: behavior{greeting: "hang"}, timeout: 200 * time.Millisecond,
			outcome: OutcomeFailed, status: []Status{D, D, D}, codes: []int{0, 0, 0},
		},
		{
			name: "AUTH 535", b: behavior{auth: "535 5.7.8 Authentication credentials invalid"},
			outcome: OutcomeRejected, status: []Status{R, R, R}, codes: []int{535, 535, 535},
		},
		{
			name: "AUTH 454", b: behavior{auth: "454 4.7.0 Temporary authentication failure"},
			outcome: OutcomeFailed, status: []Status{D, D, D}, codes: []int{454, 454, 454},
		},
		{
			name: "AUTH not offered", b: behavior{ext: []string{"8BITMIME"}},
			outcome: OutcomeRejected, status: []Status{R, R, R}, codes: []int{0, 0, 0},
		},
		{
			name: "MAIL 451", b: behavior{mail: "451 4.3.0 Local error"},
			outcome: OutcomeFailed, status: []Status{D, D, D}, codes: []int{451, 451, 451},
		},
		{
			name: "RCPT mix", b: behavior{rcpt: rcptMix}, outcome: OutcomePartial,
			status: []Status{A, D, R}, codes: []int{250, 450, 550},
			committed: true, commits: 1, sawData: true, dot: true,
		},
		{
			name: "every RCPT refused", b: behavior{rcpt: allRefused}, outcome: OutcomeFailed,
			status: []Status{D, D, R}, codes: []int{451, 450, 550},
		},
		{
			name: "DATA 554", b: behavior{data: "554 5.5.1 No valid recipients"},
			outcome: OutcomeRejected, status: []Status{R, R, R}, codes: []int{554, 554, 554}, sawData: true,
		},
		{
			name: "commit fails", commitErr: errors.New("database unreachable"),
			outcome: OutcomeFailed, status: []Status{D, D, D}, codes: []int{0, 0, 0},
			commits: 1, sawData: true,
		},
		{
			name: "drop after the end-of-data", b: behavior{rcpt: rcptMix, final: "drop"},
			outcome: OutcomeUnknown, status: []Status{U, D, R}, codes: []int{0, 450, 550},
			committed: true, commits: 1, sawData: true, dot: true,
		},
		{
			name: "final reply too slow", b: behavior{final: "hang"}, timeout: 300 * time.Millisecond,
			outcome: OutcomeUnknown, status: []Status{U, U, U}, codes: []int{0, 0, 0},
			committed: true, commits: 1, sawData: true, dot: true,
		},
		{
			name: "final 451", b: behavior{rcpt: rcptMix, final: "451 4.3.0 Queue file write error"},
			outcome: OutcomeFailed, status: []Status{D, D, R}, codes: []int{451, 450, 550},
			committed: true, commits: 1, sawData: true, dot: true,
		},
		{
			name: "final 554", b: behavior{final: "554 5.7.1 Message rejected as spam"},
			outcome: OutcomeRejected, status: []Status{R, R, R}, codes: []int{554, 554, 554},
			committed: true, commits: 1, sawData: true, dot: true,
		},
		{
			name: "final reply garbled", b: behavior{final: "OK"},
			outcome: OutcomeUnknown, status: []Status{U, U, U}, codes: []int{0, 0, 0},
			committed: true, commits: 1, sawData: true, dot: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newServer(t, tt.b)
			ctx := t.Context()
			if tt.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.timeout)
				defer cancel()
			}
			c := &commits{err: tt.commitErr}
			res, err := send(t, srv.config(), ctx, to, testRaw, c.fn)
			if res.Outcome != tt.outcome {
				t.Errorf("outcome %v, want %v (err %v)", res.Outcome, tt.outcome, err)
			}
			var status []Status
			var codes []int
			for _, r := range res.Recipients {
				status = append(status, r.Status)
				codes = append(codes, r.Code)
			}
			if !slices.Equal(status, tt.status) || !slices.Equal(codes, tt.codes) {
				t.Errorf("recipients %+v, want statuses %v codes %v", res.Recipients, tt.status, tt.codes)
			}
			if res.Committed != tt.committed {
				t.Errorf("committed %v, want %v", res.Committed, tt.committed)
			}
			if n := c.n.Load(); n != tt.commits {
				t.Errorf("commit called %d times, want %d", n, tt.commits)
			}
			if tt.commitErr != nil && !errors.Is(err, tt.commitErr) {
				t.Errorf("error %v does not wrap the commit error", err)
			}
			// A session-level refusal is visible in the error too.
			if code := tt.codes[0]; code >= 400 && slices.Equal(tt.codes, []int{code, code, code}) {
				if te, ok := errors.AsType[*textproto.Error](err); !ok || te.Code != code {
					t.Errorf("error %v does not carry the %d reply", err, code)
				}
			}

			s := srv.next(t)
			if s.sawData != tt.sawData || s.dot != tt.dot {
				t.Errorf("server saw DATA=%v end-of-data=%v, want %v %v", s.sawData, s.dot, tt.sawData, tt.dot)
			}
			if tt.dot && string(s.data) != testRaw {
				t.Errorf("server got %q", s.data)
			}
			if tt.commitErr != nil && string(s.data) != testRaw {
				// The whole body was on the wire before commit; without the
				// end-of-data the server discards it.
				t.Errorf("before the abandon the server had %q", s.data)
			}
		})
	}
}

// TestSendCancelAfterCommit: cancelling after the commit point cannot
// prove anything was withheld, so the outcome is Unknown.
func TestSendCancelAfterCommit(t *testing.T) {
	srv := newServer(t, behavior{final: "hang"})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	commit := func(context.Context) error {
		cancel()
		return nil
	}
	start := time.Now()
	res, err := send(t, srv.config(), ctx, []string{"rcpt@example.com"}, testRaw, commit)
	if res.Outcome != OutcomeUnknown || !res.Committed || res.Recipients[0].Status != StatusUnknown {
		t.Errorf("result %+v, err %v", res, err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("cancellation took %v to interrupt the attempt", d)
	}
	srv.next(t)
}

func TestSendLocalChecks(t *testing.T) {
	big := testRaw + strings.Repeat(strings.Repeat("x", 76)+"\r\n", 20)
	tests := []struct {
		name    string
		b       behavior
		caps    Caps
		from    string
		to      string
		raw     string
		want    error
		connect bool // the check needs the server's EHLO
	}{
		{name: "bare LF", raw: "Subject: x\n\nbody\n", want: ErrInvalidMessage},
		{name: "Message-ID", raw: "Message-ID: <a@example.com>\r\n\r\nbody\r\n", want: ErrHeaderNotAllowed},
		{name: "DKIM-Signature", raw: "DKIM-Signature: v=1;\r\n b=x\r\n\r\nbody\r\n", want: ErrHeaderNotAllowed},
		{
			name: "Return-Path despite caps", caps: Caps{CallerMessageID: true, CallerDKIM: true, CallerReturnPath: true},
			raw: "Return-Path: <a@example.com>\r\n\r\nbody\r\n", want: ErrHeaderNotAllowed,
		},
		{name: "recipient with CRLF", to: "a@example.com>\r\nRCPT TO:<b@example.com", raw: testRaw, want: ErrInvalidEnvelope},
		{name: "too large", b: behavior{ext: []string{"AUTH PLAIN", "SIZE 100"}}, raw: big, want: ErrMessageTooLarge, connect: true},
		{name: "UTF-8 recipient without SMTPUTF8", to: "用户@example.com", raw: testRaw, want: ErrUnsupported, connect: true},
		{name: "UTF-8 header without SMTPUTF8", raw: "Subject: 你好\r\n\r\nbody\r\n", want: ErrUnsupported, connect: true},
		{
			name: "8-bit body without 8BITMIME", b: behavior{ext: []string{"AUTH PLAIN"}},
			raw: "Subject: x\r\n\r\ncafé\r\n", want: ErrUnsupported, connect: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newServer(t, tt.b)
			cfg := srv.config()
			cfg.Caps = tt.caps
			tr, err := NewSMTP(cfg)
			if err != nil {
				t.Fatal(err)
			}
			from, to := "sender@example.com", "rcpt@example.com"
			if tt.to != "" {
				to = tt.to
			}
			c := &commits{}
			res, err := tr.Send(t.Context(), Envelope{From: from, To: []string{to}}, []byte(tt.raw), c.fn)
			if !errors.Is(err, tt.want) {
				t.Errorf("error %v, want %v", err, tt.want)
			}
			if res.Outcome != OutcomeRejected || res.Recipients[0].Status != StatusRejected || res.Committed || c.n.Load() != 0 {
				t.Errorf("result %+v, commits %d", res, c.n.Load())
			}
			if tt.connect {
				if s := srv.next(t); s.mail != "" {
					t.Errorf("MAIL sent: %q", s.mail)
				}
			} else if n := srv.conns.Load(); n != 0 {
				t.Errorf("%d connections for a message that could not be sent", n)
			}
		})
	}
}

func TestSendExtensions(t *testing.T) {
	srv := newServer(t, behavior{ext: []string{"AUTH PLAIN", "8BITMIME", "SMTPUTF8", "SIZE"}})
	// No trailing CRLF: the end-of-data supplies it.
	raw := "Subject: 你好\r\n\r\ncafé"
	tr, err := NewSMTP(srv.config())
	if err != nil {
		t.Fatal(err)
	}
	c := &commits{}
	res, err := tr.Send(t.Context(), Envelope{From: "", To: []string{"用户@example.com"}}, []byte(raw), c.fn)
	if err != nil || res.Outcome != OutcomeAccepted {
		t.Fatalf("result %+v, err %v", res, err)
	}
	s := srv.next(t)
	want := "MAIL FROM:<> SMTPUTF8 BODY=8BITMIME SIZE=" + strconv.Itoa(len(raw)+2)
	if s.mail != want {
		t.Errorf("MAIL %q, want %q", s.mail, want)
	}
	if !slices.Equal(s.rcpts, []string{"用户@example.com"}) {
		t.Errorf("RCPT %q", s.rcpts)
	}
	if string(s.data) != raw+"\r\n" {
		t.Errorf("server got %q", s.data)
	}
}

func TestSendNoAuth(t *testing.T) {
	srv := newServer(t, behavior{ext: []string{"8BITMIME"}})
	cfg := srv.config()
	cfg.Username, cfg.Password = "", ""
	res, err := send(t, cfg, t.Context(), []string{"rcpt@example.com"}, testRaw, (&commits{}).fn)
	if err != nil || res.Outcome != OutcomeAccepted {
		t.Fatalf("result %+v, err %v", res, err)
	}
	if s := srv.next(t); s.auth != "" {
		t.Errorf("AUTH sent without credentials: %q", s.auth)
	}
}

// TestSendVerifiesCertificate: without the test CA the default
// configuration must refuse the server, before sending credentials.
func TestSendVerifiesCertificate(t *testing.T) {
	srv := newServer(t, behavior{})
	cfg := srv.config()
	cfg.RootCAs = nil
	res, err := send(t, cfg, t.Context(), []string{"rcpt@example.com"}, testRaw, (&commits{}).fn)
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); !ok {
		t.Errorf("error %v, want a certificate verification error", err)
	}
	if res.Outcome != OutcomeFailed || res.Committed {
		t.Errorf("result %+v", res)
	}
	if s := srv.next(t); s.auth != "" || s.mail != "" {
		t.Errorf("server session got %+v", s)
	}
}

func TestSendConnectionRefused(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	cfg := Config{Host: "127.0.0.1", Port: port}
	res, err := send(t, cfg, t.Context(), []string{"rcpt@example.com"}, testRaw, (&commits{}).fn)
	if err == nil || res.Outcome != OutcomeFailed || res.Recipients[0].Status != StatusDeferred {
		t.Errorf("result %+v, err %v", res, err)
	}
}

func TestNewSMTP(t *testing.T) {
	if _, err := NewSMTP(Config{}); err == nil {
		t.Error("accepted a config without host")
	}
	if _, err := NewSMTP(Config{Host: "smtp.example.com", Password: "x"}); err == nil {
		t.Error("accepted a password without username")
	}
	tr, err := NewSMTP(Cloudflare("user", "token"))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Name() != "cloudflare" || tr.cfg.Port != 465 || tr.Caps() != (Caps{}) {
		t.Errorf("cloudflare config %+v", tr.cfg)
	}
}

func TestParseProviderID(t *testing.T) {
	tests := map[string]string{
		"2.0.0 Ok: queued as 4BxYz1RtQ2z9": "4BxYz1RtQ2z9",
		"2.0.0 OK: QUEUED AS abc def":      "abc",
		"2.0.0 Ok: queued as ":             "",
		"2.0.0 OK 1712345678 x12si":        "",
		"Ok 010001900000000-0000-0000":     "",
		"":                                 "",
	}
	for text, want := range tests {
		if got := parseProviderID(text); got != want {
			t.Errorf("parseProviderID(%q) = %q, want %q", text, got, want)
		}
	}
}
