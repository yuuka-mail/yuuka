package imap

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultIdleTimeout is how long a read or a write may wait without
// progress when Config.IdleTimeout is zero. A command as a whole is
// bounded by its context; this catches a connection that died silently
// in the middle of a long download.
const DefaultIdleTimeout = 2 * time.Minute

// Config says where to connect.
type Config struct {
	// Host is the server name, used to dial and to verify the certificate.
	Host string
	// Port defaults to 993. The connection is always implicit TLS.
	Port int
	// RootCAs verifies the server certificate; nil means the system roots.
	// Tests point it at their own CA. There is deliberately no way to skip
	// verification.
	RootCAs *x509.CertPool
	// IdleTimeout bounds each read and write; zero means
	// DefaultIdleTimeout.
	IdleTimeout time.Duration
}

// Caps is the server's capability list as it last announced it.
type Caps []string

// Has reports whether the capability is listed, ignoring case. A SASL
// mechanism is listed as "AUTH=PLAIN".
func (c Caps) Has(name string) bool {
	for _, x := range c {
		if strings.EqualFold(x, name) {
			return true
		}
	}
	return false
}

// Client is one connection to an IMAP server.
type Client struct {
	conn *deadlineConn
	rd   *reader
	bw   *bufio.Writer
	tag  int

	caps     Caps
	greeting *StatusResponse
	// bye is the server's BYE, kept to explain the end of the connection.
	bye *StatusResponse
	// alerts are [ALERT] texts not yet taken by Alerts.
	alerts []string
	// err is set once the connection is unusable; every later command
	// returns it.
	err error
}

// Dial connects with implicit TLS and reads the server's greeting. The
// context bounds the connection and the greeting only.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Host == "" {
		return nil, &Error{Kind: KindRefused, Op: "connect", Err: errors.New("no host")}
	}
	port := cfg.Port
	if port == 0 {
		port = 993
	}
	d := &tls.Dialer{Config: &tls.Config{
		ServerName: cfg.Host,
		RootCAs:    cfg.RootCAs,
		MinVersion: tls.VersionTLS12,
	}}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(port)))
	if err != nil {
		return nil, &Error{Kind: KindConnection, Op: "connect", Err: ctxErr(ctx, err)}
	}
	return newClient(ctx, conn, cfg.IdleTimeout)
}

// newClient reads the greeting on an established connection.
func newClient(ctx context.Context, conn net.Conn, idle time.Duration) (*Client, error) {
	if idle <= 0 {
		idle = DefaultIdleTimeout
	}
	dc := &deadlineConn{Conn: conn, idle: idle}
	c := &Client{conn: dc, rd: newReader(dc), bw: bufio.NewWriter(dc)}
	done, err := c.begin(ctx, "greeting")
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	defer done()
	resp, err := c.rd.readResponse(smallLiteral)
	if err != nil {
		return nil, c.fail(ctx, "greeting", err)
	}
	if resp.tag != "*" || resp.status == nil {
		return nil, c.fail(ctx, "greeting", syntaxf("greeting is not a status response"))
	}
	c.greeting = resp.status
	switch resp.status.Type {
	case "OK", "PREAUTH":
		c.untagged(resp)
		return c, nil
	case "BYE":
		c.bye = resp.status
		return nil, c.fail(ctx, "greeting", errors.New("server refused the connection"))
	}
	return nil, c.fail(ctx, "greeting", syntaxf("greeting is %s", resp.status.Type))
}

// Caps returns the capabilities from the greeting or the last
// CAPABILITY. They change after login; Login and the Authenticate methods
// refresh them.
func (c *Client) Caps() Caps { return c.caps }

// Greeting returns the server's greeting. Its Type is PREAUTH when the
// connection is already authenticated.
func (c *Client) Greeting() *StatusResponse { return c.greeting }

// Alerts returns and forgets the [ALERT] texts the server has sent. RFC
// 3501 §7.1 asks that they reach a person; they are server text, for
// display and logs only.
func (c *Client) Alerts() []string {
	a := c.alerts
	c.alerts = nil
	return a
}

// Close closes the connection without logging out.
func (c *Client) Close() error {
	if c.err == nil {
		c.err = &Error{Kind: KindConnection, Op: "close", Err: net.ErrClosed}
	}
	return c.conn.Close()
}

// handler sees the untagged responses of one command.
type handler func(*response) error

// exec runs one command: it sends cmd, passes each untagged response to
// fn and each continuation request to cont, and returns the tagged
// completion. A NO or BAD is returned as an *Error classified for op;
// auth marks LOGIN and AUTHENTICATE. maxLit bounds the literals of each
// response.
//
// An error from fn or cont, a broken stream and a cancelled context all
// close the connection, since the command is then in an unknown state.
func (c *Client) exec(ctx context.Context, op, cmd string, auth bool, maxLit int64, fn handler, cont func(text string) string) (*StatusResponse, error) {
	if c.err != nil {
		return nil, c.err
	}
	done, err := c.begin(ctx, op)
	if err != nil {
		return nil, err
	}
	defer done()

	c.tag++
	tag := "A" + strconv.Itoa(c.tag)
	if err := c.send(tag + " " + cmd); err != nil {
		return nil, c.fail(ctx, op, err)
	}
	for {
		resp, err := c.rd.readResponse(maxLit)
		if err != nil {
			return nil, c.fail(ctx, op, err)
		}
		switch resp.tag {
		case "*":
			c.untagged(resp)
			if fn != nil {
				if err := fn(resp); err != nil {
					return nil, c.fail(ctx, op, err)
				}
			}
		case "+":
			if cont == nil {
				return nil, c.fail(ctx, op, syntaxf("unexpected continuation request"))
			}
			if err := c.send(cont(resp.text)); err != nil {
				return nil, c.fail(ctx, op, err)
			}
		case tag:
			s := resp.status
			if s.Code == "CAPABILITY" {
				c.caps = Caps(strings.Fields(s.Arg))
			}
			c.alert(s)
			switch s.Type {
			case "OK":
				return s, nil
			case "NO", "BAD":
				return s, classify(op, s, auth)
			}
			return nil, c.fail(ctx, op, syntaxf("tagged %s", s.Type))
		default:
			return nil, c.fail(ctx, op, syntaxf("response for unknown tag %q", resp.tag))
		}
	}
}

