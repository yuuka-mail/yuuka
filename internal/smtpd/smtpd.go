// Package smtpd is the SMTP submission port (§13.6): services that speak
// SMTP hand yuuka a finished message under an SMTP password (§13.3), and
// the submission core decides where it goes.
//
// This package is the protocol and nothing else. Which identity the
// header From names, whether MAIL FROM agrees with it, the display name,
// how far the principal may send: all of that belongs to the Submitter,
// which every submission path shares. What is here is AUTH PLAIN against
// credentials, the envelope and DATA with their limits, the mapping of
// the core's answer to a reply, and the abuse controls of a port that
// takes passwords: connection caps, auth-failure throttling, timeouts and
// an audit entry for every authentication and submission.
//
// The network edge (implicit TLS, the PROXY protocol, who may send
// plaintext) is internal/mailnet's; Serve puts it in front of the
// listener it is given.
package smtpd

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-smtp"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/clientip"
	"github.com/yuuka-mail/yuuka/internal/mailnet"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Protocol limits. The message size and recipient count are the
// Cloudflare exit's (§10.2 measurements: SIZE 26214400, 50 recipients):
// accepting more here would only turn into a rejection at the exit,
// after the client was told 250.
const (
	MaxMessageBytes = 25 << 20
	MaxRecipients   = 50
	// MaxLineLength bounds one command or DATA line. RFC 5321 allows
	// 1000 octets; go-smtp's default doubles that for tolerance, and the
	// memory a line can take is what matters here.
	MaxLineLength = 2000
	// ReadTimeout is the RFC 5321 minimum for the server's wait on a
	// command. go-smtp sets the read deadline per command line, so it
	// also bounds a whole DATA transfer: 25 MiB in five minutes needs
	// under 1 Mbit/s.
	ReadTimeout  = 5 * time.Minute
	WriteTimeout = time.Minute
)

// via is how the audit log and authn name this port.
const via = "smtp"

// Result is what the submission core did with one message. Its fields
// are those of the core's own result, so the wiring converts one into
// the other directly.
type Result struct {
	// Queued: the message is in the outbox, due to be sent.
	Queued bool
	// OutboxID is set when the message has an outbox row, queued or
	// held for review; DraftID when it was kept as a draft instead.
	OutboxID, DraftID string
	// HoldReason says why it was not queued; Detail is free text.
	HoldReason, Detail string
}

// ErrRefused is wrapped by Submitter errors that are the policy's final
// answer: no identity covers the header From, the grant says none, the
// message breaks a rule of §13.6. The client gets 550 5.7.1 with the
// error's text; any other error is temporary (451) and the client keeps
// the message and retries.
var ErrRefused = errors.New("refused")

// Submitter is the submission core, consumed here. It owns every rule
// about the message itself (§13.6); this package only carries it in.
type Submitter interface {
	SubmitRaw(ctx context.Context, v *auth.Viewer, mailFrom string, rcpts []string, raw []byte) (Result, error)
}

// Authenticator checks an SMTP user name and password: authn.Bearer.
// Errors wrap authn.ErrUnauthenticated unless the check itself failed.
type Authenticator interface {
	AuthenticateSMTP(ctx context.Context, username, password, via string) (*auth.Viewer, error)
}

// Auditor appends to the audit log: store.DB.
type Auditor interface {
	RecordAction(ctx context.Context, a store.Action) error
}

// Config is what a Server needs.
type Config struct {
	Submitter Submitter
	Auth      Authenticator
	Audit     Auditor

	// TLS, if set, makes yuuka terminate TLS. If nil a proxy in front
	// terminates it and Proxies must name it; see mailnet.Config, which
	// these two fields are passed to.
	TLS     *tls.Config
	Proxies clientip.Proxies

	// Domain is the name in the greeting. Default "yuuka".
	Domain string
	Log    *slog.Logger

	// limits are the abuse controls; zero fields take the defaults.
	// Tests shrink them.
	limits limits
}

// Server is a submission port. Create it with New and run it with Serve.
type Server struct {
	cfg    Config
	smtp   *smtp.Server
	gate   *gate
	authFl *limiter // auth failures per client
	logFl  *limiter // throttled-auth and refused-connection log entries per client
	now    func() time.Time
}

