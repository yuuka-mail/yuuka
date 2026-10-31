package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// host is what the management commands (prefix, token, outbox, identity,
// grant, rule) share. They run on the host, like `admin enroll`: being able to
// run them there is the highest authority there is (§13.7), so they need
// no login, and nobody is authenticated either. Their actions are
// recorded with the system principal as actor, no credential, and
// args.via = "cli" plus the
// operating system user that ran them (host_user), which is what is
// actually known. Recording a human principal instead would claim a login
// that never happened.
type host struct {
	db  *store.DB
	out io.Writer // results: tables, the token
	msg io.Writer // notes for the person at the terminal
	in  io.Reader // what `--spec -` reads; nil is stdin
	now func() time.Time
	// user is the OS user name; "" if unknown.
	user string
}

// withHost opens the database (openDB) and runs f.
func withHost(ctx context.Context, f func(*host) error) error {
	d, err := loadDBConfig(os.Getenv)
	if err != nil {
		return err
	}
	pool, db, err := openDB(ctx, d)
	if err != nil {
		return err
	}
	defer pool.Close()
	return f(&host{db: db, out: os.Stdout, msg: os.Stderr, now: time.Now, user: osUser()})
}

func osUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

// say writes one line of result.
func (h *host) say(format string, args ...any) error {
	_, err := fmt.Fprintf(h.out, format+"\n", args...)
	return err
}

// note writes a line for the person at the terminal, not for a pipe.
// Best effort: the result on out is what matters.
func (h *host) note(format string, args ...any) {
	_, _ = fmt.Fprintf(h.msg, format+"\n", args...)
}

// auditArgs are added to every action a management command records.
func (h *host) auditArgs() map[string]any {
	a := map[string]any{"via": "cli"}
	if h.user != "" {
		a["host_user"] = h.user
	}
	return a
}

// action is an audit entry of tool by the host with args.
func (h *host) action(tool string, args map[string]any) store.Action {
	all := h.auditArgs()
	for k, v := range args {
		all[k] = v
	}
	now := h.now()
	return store.Action{ID: ulid.New(now), Actor: store.SystemPrincipalID, Tool: tool, At: now, Args: all}
}

// viewer is who the host's reads run as: the system principal, with only
// the scopes the command needs. The store requires a viewer on every read
// of mail-derived data (§13.8), the host included.
func (h *host) viewer(scopes ...auth.Scope) *auth.Viewer {
	return &auth.Viewer{PrincipalID: store.SystemPrincipalID, PrincipalKind: "system", PrincipalName: "system", Scopes: scopes}
}

// shown makes text from mail safe for a terminal and a table: discovered
// prefixes are named by whoever sent the mail, so control characters and
// tabs are escaped rather than written.
func shown(s string) string {
	for _, r := range s {
		if !strconv.IsGraphic(r) {
			q := strconv.QuoteToGraphic(s)
			return q[1 : len(q)-1]
		}
	}
	return s
}

// stamp formats an optional time for a table.
func stamp(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}
