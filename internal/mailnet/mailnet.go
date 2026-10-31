// Package mailnet is the network edge of yuuka's mail-protocol listeners
// (SMTP submission now, perhaps IMAP later; §13.6). It wraps a TCP
// listener so that what Accept returns has already been vetted:
//
//   - Implicit TLS only. With a tls.Config, yuuka terminates TLS itself;
//     without one, a proxy in front has terminated it and plaintext is
//     accepted from that proxy alone. No connection from anyone else is
//     ever served in plaintext, so credentials never cross the wire bare.
//   - The PROXY protocol (v1 and v2) is required from the configured
//     proxies and refused from everyone else, so the address a server
//     rate-limits and audits is the real client's and nobody can choose
//     their own.
//
// The PROXY header and the TLS handshake are done in a goroutine per
// connection, under a deadline, before Accept hands the connection out:
// a silent peer costs one goroutine until the deadline and never delays
// another client's Accept, and a server sees only connections that
// passed. The behaviour for every combination is the table in §13.6.
package mailnet

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/yuuka-mail/yuuka/internal/clientip"
)

// DefaultHandshakeTimeout bounds the PROXY header and TLS handshake when
// Config.HandshakeTimeout is zero.
const DefaultHandshakeTimeout = 10 * time.Second

// Reasons a connection is turned away before Accept returns it, passed
// to Config.Rejected. Malformed headers, timeouts and TLS failures are
// reported with their own errors.
var (
	// ErrUntrustedHeader: a peer that is not a configured proxy sent
	// what looks like a PROXY header, i.e. tried to name its own address.
	ErrUntrustedHeader = errors.New("mailnet: PROXY header from a peer that is not a trusted proxy")
	// ErrMissingHeader: a configured proxy connected without a PROXY
	// header. Accepting it would attribute its client to the proxy.
	ErrMissingHeader = errors.New("mailnet: trusted proxy sent no PROXY header")
	// ErrPlaintext: a peer that is not a configured proxy connected to a
	// listener whose TLS is terminated in front of yuuka, or sent
	// something other than a TLS handshake to one that terminates it.
	ErrPlaintext = errors.New("mailnet: plaintext connection from a peer that is not a trusted proxy")
)

// tlsHandshake is the first byte of a TLS record carrying a handshake,
// which is how every implicit-TLS client opens.
const tlsHandshake = 0x16

// bufSize must hold the largest v2 header, which readV2 peeks whole.
const bufSize = 4096

var _ [bufSize - v2HeaderLen - v2MaxBody]struct{} // bufSize is big enough

// Config says how a Listener vets connections.
type Config struct {
	// TLS, if set, makes yuuka terminate TLS itself, after any PROXY
	// header. If nil, TLS is terminated by a proxy in front and only
	// Proxies may connect; NewListener refuses that with no Proxies.
	TLS *tls.Config

	// Proxies are the peers that must send a PROXY header and are
	// believed when they do. This is deliberately a separate set from
	// the HTTP one (YUUKA_TRUSTED_PROXIES): see §13.6.
	Proxies clientip.Proxies

	// HandshakeTimeout bounds reading the PROXY header and the TLS
	// handshake together. Zero means DefaultHandshakeTimeout.
	HandshakeTimeout time.Duration

	// Rejected, if set, is called with the peer's address (the TCP peer,
	// not anything a header claimed) and the reason whenever a
	// connection is closed before Accept returned it. It is called from
	// the connection's own goroutine, possibly concurrently.
	Rejected func(peer net.Addr, err error)
}

// Listener is a net.Listener whose Accept returns only vetted
// connections. When Config.TLS is set they are *tls.Conn with the
// handshake done, which lets a server such as go-smtp see the TLS state;
// otherwise they are *Conn. Either way RemoteAddr is the real client's
// address and Proxy reports the proxy it came through.
type Listener struct {
	inner    net.Listener
	tls      *tls.Config
	proxies  clientip.Proxies
	timeout  time.Duration
	rejected func(net.Addr, error)

	ready chan net.Conn
	errc  chan error
	// ctx is cancelled by Close, which aborts handshakes in progress.
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
}

// NewListener wraps inner, which should be a TCP listener; it starts
// accepting from inner right away. Closing the Listener closes inner.
func NewListener(inner net.Listener, cfg Config) (*Listener, error) {
	if cfg.TLS == nil && cfg.Proxies.Empty() {
		return nil, errors.New("mailnet: without a TLS config a trusted proxy must terminate TLS, but none is configured")
	}
	l := &Listener{
		inner:    inner,
		tls:      cfg.TLS,
		proxies:  cfg.Proxies,
		timeout:  cfg.HandshakeTimeout,
		rejected: cfg.Rejected,
		ready:    make(chan net.Conn),
		errc:     make(chan error),
	}
	l.ctx, l.cancel = context.WithCancel(context.Background())
	if l.timeout <= 0 {
		l.timeout = DefaultHandshakeTimeout
	}
	go l.acceptLoop()
	return l, nil
}

