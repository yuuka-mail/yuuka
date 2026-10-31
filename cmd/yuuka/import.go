package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// The one-shot import of the deprecated environment transports into the
// database (§17.7). serve calls importEnvTransports once at start. For
// each transport in YUUKA_TRANSPORTS that the database has only as a
// placeholder, or not at all, it writes host, port, caps and the
// password from _PASSWORD_FILE, sealed. A transport the database already
// has configured, or has removed, wins over the environment, which is
// ignored with a warning: there is one source of truth, and after the
// import it is the database. The parsing and this code go one release
// later (§17.9 P7).
//
// Every write is a ConfigureTransport at the revision read (0 for a new
// row), so of several serve processes starting at once exactly one
// writes each transport; the others see their write refused, read again,
// find it configured and move on. Running it again later imports nothing.
//
// Without a KEK nothing is imported: the environment transports keep
// working from memory as before, and serve says so. An upgrade never
// stops mail from going out.

// importAttempts bounds the read-and-write loop per transport. A
// refused write means another process just wrote it; the second read
// sees that.
const importAttempts = 3

// importEnvTransports imports ts, sealing with s, serve's Sealer (nil
// without YUUKA_KEK_FILE: then nothing is imported). Failures to import
// one transport are logged, not returned: the environment still works
// for it.
func importEnvTransports(ctx context.Context, log *slog.Logger, db *store.DB, ts []transportConfig,
	s *secret.Sealer, readFile func(string) ([]byte, error),
) {
	if len(ts) == 0 {
		return
	}
	if s == nil {
		log.Warn("YUUKA_TRANSPORTS is deprecated and not imported without YUUKA_KEK_FILE; "+
			"the transports keep working from the environment for now (docs/operate/secrets.md)",
			"transports", len(ts))
		return
	}
	for _, t := range ts {
		if err := importTransport(ctx, log, db, s, t, readFile); err != nil {
			log.Error("import transport from the environment; it keeps working from there",
				"transport", t.Name, "err", err)
		}
	}
}

func importTransport(ctx context.Context, log *slog.Logger, db *store.DB, s *secret.Sealer, t transportConfig,
	readFile func(string) ([]byte, error),
) error {
	vars := transportVar(t.Name, "*")
	for range importAttempts {
		cur, err := db.TransportByName(ctx, t.Name)
		var rev int64
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return err
		case cur.DisabledAt != nil:
			log.Warn("transport was removed in the database; the environment's is ignored: remove "+vars,
				"transport", t.Name)
			return nil
		case cur.ConfiguredAt != nil:
			log.Warn("transport is configured in the database; the environment's is ignored: remove "+vars,
				"transport", t.Name)
			return nil
		default:
			rev = cur.Revision
		}
		err = writeImport(ctx, db, s, t, rev, readFile)
		if errors.Is(err, store.ErrStale) || errors.Is(err, store.ErrDuplicate) {
			continue // another process imported it; the next read says so
		}
		if err != nil {
			return err
		}
		log.Info("imported transport from the environment into the database; remove "+vars, "transport", t.Name)
		return nil
	}
	return fmt.Errorf("transport %s kept changing while it was imported", t.Name)
}

func writeImport(ctx context.Context, db *store.DB, s *secret.Sealer, t transportConfig, rev int64,
	readFile func(string) ([]byte, error),
) error {
	c := store.TransportConfig{Name: t.Name, Host: t.Host, Port: t.Port, Auth: store.TransportAuthNone, Caps: capNames(t.Caps)}
	var err error
	if c.Host, err = secret.CanonicalHost(t.Host); err != nil {
		return err
	}
	now := time.Now()
	var sealed *secret.Record
	if t.Username != "" {
		c.Auth, c.Username = store.TransportAuthPlain, t.Username
		pw, err := readFile(t.PasswordFile)
		if err != nil {
			return fmt.Errorf("password file: %w", err)
		}
		// As openTransports reads it: one trailing newline dropped.
		pw = bytes.TrimSuffix(bytes.TrimSuffix(pw, []byte("\n")), []byte("\r"))
		sealed, err = sealSMTPPassword(ctx, s, c, pw, now)
		clear(pw)
		if err != nil {
			return err
		}
	}
	args := transportAuditArgs(c, sealed)
	args["via"] = "env"
	if rev != 0 {
		args["revision"] = rev
	}
	a := store.Action{ID: ulid.New(now), Actor: store.SystemPrincipalID, Tool: "config.import", At: now, Args: args}
	_, err = db.ConfigureTransport(ctx, c, rev, sealed, a)
	return err
}
