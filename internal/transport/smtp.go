package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout bounds an attempt whose context has no deadline, so a
// server that never answers cannot hold an attempt forever. It matches the
// longest timeout RFC 5321 §4.5.3.2 suggests (waiting for the reply to the
// end of the data).
const DefaultTimeout = 10 * time.Minute

// CloudflareHost is Cloudflare Email Sending's SMTP endpoint.
const CloudflareHost = "smtp.mx.cloudflare.net"

// Config is one SMTP exit. Exits differ only in configuration (§10.2).
type Config struct {
	// Name identifies the exit in send_attempts.transport.
	Name string
	// Host is the server name, used to dial, to verify its certificate and
	// for AUTH.
	Host string
	// Port defaults to 465. The connection is always implicit TLS (RFC 8314);
	// there is no STARTTLS and no plaintext mode.
	Port int
	// Username and Password enable AUTH PLAIN. Empty Username means no AUTH.
	Username string
	Password string
	// HeloName is sent in EHLO; empty means net/smtp's default, "localhost".
	HeloName string
	Caps     Caps
	// RootCAs verifies the server certificate; nil means the system roots.
	// Tests point it at their own CA. There is deliberately no way to skip
	// verification.
	RootCAs *x509.CertPool
}

// Cloudflare returns the configuration of Cloudflare Email Sending.
func Cloudflare(username, password string) Config {
	return Config{
		Name:     "cloudflare",
		Host:     CloudflareHost,
		Port:     465,
		Username: username,
		Password: password,
		Caps:     CloudflareCaps(),
	}
}

// SMTP delivers raw MIME over SMTP submission with implicit TLS. One value
// serves concurrent Sends; each opens its own connection.
type SMTP struct {
	cfg Config
}

// NewSMTP checks cfg and returns a transport for it.
func NewSMTP(cfg Config) (*SMTP, error) {
	if cfg.Host == "" {
		return nil, errors.New("transport: no host")
	}
	if cfg.Port == 0 {
		cfg.Port = 465
	}
	if cfg.Name == "" {
		cfg.Name = cfg.Host
	}
	if cfg.Password != "" && cfg.Username == "" {
		return nil, errors.New("transport: password without username")
	}
	return &SMTP{cfg: cfg}, nil
}

// Name returns the configured exit name.
func (t *SMTP) Name() string { return t.cfg.Name }

// Caps returns the configured caps.
func (t *SMTP) Caps() Caps { return t.cfg.Caps }