// acceptLoop takes raw connections off inner and vets each in its own
// goroutine. An error from inner is handed to exactly one Accept call
// before inner is tried again, so the caller's retry policy (go-smtp and
// net/http back off on temporary errors) still paces the loop.
func (l *Listener) acceptLoop() {
	for {
		raw, err := l.inner.Accept()
		if err != nil {
			select {
			case l.errc <- err:
				continue
			case <-l.ctx.Done():
				return
			}
		}
		go l.vet(raw)
	}
}

func (l *Listener) vet(raw net.Conn) {
	// Close interrupts whatever read is in progress by moving the
	// deadline into the past; the header parser knows no context.
	stop := context.AfterFunc(l.ctx, func() { _ = raw.SetDeadline(time.Unix(1, 0)) })
	c, err := l.handshake(raw)
	if !stop() {
		err = net.ErrClosed
	}
	if err == nil {
		err = raw.SetDeadline(time.Time{})
	}
	if err != nil {
		_ = raw.Close()
		if l.rejected != nil && l.ctx.Err() == nil {
			l.rejected(raw.RemoteAddr(), err)
		}
		return
	}
	select {
	case l.ready <- c:
	case <-l.ctx.Done():
		_ = c.Close()
	}
}

// handshake applies the policy in §13.6's table to one raw connection.
// It leaves the deadline set; vet clears it.
func (l *Listener) handshake(raw net.Conn) (net.Conn, error) {
	if err := raw.SetDeadline(time.Now().Add(l.timeout)); err != nil {
		return nil, err
	}
	c := &Conn{Conn: raw, r: bufio.NewReaderSize(raw, bufSize), remote: raw.RemoteAddr()}
	switch {
	case l.trusts(raw.RemoteAddr()):
		h, err := readHeader(c.r)
		if err != nil {
			return nil, err
		}
		c.proxy = raw.RemoteAddr()
		if !h.local {
			c.remote = net.TCPAddrFromAddrPort(h.src)
		}
	case l.tls == nil:
		// Not even a byte is read: there is nothing this peer could send
		// that would make plaintext from it acceptable.
		return nil, ErrPlaintext
	default:
		b, err := c.r.Peek(1)
		if err != nil {
			return nil, err
		}
		switch {
		case b[0] == 'P' || b[0] == v2Sig[0]:
			return nil, ErrUntrustedHeader
		case b[0] != tlsHandshake:
			return nil, ErrPlaintext
		}
	}
	var out net.Conn = c
	if l.tls != nil {
		tc := tls.Server(c, l.tls)
		if err := tc.HandshakeContext(l.ctx); err != nil {
			return nil, err
		}
		out = tc
	}
	return out, nil
}

// Accept returns the next connection that passed vetting, or an error
// from the underlying listener.
func (l *Listener) Accept() (net.Conn, error) {
	if l.ctx.Err() != nil {
		return nil, l.closedErr()
	}
	select {
	case c := <-l.ready:
		return c, nil
	case err := <-l.errc:
		return nil, err
	case <-l.ctx.Done():
		return nil, l.closedErr()
	}
}

func (l *Listener) closedErr() error {
	return &net.OpError{Op: "accept", Net: l.inner.Addr().Network(), Addr: l.inner.Addr(), Err: net.ErrClosed}
}

// Close stops accepting, closes the underlying listener and aborts the
// connections still in their handshake. Connections Accept already
// returned are unaffected.
func (l *Listener) Close() error {
	err := l.closedErr()
	l.closeOnce.Do(func() {
		l.cancel()
		err = l.inner.Close()
	})
	return err
}

// Addr is the underlying listener's address.
func (l *Listener) Addr() net.Addr { return l.inner.Addr() }

// Conn is a vetted connection before any TLS: what Accept returns when
// a proxy terminates TLS, and what a *tls.Conn from Accept wraps
// otherwise.
type Conn struct {
	net.Conn
	// r holds whatever was read past the PROXY header, such as the start
	// of a ClientHello in the same segment; all reads go through it.
	r      *bufio.Reader
	remote net.Addr
	proxy  net.Addr
}

func (c *Conn) Read(p []byte) (int, error) { return c.r.Read(p) }

// RemoteAddr is the client's address: the TCP peer for a direct
// connection, the address in the PROXY header for a proxied one, or the
// proxy itself when its header named no client (v2 LOCAL, such as a
// health check, or v1 UNKNOWN).
func (c *Conn) RemoteAddr() net.Addr { return c.remote }

// Proxy is the proxy's address, or nil for a direct connection.
func (c *Conn) Proxy() net.Addr { return c.proxy }

// Proxy returns the address of the proxy a connection from a Listener
// came through, or nil if it came directly (or is not from a Listener).
// It sees through a *tls.Conn, so it works on whatever Accept returned,
// and on what a server such as go-smtp hands back as the underlying
// connection.
func Proxy(c net.Conn) net.Addr {
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	if mc, ok := c.(*Conn); ok {
		return mc.proxy
	}
	return nil
}

// trusts reports whether a TCP peer is a configured proxy. Anything
// that is not a TCP address is not.
func (l *Listener) trusts(a net.Addr) bool {
	ta, ok := a.(*net.TCPAddr)
	return ok && l.proxies.Trusts(ta.AddrPort().Addr())
}
