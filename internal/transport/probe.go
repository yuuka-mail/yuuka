package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ProbeTimeout bounds a Probe whose context has no deadline: a probe never
// waits for a message to be processed, so a server that is slower than
// this to greet and authenticate is as good as down.
const ProbeTimeout = 30 * time.Second

// Probe checks that the exit can be used, without sending anything: it
// connects with implicit TLS and verifies the certificate, reads the
// greeting, says EHLO, authenticates with AUTH PLAIN when a username is
// configured, and quits. It never sends MAIL, so the server hears nothing
// about a message and nothing can be delivered (`yuuka transport test`,
// §17.6: after a password change there is no need for a real message).
//
// A failure is an *Error naming the stage, the same stages and the same
// ExitFailure classification Send uses.
func (t *SMTP) Probe(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ProbeTimeout)
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
		return &Error{Stage: StageConnect, Err: err}
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(deadline); err != nil {
		return &Error{Stage: StageConnect, Err: err}
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	c, err := smtp.NewClient(conn, t.cfg.Host)
	if err != nil {
		return &Error{Stage: StageGreeting, Err: err}
	}
	// Best effort, as in Send: whether the server answers QUIT says
	// nothing about the exit.
	defer func() { _ = c.Text.PrintfLine("QUIT") }()

	helo := t.cfg.HeloName
	if helo == "" {
		helo = "localhost"
	}
	if err := c.Hello(helo); err != nil {
		return &Error{Stage: StageEHLO, Err: err}
	}
	if t.cfg.Username == "" {
		return nil
	}
	_, mechs := c.Extension("AUTH")
	if !slices.ContainsFunc(strings.Fields(mechs), func(m string) bool { return strings.EqualFold(m, "PLAIN") }) {
		return &Error{Stage: StageAuth, Err: fmt.Errorf("%w: AUTH PLAIN", ErrUnsupported)}
	}
	if err := c.Auth(smtp.PlainAuth("", t.cfg.Username, t.cfg.Password, t.cfg.Host)); err != nil {
		return &Error{Stage: StageAuth, Err: err}
	}
	return nil
}