// Send delivers raw to env in one SMTP transaction.
//
// raw must already be what this exit may receive: CRLF lines of at most 998
// octets, without the header fields StripHeaders removes. Send checks and
// fails early rather than repairing anything.
//
// commit is the point of no return (§4.5). Send calls it once, after every
// byte of raw has been flushed to the server and before the end-of-data
// sequence <CRLF>.<CRLF>; only the end-of-data makes the server act on the
// message, so up to that moment closing the connection discards it
// (RFC 5321 §4.1.1.4). If commit returns an error, Send closes the
// connection without sending the end-of-data and reports OutcomeFailed with
// Committed false. Send does not call commit when no recipient was accepted
// or the transaction failed earlier.
//
// Send always returns a result. The error is nil for OutcomeAccepted and
// OutcomePartial and explains the failure otherwise. The context's deadline
// (or DefaultTimeout) bounds the whole attempt; cancelling it after commit
// yields OutcomeUnknown.
func (t *SMTP) Send(ctx context.Context, env Envelope, raw []byte, commit func(context.Context) error) (*SendResult, error) {
	a := &attempt{res: &SendResult{Recipients: make([]RecipientResult, len(env.To))}}
	for i, to := range env.To {
		a.res.Recipients[i].Addr = to
	}
	if commit == nil {
		return a.local(errors.New("nil commit"))
	}
	if err := checkEnvelope(env); err != nil {
		return a.local(err)
	}
	msg, err := inspect(raw)
	if err != nil {
		return a.local(err)
	}
	if err := checkHeaders(raw, t.cfg.Caps); err != nil {
		return a.local(err)
	}

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}
	deadline, _ := ctx.Deadline()

	addr := net.JoinHostPort(t.cfg.Host, strconv.Itoa(t.cfg.Port))
	d := &tls.Dialer{Config: &tls.Config{
		ServerName: t.cfg.Host,
		RootCAs:    t.cfg.RootCAs,
		MinVersion: tls.VersionTLS12,
	}}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return a.fail("connect", err)
	}
	// This Close is also how an attempt is abandoned: closing while the data
	// is still open makes the server discard the message.
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(deadline); err != nil {
		return a.fail("connect", err)
	}
	// Cancellation interrupts blocked I/O by moving the deadline into the
	// past. After commit that surfaces as an I/O error, hence Unknown.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	c, err := smtp.NewClient(conn, t.cfg.Host)
	if err != nil {
		return a.fail("greeting", err)
	}
	// dataOpen is true while a DotWriter is open. Then nothing else may be
	// written: textproto closes an open DotWriter, i.e. sends the
	// end-of-data, before writing any command, QUIT included.
	dataOpen := false
	defer func() {
		if !dataOpen {
			// Polite and best effort: send QUIT, do not wait for 221.
			_ = c.Text.PrintfLine("QUIT")
		}
	}()

	helo := t.cfg.HeloName
	if helo == "" {
		helo = "localhost"
	}
	if err := c.Hello(helo); err != nil {
		return a.fail("EHLO", err)
	}
	if t.cfg.Username != "" {
		_, mechs := c.Extension("AUTH")
		if !slices.ContainsFunc(strings.Fields(mechs), func(m string) bool { return strings.EqualFold(m, "PLAIN") }) {
			return a.local(fmt.Errorf("%w: AUTH PLAIN", ErrUnsupported))
		}
		if err := c.Auth(smtp.PlainAuth("", t.cfg.Username, t.cfg.Password, t.cfg.Host)); err != nil {
			return a.fail("AUTH", err)
		}
	}

	params := ""
	if msg.header8bit || env.needsUTF8() {
		if ok, _ := c.Extension("SMTPUTF8"); !ok {
			return a.local(fmt.Errorf("%w: SMTPUTF8 (non-ASCII address or header)", ErrUnsupported))
		}
		params += " SMTPUTF8"
	}
	if msg.header8bit || msg.body8bit {
		if ok, _ := c.Extension("8BITMIME"); !ok {
			return a.local(fmt.Errorf("%w: 8BITMIME (non-ASCII content)", ErrUnsupported))
		}
		params += " BODY=8BITMIME"
	}
	if ok, limit := c.Extension("SIZE"); ok {
		// "SIZE" alone or "SIZE 0" announces no fixed limit (RFC 1870).
		if n, err := strconv.Atoi(strings.TrimSpace(limit)); err == nil && n > 0 && msg.size > n {
			return a.local(fmt.Errorf("%w: %d octets, limit %d", ErrMessageTooLarge, msg.size, n))
		}
		params += " SIZE=" + strconv.Itoa(msg.size)
	}

	r, err := cmd(c.Text, "MAIL FROM:<%s>%s", env.From, params)
	if err != nil {
		return a.fail("MAIL", err)
	}
	if r.Code/100 != 2 {
		return a.refuse("MAIL", r)
	}

	accepted := 0
	for i, to := range env.To {
		r, err := cmd(c.Text, "RCPT TO:<%s>", to)
		if err != nil {
			return a.fail("RCPT", err)
		}
		rr := &a.res.Recipients[i]
		rr.Code, rr.Text = r.Code, r.Text
		switch r.Code / 100 {
		case 2:
			accepted++ // stays pending: the final reply decides
		case 5:
			rr.Status = StatusRejected
		default:
			rr.Status = StatusDeferred
		}
	}
	if accepted == 0 {
		return a.finish(errors.New("transport: RCPT: no recipient accepted"))
	}

	r, err = cmd(c.Text, "DATA")
	if err != nil {
		return a.fail("DATA", err)
	}
	if r.Code != 354 {
		return a.refuse("DATA", r)
	}

	// net/textproto's DotWriter writes into the connection's bufio.Writer and
	// emits the end-of-data only in Close, so after Write and Flush every
	// body byte is on the wire and the terminator is not. Flushing before
	// commit also surfaces a broken connection while it is still a safe
	// failure.
	dataOpen = true
	w := c.Text.DotWriter()
	if _, err := w.Write(raw); err != nil {
		return a.fail("DATA", err)
	}
	if err := c.Text.W.Flush(); err != nil {
		return a.fail("DATA", err)
	}
	if err := commit(ctx); err != nil {
		// Return with the DotWriter open: the deferred conn.Close drops the
		// connection mid-data, and the server discards the message.
		err = fmt.Errorf("transport: commit: %w", err)
		a.settle(StatusDeferred, Reply{Text: "not sent: " + err.Error()})
		return a.finish(err)
	}
	a.res.Committed = true

	if err := w.Close(); err != nil {
		return a.fail("end of data", err)
	}
	dataOpen = false
	code, text, err := c.Text.ReadResponse(0)
	if err != nil {
		return a.fail("final reply", err)
	}
	final := Reply{Code: code, Text: text}
	a.res.Final = final
	switch code / 100 {
	case 2:
		a.settle(StatusAccepted, final)
		a.res.ProviderID = parseProviderID(text)
		if !t.cfg.Caps.CallerMessageID {
			// The exit wrote its own Message-ID (§10.4.1). An exit that
			// keeps ours needs no report, and a reply that merely looks
			// like one must not override the id we sent.
			a.res.MessageID = reportedMessageID(text)
		}
		return a.finish(nil)
	case 4:
		a.settle(StatusDeferred, final)
	case 5:
		a.settle(StatusRejected, final)
	default:
		a.settle(StatusUnknown, final) // not a valid answer to the end of data
	}
	return a.finish(fmt.Errorf("transport: final reply: %w", &textproto.Error{Code: code, Msg: text}))
}