// New returns a Server. It does not listen yet.
func New(cfg Config) *Server {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Domain == "" {
		cfg.Domain = "yuuka"
	}
	cfg.limits = cfg.limits.withDefaults()
	s := &Server{cfg: cfg, now: time.Now}
	s.gate = newGate(cfg.limits.conns, cfg.limits.connsPerClient)
	s.authFl = newLimiter(cfg.limits.authFailures, cfg.limits.authFailureEvery, s.now)
	s.logFl = newLimiter(1, 10*time.Minute, s.now)

	srv := smtp.NewServer(smtp.BackendFunc(s.newSession))
	srv.Domain = cfg.Domain
	srv.MaxMessageBytes = MaxMessageBytes
	srv.MaxRecipients = MaxRecipients
	srv.MaxLineLength = MaxLineLength
	srv.ReadTimeout = ReadTimeout
	srv.WriteTimeout = WriteTimeout
	// Without TLS of its own, every connection go-smtp sees comes from a
	// configured proxy that terminated TLS: mailnet refuses plaintext
	// from anyone else and refuses to start without proxies. So
	// plaintext AUTH is safe exactly then. srv.TLSConfig stays nil
	// either way, which is what keeps STARTTLS unadvertised (§13.6).
	srv.AllowInsecureAuth = cfg.TLS == nil
	// SMTPUTF8 stays off: the Cloudflare exit does not offer it, so a
	// message that needed it could not be sent on. 8BITMIME and CHUNKING
	// go-smtp always advertises; the exit takes 8-bit bodies, and the
	// transport checks what it sends against what the exit offers.
	srv.ErrorLog = errorLog{cfg.Log}
	s.smtp = srv
	return s
}

// Serve accepts connections on inner, a TCP listener, until Shutdown,
// which closes inner. Connections pass mailnet's vetting and the
// connection caps before the greeting.
func (s *Server) Serve(inner net.Listener) error {
	ml, err := mailnet.NewListener(trackedListener{inner}, mailnet.Config{
		TLS:      s.cfg.TLS,
		Proxies:  s.cfg.Proxies,
		Rejected: s.refused,
	})
	if err != nil {
		_ = inner.Close()
		return err
	}
	err = s.smtp.Serve(&gatedListener{Listener: ml, s: s})
	if errors.Is(err, smtp.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops accepting and waits for open sessions to end, then
// closes whatever is left when ctx is done. A session cut in the middle
// of DATA never got its 250, so its client still holds the message.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.smtp.Shutdown(ctx)
	if ctx.Err() != nil {
		// go-smtp's Close is a no-op after Shutdown, so cut the
		// connections underneath it.
		s.gate.closeAll()
	}
	if errors.Is(err, smtp.ErrServerClosed) {
		return nil
	}
	return err
}

// refused logs a connection mailnet turned away, at most once per
// client per ten minutes so a scan does not flood the log.
func (s *Server) refused(peer net.Addr, err error) {
	if _, key := clientOf(peer); s.logFl.allow("conn " + key) {
		s.cfg.Log.Info("smtp: connection refused", "peer", peer.String(), "err", err)
	}
}

// record appends an audit entry, logging instead of failing: the
// protocol outcome is already decided when it is written.
func (s *Server) record(a store.Action) {
	a.At = s.now()
	a.ID = ulid.New(a.At)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.cfg.Audit.RecordAction(ctx, a); err != nil {
		s.cfg.Log.Error("smtp: cannot record action", "tool", a.Tool, "err", err)
	}
}

// errorLog sends go-smtp's own complaints (mostly connections that broke
// off) to the structured log.
type errorLog struct{ log *slog.Logger }

func (e errorLog) Printf(format string, v ...any) {
	e.log.Info("smtp: " + strings.TrimSpace(fmt.Sprintf(format, v...)))
}

func (e errorLog) Println(v ...any) {
	e.log.Info("smtp: " + strings.TrimSpace(fmt.Sprintln(v...)))
}