// untagged keeps the state any untagged response may carry.
func (c *Client) untagged(resp *response) {
	if s := resp.status; s != nil {
		switch {
		case s.Type == "BYE":
			c.bye = s
		case s.Code == "CAPABILITY":
			c.caps = Caps(strings.Fields(s.Arg))
		}
		c.alert(s)
		return
	}
	if resp.name == "CAPABILITY" && !resp.hasNum {
		caps := make(Caps, 0, len(resp.fields))
		for _, v := range resp.fields {
			if t, ok := v.Text(); ok {
				caps = append(caps, t)
			}
		}
		c.caps = caps
	}
}

func (c *Client) alert(s *StatusResponse) {
	// A bound, because the server decides how many it sends.
	if s.Code == "ALERT" && len(c.alerts) < 16 {
		c.alerts = append(c.alerts, s.Text)
	}
}

func (c *Client) send(line string) error {
	if _, err := c.bw.WriteString(line + "\r\n"); err != nil {
		return err
	}
	return c.bw.Flush()
}

// fail closes the connection and records why. err is a cause from the
// stream, the parser, a handler or a local check.
func (c *Client) fail(ctx context.Context, op string, err error) error {
	_ = c.conn.Close()
	var e *Error
	var syn *errSyntax
	var big *errTooLarge
	var ce *callerError
	switch {
	case errors.As(err, &ce):
		// The caller's own error goes back to the caller unchanged.
		c.err = &Error{Kind: KindConnection, Op: op, Err: errors.New("closed after the caller stopped a command")}
		return ce.err
	case errors.As(err, &syn):
		e = &Error{Kind: KindProtocol, Op: op, Err: err}
	case errors.As(err, &big):
		e = &Error{Kind: KindTooLarge, Op: op, Err: err}
	case c.bye != nil:
		// The server said goodbye and hung up: the BYE explains it.
		e = classify(op, c.bye, false)
		e.Err = err
	default:
		e = &Error{Kind: KindConnection, Op: op, Err: ctxErr(ctx, err)}
	}
	c.err = e
	return e
}

// callerError carries an error of the caller's callback through exec.
type callerError struct{ err error }

func (e *callerError) Error() string { return e.err.Error() }

// begin arms the deadlines of one command and returns the function that
// disarms them.
func (c *Client) begin(ctx context.Context, op string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, &Error{Kind: KindConnection, Op: op, Err: err}
	}
	limit, _ := ctx.Deadline()
	c.conn.start(limit)
	stop := context.AfterFunc(ctx, c.conn.cancel)
	return func() {
		if !stop() {
			// cancel has started: wait for it to finish, so that it
			// cannot reach into the next command.
			c.conn.mu.Lock()
			c.conn.mu.Unlock() //nolint:staticcheck // SA2001: the empty section is the wait
		}
	}, nil
}

// ctxErr reports a failure caused by the context as the context's error,
// so that callers can tell cancellation from a network fault.
func ctxErr(ctx context.Context, err error) error {
	cerr := ctx.Err()
	if dl, ok := ctx.Deadline(); ok && cerr == nil && !time.Now().Before(dl) {
		// The connection's deadline, set to the context's, can fire a
		// moment before the context's own timer.
		cerr = context.DeadlineExceeded
	}
	if cerr != nil {
		return fmt.Errorf("%w (%w)", cerr, err)
	}
	return err
}

// deadlineConn sets the deadline before every read and write: the idle
// timeout from now, but never past the command's context deadline.
type deadlineConn struct {
	net.Conn
	idle time.Duration

	mu        sync.Mutex
	limit     time.Time
	cancelled bool
}

func (d *deadlineConn) start(limit time.Time) {
	d.mu.Lock()
	d.limit, d.cancelled = limit, false
	d.mu.Unlock()
}

// cancel interrupts blocked I/O by moving the deadline into the past, and
// keeps it there: arm must not move it forward again.
func (d *deadlineConn) cancel() {
	d.mu.Lock()
	d.cancelled = true
	_ = d.SetDeadline(time.Unix(1, 0))
	d.mu.Unlock()
}

func (d *deadlineConn) arm() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cancelled {
		return context.Canceled
	}
	dl := time.Now().Add(d.idle)
	if !d.limit.IsZero() && d.limit.Before(dl) {
		dl = d.limit
	}
	return d.SetDeadline(dl)
}

func (d *deadlineConn) Read(p []byte) (int, error) {
	if err := d.arm(); err != nil {
		return 0, err
	}
	return d.Conn.Read(p)
}

func (d *deadlineConn) Write(p []byte) (int, error) {
	if err := d.arm(); err != nil {
		return 0, err
	}
	return d.Conn.Write(p)
}