// cmd sends one command and reads its reply without judging the code, so
// every reply is classified in one place. err is an I/O or protocol error.
func cmd(t *textproto.Conn, format string, args ...any) (Reply, error) {
	id, err := t.Cmd(format, args...)
	if err != nil {
		return Reply{}, err
	}
	t.StartResponse(id)
	defer t.EndResponse(id)
	code, text, err := t.ReadResponse(0)
	return Reply{Code: code, Text: text}, err
}

// attempt accumulates the result of one Send.
type attempt struct {
	res *SendResult
}

// settle decides every recipient that is still pending.
func (a *attempt) settle(s Status, r Reply) {
	for i := range a.res.Recipients {
		if rr := &a.res.Recipients[i]; rr.Status == statusPending {
			rr.Status, rr.Code, rr.Text = s, r.Code, r.Text
		}
	}
}

func (a *attempt) finish(err error) (*SendResult, error) {
	a.res.Outcome = summarize(a.res.Recipients)
	return a.res, err
}

// local fails the attempt on a check of our own: permanent, no reply code.
func (a *attempt) local(err error) (*SendResult, error) {
	err = fmt.Errorf("transport: %w", err)
	a.settle(StatusRejected, Reply{Text: err.Error()})
	return a.finish(err)
}

// refuse fails the attempt on a server reply before the commit point.
func (a *attempt) refuse(stage string, r Reply) (*SendResult, error) {
	s := StatusDeferred
	if r.Code/100 == 5 {
		s = StatusRejected
	}
	a.settle(s, r)
	return a.finish(fmt.Errorf("transport: %s: %w", stage, &textproto.Error{Code: r.Code, Msg: r.Text}))
}

// fail fails the attempt on an error from net/smtp or the connection.
// Before the commit point nothing was delivered: a reply decides by its
// code, anything else is temporary. After it, the outcome is unknown.
func (a *attempt) fail(stage string, err error) (*SendResult, error) {
	if !a.res.Committed {
		if te, ok := errors.AsType[*textproto.Error](err); ok {
			return a.refuse(stage, Reply{Code: te.Code, Text: te.Msg})
		}
	}
	err = fmt.Errorf("transport: %s: %w", stage, err)
	if a.res.Committed {
		a.settle(StatusUnknown, Reply{Text: "after commit: " + err.Error()})
	} else {
		a.settle(StatusDeferred, Reply{Text: err.Error()})
	}
	return a.finish(err)
}

// okMsgID is the whole final reply of Cloudflare Email Sending: an enhanced
// status code, "Ok" and one msg-id in angle brackets. That id is the
// Message-ID header the exit wrote into the delivered message (measured
// 2026-10-31, §10.4.1). Only the complete form matches, so a free-text
// reply that happens to contain brackets is not mistaken for an id.
var okMsgID = regexp.MustCompile(`^(?:[245]\.[0-9]{1,3}\.[0-9]{1,3} )?(?i:ok) <([^<>@\s]+@[^<>@\s]+)>$`)

// reportedMessageID returns the Message-ID an exit reports in its final
// reply, without angle brackets, or "" when the reply has no such form.
func reportedMessageID(text string) string {
	if m := okMsgID.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}

// parseProviderID extracts the exit's id for the message from the reply
// forms that are unambiguous: Postfix's "queued as <id>", and Cloudflare's
// "Ok <msg-id>", whose id is returned without the angle brackets. Any other
// form yields "", and the verbatim reply in SendResult.Final remains for
// study (§10.4.1).
func parseProviderID(text string) string {
	if id := reportedMessageID(text); id != "" {
		return id
	}
	const marker = "queued as "
	for i := 0; i+len(marker) <= len(text); i++ {
		if strings.EqualFold(text[i:i+len(marker)], marker) {
			if f := strings.Fields(text[i+len(marker):]); len(f) > 0 {
				return f[0]
			}
			return ""
		}
	}
	return ""
}
