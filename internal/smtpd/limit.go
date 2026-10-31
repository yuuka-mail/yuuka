package smtpd

import (
	"crypto/tls"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/yuuka-mail/yuuka/internal/mailnet"
)

// limits are the abuse controls. The port is meant for a handful of
// services on the same network (§13.7), so the caps are far above what
// they need and far below what would hurt: a session in DATA holds up to
// MaxMessageBytes in memory.
type limits struct {
	// conns caps open connections; connsPerClient those of one client
	// (an IPv4 address or an IPv6 /64), so one client cannot hold them
	// all.
	conns, connsPerClient int
	// authFailures failed AUTHs are allowed at once per client, then one
	// per authFailureEvery; past that AUTH is refused without a check.
	// Passwords are 256-bit random (§13.3), so this does not protect
	// them: it bounds the database lookups and audit entries a client
	// can cause.
	authFailures     int
	authFailureEvery time.Duration
}

func (l limits) withDefaults() limits {
	if l.conns <= 0 {
		l.conns = 32
	}
	if l.connsPerClient <= 0 {
		l.connsPerClient = 8
	}
	if l.authFailures <= 0 {
		l.authFailures = 5
	}
	if l.authFailureEvery <= 0 {
		l.authFailureEvery = time.Minute
	}
	return l
}

// clientOf returns the address recorded for a peer and the key it is
// limited under. An IPv6 client is keyed by its /64, which is what one
// host usually gets; per address, it could pick a fresh key each time.
func clientOf(a net.Addr) (addr, key string) {
	ta, ok := a.(*net.TCPAddr)
	if !ok {
		return a.String(), a.String()
	}
	ip := ta.AddrPort().Addr().Unmap()
	if ip.Is6() {
		return ip.String(), netip.PrefixFrom(ip, 64).Masked().String()
	}
	return ip.String(), ip.String()
}

// The connection caps are counted on the raw TCP connection, underneath
// mailnet and TLS. go-smtp only lets a connection use AUTH over TLS if
// it is a *tls.Conn, so the connection it gets cannot be wrapped; the
// raw one can, and closing what go-smtp holds closes it too.

// trackedListener wraps the TCP listener so that each connection can
// report its own close.
type trackedListener struct{ net.Listener }

func (l trackedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &trackedConn{Conn: c}, nil
}

type trackedConn struct {
	net.Conn
	mu      sync.Mutex
	closed  bool
	onClose func()
}

func (c *trackedConn) Close() error {
	c.mu.Lock()
	f := c.onClose
	c.onClose, c.closed = nil, true
	c.mu.Unlock()
	if f != nil {
		f()
	}
	return c.Conn.Close()
}

// setOnClose arranges for f to run once when c closes; false if it
// already has.
func (c *trackedConn) setOnClose(f func()) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.onClose = f
	return true
}

// tracked finds the trackedConn under what mailnet's Accept returned.
func tracked(c net.Conn) *trackedConn {
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	if mc, ok := c.(*mailnet.Conn); ok {
		c = mc.Conn
	}
	t, _ := c.(*trackedConn)
	return t
}

// gate counts open connections, in total and per client.
type gate struct {
	mu        sync.Mutex
	max, per  int
	perClient map[string]int
	open      map[*trackedConn]struct{}
}

func newGate(maxConns, perClient int) *gate {
	return &gate{max: maxConns, per: perClient, perClient: map[string]int{}, open: map[*trackedConn]struct{}{}}
}

// enter admits c under key, or reports false when a cap is reached.
func (g *gate) enter(key string, c *trackedConn) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.open) >= g.max || g.perClient[key] >= g.per {
		return false
	}
	leave := func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		delete(g.open, c)
		if g.perClient[key]--; g.perClient[key] <= 0 {
			delete(g.perClient, key)
		}
	}
	if !c.setOnClose(leave) {
		return false
	}
	g.open[c] = struct{}{}
	g.perClient[key]++
	return true
}

// closeAll closes every admitted connection.
func (g *gate) closeAll() {
	g.mu.Lock()
	cs := make([]*trackedConn, 0, len(g.open))
	for c := range g.open {
		cs = append(cs, c)
	}
	g.mu.Unlock()
	for _, c := range cs {
		_ = c.Close()
	}
}

// gatedListener is what go-smtp accepts from: mailnet's listener, minus
// the connections over a cap, which get a 421 and are closed.
type gatedListener struct {
	net.Listener
	s *Server
}

func (l *gatedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		t := tracked(c)
		addr, key := clientOf(c.RemoteAddr())
		if t != nil && l.s.gate.enter(key, t) {
			return c, nil
		}
		if t == nil {
			l.s.cfg.Log.Error("smtp: connection without a tracked socket refused", "client", addr)
		} else if l.s.logFl.allow("busy " + key) {
			l.s.cfg.Log.Warn("smtp: connection cap reached", "client", addr)
		}
		// A reply rather than a bare close, so the client knows to try
		// later; it must not hold up Accept, hence the short deadline.
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, _ = io.WriteString(c, "421 4.7.0 Too many connections, try again later\r\n")
		_ = c.Close()
	}
}

// limiter is a token bucket per key: burst at once, then one per every.
type limiter struct {
	mu      sync.Mutex
	burst   float64
	every   time.Duration
	now     func() time.Time
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

// maxBuckets bounds memory; past it, full (idle) buckets are dropped.
const maxBuckets = 10000

func newLimiter(burst int, every time.Duration, now func() time.Time) *limiter {
	return &limiter{burst: float64(burst), every: every, now: now, buckets: map[string]*bucket{}}
}

// refill brings key's bucket up to date; the caller holds mu.
func (l *limiter) refill(key string) *bucket {
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= maxBuckets {
			for k, b := range l.buckets {
				if b.tokens+float64(now.Sub(b.at))/float64(l.every) >= l.burst {
					delete(l.buckets, k)
				}
			}
		}
		b = &bucket{tokens: l.burst, at: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+float64(now.Sub(b.at))/float64(l.every))
	b.at = now
	return b
}

// allow takes a token if there is one.
func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.refill(key)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// empty reports whether key has no token left, without taking one.
func (l *limiter) empty(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.refill(key).tokens < 1
}
