package authn

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Bearer authenticates API callers (REST, MCP) by bearer token alone
// (§13.7). Unlike Service it needs no WebUI origin, so the agent endpoints
// work on a deployment without the WebUI login; and it never looks at a
// session cookie: a browser's ambient session is not an agent's credential.
type Bearer struct {
	db        *store.DB
	log       *slog.Logger
	now       func() time.Time
	rejectLog *limiter
}

// NewBearer returns a Bearer. now nil means time.Now; log nil means the
// default logger.
func NewBearer(db *store.DB, log *slog.Logger, now func() time.Time) *Bearer {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &Bearer{db: db, log: log, now: now, rejectLog: newLimiter(1, 10*time.Minute, now)}
}

// Authenticate resolves a token to a Viewer. Errors wrap
// ErrUnauthenticated unless the database failed. A token that names an
// existing credential but is refused (bad secret, revoked, expired) is
// recorded in the audit log, at most once per credential per ten minutes;
// via says where it was presented, e.g. "mcp stdio".
func (b *Bearer) Authenticate(ctx context.Context, token, via string) (*auth.Viewer, error) {
	return b.check(ctx, auth.KindToken, token, via)
}

// AuthenticateSMTP resolves the SASL user name and password of the SMTP
// submission port (§13.6) to a Viewer, with the same audit of refused
// credentials as Authenticate. The user name is the credential id, which
// the password carries too; they must agree, so a password cannot be
// presented under another credential's name. Errors wrap
// ErrUnauthenticated unless the database failed.
func (b *Bearer) AuthenticateSMTP(ctx context.Context, username, password, via string) (*auth.Viewer, error) {
	id, _, ok := auth.ParseSecret(auth.PrefixSMTP, password)
	if !ok || id != username {
		return nil, ErrUnauthenticated
	}
	return b.check(ctx, auth.KindSMTPPassword, password, via)
}

func (b *Bearer) check(ctx context.Context, kind, secret, via string) (*auth.Viewer, error) {
	v, err := authenticate(ctx, b.db, b.log, b.now(), kind, secret)
	if rej, ok := errors.AsType[*rejection](err); ok && b.rejectLog.allow(rej.credentialID) {
		now := b.now()
		a := store.Action{
			ID: ulid.New(now), CredentialID: rej.credentialID, Tool: "auth.reject", At: now,
			Args: map[string]any{"reason": rej.reason, "via": via},
		}
		if err := b.db.RecordAction(context.WithoutCancel(ctx), a); err != nil {
			b.log.Error("auth: cannot record action", "tool", a.Tool, "err", err)
		}
	}
	return v, err
}
