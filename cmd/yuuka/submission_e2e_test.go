package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authn"
	"github.com/yuuka-mail/yuuka/internal/localraw"
	"github.com/yuuka-mail/yuuka/internal/outbox"
	"github.com/yuuka-mail/yuuka/internal/r2/r2fake"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
	"github.com/yuuka-mail/yuuka/internal/submit"
	"github.com/yuuka-mail/yuuka/internal/transport"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// capsOnly is an exit the test never sends through.
type capsOnly struct{}

func (capsOnly) Caps() transport.Caps { return transport.CloudflareCaps() }

func (capsOnly) Send(context.Context, transport.Envelope, []byte, func(context.Context) error) (*transport.SendResult, error) {
	return nil, errors.New("not sent in this test")
}

// SMTP client → smtpd → submit → the outbox row or the held draft, as
// serve wires them: a queued message, a held one, and the refusals that
// are the port's 550.
func TestSubmissionEndToEnd(t *testing.T) {
	t.Parallel()
	db, pool := storetest.New(t)
	ctx := t.Context()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Now()
	principal := func(kind, name string) store.Principal {
		p := store.Principal{ID: ulid.New(time.Now()), Kind: kind, Name: name, DisplayName: name, CreatedAt: now}
		if err := db.CreatePrincipal(ctx, p, store.Action{ID: ulid.New(now), Tool: "principal.create", At: now}); err != nil {
			t.Fatal(err)
		}
		return p
	}
	op, monitor := principal("human", "operator"), principal("service", "monitoring")
	account := storetest.Account(t, db)
	for addr, owner := range map[string]string{"alerts": monitor.ID, "reports": monitor.ID, "ops": op.ID} {
		id := ulid.New(time.Now())
		if _, err := pool.Exec(ctx, `INSERT INTO identities (id, account_id, zone, prefix, display_name, owner_id)
			VALUES ($1, $2, 'example.com', $3, $4, $5)`, id, account, addr, strings.ToUpper(addr[:1])+addr[1:], owner); err != nil {
			t.Fatal(err)
		}
		storetest.Route(t, pool, id, "cf")
		if addr == "alerts" {
			if _, err := pool.Exec(ctx, `INSERT INTO identity_grants (identity_id, principal_id, send_policy, recipient_scope)
				VALUES ($1, $2, 'auto', 'any')`, id, monitor.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	pw, err := authn.IssueToken(ctx, db, now, authn.TokenRequest{
		Kind: auth.KindSMTPPassword, PrincipalID: monitor.ID, Scopes: []auth.Scope{auth.SubmissionSend}, IssuedBy: store.SystemPrincipalID,
	})
	if err != nil {
		t.Fatal(err)
	}
	user, _, _ := auth.ParseSecret(auth.PrefixSMTP, pw)

	out := outbox.New(outbox.Config{
		DB: db, Raw: r2fake.New(), Local: localraw.Dir{Root: t.TempDir()},
		Transports: map[string]outbox.Transport{"cf": capsOnly{}}, Log: log,
	})
	core := submitPort{submit.New(submit.Config{DB: db, Outbox: out, Log: log})}

	certFile, keyFile, roots := testCert(t, t.TempDir())
	probe, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()
	stop, errc, err := startSubmission(submissionConfig{Listen: addr, CertFile: certFile, KeyFile: keyFile}, core, db, "mail.example.com", log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := stop(sctx); err != nil {
			t.Error(err)
		}
		if err := <-errc; err != nil {
			t.Error(err)
		}
	})

	send := func(from string) (int, string) {
		t.Helper()
		conn, err := (&tls.Dialer{Config: &tls.Config{RootCAs: roots, ServerName: "mail.example.com", MinVersion: tls.VersionTLS12}}).
			DialContext(ctx, "tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		c, err := smtp.NewClient(conn, "mail.example.com")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		if err := c.Auth(smtp.PlainAuth("", user, pw, "mail.example.com")); err != nil {
			t.Fatal(err)
		}
		if err := c.Mail(from); err != nil {
			return replyCode(err)
		}
		if err := c.Rcpt("oncall@example.net"); err != nil {
			return replyCode(err)
		}
		w, err := c.Data()
		if err != nil {
			return replyCode(err)
		}
		_, _ = io.WriteString(w, "From: \"The CEO\" <"+from+">\r\nTo: oncall@example.net\r\nSubject: disk\r\n\r\nDisk full.\r\n")
		if err := w.Close(); err != nil {
			return replyCode(err)
		}
		return 250, ""
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Granted: queued, as the service, under the identity's name.
	if n, msg := send("alerts@example.com"); n != 250 {
		t.Fatalf("alerts: %d %s", n, msg)
	}
	if count(`SELECT count(*) FROM outbox WHERE from_addr = 'alerts@example.com' AND submitted_by = $1 AND state = 'queued'`, monitor.ID) != 1 {
		t.Error("no queued outbox row for alerts@")
	}
	// Its own identity without a grant: review + thread, held as a raw
	// draft, still 250 for the client.
	if n, msg := send("reports@example.com"); n != 250 {
		t.Fatalf("reports: %d %s", n, msg)
	}
	if count(`SELECT count(*) FROM drafts WHERE from_addr = 'reports@example.com' AND raw_id IS NOT NULL AND hold_reason IS NOT NULL`) != 1 {
		t.Error("no held draft for reports@")
	}
	// The operator's address (tier none) and an address nobody has: 550.
	for _, from := range []string{"ops@example.com", "someone@example.org"} {
		if n, msg := send(from); n != 550 {
			t.Errorf("%s: %d %s, want 550", from, n, msg)
		}
	}
	if count(`SELECT count(*) FROM outbox`) != 1 || count(`SELECT count(*) FROM drafts`) != 1 {
		t.Error("a refused message left a row")
	}
}

func replyCode(err error) (int, string) {
	if te, ok := errors.AsType[*textproto.Error](err); ok {
		return te.Code, te.Msg
	}
	return -1, err.Error()
}
