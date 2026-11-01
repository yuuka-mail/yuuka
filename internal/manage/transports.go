package manage

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"time"

	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/transport"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Transports (§17, §10.2). Configuring one always widens: it is a secret
// write or a change of where mail goes (§17.6). Removing one tightens.

// ErrPasswordUnused: a password came for a transport that does not
// authenticate (auth none, no username).
var ErrPasswordUnused = errors.New("a password needs auth plain and a username")

// PasswordNeededError: the write authenticates with a password and none
// came with it, while none can be kept.
type PasswordNeededError struct {
	// Moved: the transport has a password, sealed for the destination it
	// moves away from. A password is only ever sent where it was set for
	// (§17.4), so it has to be given again.
	Moved bool
	// Destination is where the transport goes after the write.
	Destination string
}

func (e *PasswordNeededError) Error() string {
	if e.Moved {
		return "this moves the transport to " + e.Destination + ": a password is only ever sent where it was set for, so give it again"
	}
	return "auth plain needs the password"
}

// Destination describes where a transport connects, as its audience
// would, without the scheme: user@host:port.
func Destination(c store.TransportConfig) string {
	d := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	if c.Username != "" {
		d = c.Username + "@" + d
	}
	return d
}

// Moves reports whether writing c over cur (nil: no row) changes where a
// secret would be sent: a new transport, a placeholder being configured,
// another host, port, auth or user name.
func Moves(cur *store.Transport, c store.TransportConfig) bool {
	return cur == nil || cur.ConfiguredAt == nil ||
		cur.Host != c.Host || cur.Port != c.Port || cur.Auth != c.Auth || cur.Username != c.Username
}

// CheckPassword says whether writing c over cur needs a password and
// whether one may come with it (hasPassword): ErrPasswordUnused, or a
// *PasswordNeededError. c must have been checked (CheckTransportConfig).
func CheckPassword(cur *store.Transport, c store.TransportConfig, hasPassword bool) error {
	switch {
	case c.Auth != store.TransportAuthPlain && hasPassword:
		return ErrPasswordUnused
	case c.Auth == store.TransportAuthPlain && !hasPassword && (Moves(cur, c) || cur.SecretID == ""):
		moved := cur != nil && cur.ConfiguredAt != nil && cur.SecretID != ""
		return &PasswordNeededError{Moved: moved, Destination: Destination(c)}
	}
	return nil
}

// CheckTransportConfig is store.CheckTransportConfig, with what it
// refuses as *Invalid.
func CheckTransportConfig(c store.TransportConfig) (store.TransportConfig, error) {
	c, err := store.CheckTransportConfig(c)
	if err != nil {
		return c, &Invalid{Reason: err.Error()}
	}
	return c, nil
}

// SealSMTPPassword seals pw for c's destination under a fresh id. It
// does not keep pw; the caller clears it.
func SealSMTPPassword(ctx context.Context, s *secret.Sealer, c store.TransportConfig, pw []byte, now time.Time) (*secret.Record, error) {
	if s == nil {
		return nil, ErrLocked
	}
	aud, err := secret.SMTPS(c.Username, c.Host, c.Port)
	if err != nil {
		return nil, fmt.Errorf("transport %s: %w", c.Name, err)
	}
	r, err := s.Seal(ctx, ulid.New(now), store.SecretSMTPPassword, aud, pw)
	if err != nil {
		return nil, fmt.Errorf("transport %s: seal the password: %w", c.Name, err)
	}
	return &r, nil
}

// TransportAuditArgs are what an action that widens a transport records
// (§17.6): where it goes and which sealed secret, never the secret, its
// hash or anything else derived from the plaintext.
func TransportAuditArgs(c store.TransportConfig, sealed *secret.Record) map[string]any {
	a := map[string]any{"name": c.Name, "host": c.Host, "port": c.Port, "auth": c.Auth, "username": c.Username, "caps": c.Caps}
	if sealed != nil {
		a["secret_id"] = sealed.ID
		a["kind"] = sealed.Kind
		a["audience"] = sealed.Audience
		a["wrapper"] = sealed.Wrapper
		a["key_ref"] = sealed.KeyRef
	}
	return a
}

// TransportStore is what configuring and removing transports needs.
type TransportStore interface {
	ConfigureTransport(ctx context.Context, c store.TransportConfig, revision int64, sealed *secret.Record, a store.Action) (store.Transport, error)
	DisableTransport(ctx context.Context, name string, revision int64, a store.Action) error
}

// TransportWrite is a whole configuration to write over a transport.
type TransportWrite struct {
	Config store.TransportConfig
	// Password is the new password; nil keeps the one sealed now. It is
	// not kept: the caller clears it.
	Password []byte
	// Tool is what the action is recorded as: transport.add or
	// transport.set.
	Tool string
}

// ConfigureTransport writes w over cur, the row as the caller read it
// (nil when the name has none), at cur's revision: a change made since is
// store.ErrStale, a row made since store.ErrDuplicate, and nothing is
// written. A new password, and any move to another destination, seals a
// new secret under a new id (§17.4). The action records the destination
// and the sealed secret's metadata (TransportAuditArgs) and what it was.
//
// Errors: *Invalid, ErrPasswordUnused, *PasswordNeededError, ErrLocked
// (a password and no key), and the store's.
func ConfigureTransport(ctx context.Context, st TransportStore, sealer *secret.Sealer, cur *store.Transport,
	w TransportWrite, act Act, now time.Time,
) (store.Transport, *secret.Record, error) {
	c, err := CheckTransportConfig(w.Config)
	if err != nil {
		return store.Transport{}, nil, err
	}
	if err := CheckPassword(cur, c, w.Password != nil); err != nil {
		return store.Transport{}, nil, err
	}
	var sealed *secret.Record
	if w.Password != nil {
		if sealed, err = SealSMTPPassword(ctx, sealer, c, w.Password, now); err != nil {
			return store.Transport{}, nil, err
		}
	}
	args := TransportAuditArgs(c, sealed)
	var rev int64
	if cur != nil {
		rev = cur.Revision
		args["transport_id"] = cur.ID
		args["revision"] = cur.Revision
		if cur.ConfiguredAt != nil {
			args["was"] = map[string]any{"host": cur.Host, "port": cur.Port, "auth": cur.Auth, "username": cur.Username, "caps": cur.Caps}
		}
	}
	t, err := st.ConfigureTransport(ctx, c, rev, sealed, act(w.Tool, args))
	return t, sealed, err
}

// DisableTransport removes cur, as read, at its revision: disabled and
// its secret erased (store.DisableTransport). A tightening, recorded as
// transport.rm.
func DisableTransport(ctx context.Context, st TransportStore, cur store.Transport, act Act) error {
	args := map[string]any{
		"transport_id": cur.ID, "name": cur.Name, "host": cur.Host, "port": cur.Port,
		"username": cur.Username, "revision": cur.Revision,
	}
	if cur.SecretID != "" {
		args["secret_id"] = cur.SecretID
	}
	return st.DisableTransport(ctx, cur.Name, cur.Revision, act("transport.rm", args))
}

// NamedBy are the enabled identities among ids whose chain names
// transport name.
func NamedBy(ids []store.Identity, name string) []store.Identity {
	var out []store.Identity
	for _, i := range ids {
		if i.DisabledAt == nil && slices.Contains(i.Transports, name) {
			out = append(out, i)
		}
	}
	return out
}

// Why a transport cannot be probed.
var (
	ErrPlaceholder = errors.New("a placeholder: it has no destination yet")
	ErrRemoved     = errors.New("removed")
	ErrNoPassword  = errors.New("it authenticates and has no password")
)

// OpenError: a secret did not open as sealed. Reason says why in §17.3's
// terms (OpenFailure); Err is what opening returned.
type OpenError struct{ Err error }

func (e *OpenError) Error() string { return OpenFailure(e.Err) }
func (e *OpenError) Unwrap() error { return e.Err }

// SecretReader reads sealed secrets.
type SecretReader interface {
	Secret(ctx context.Context, id string) (store.StoredSecret, error)
}

// ProbeConfig is what probing cur connects with: its password opened
// for the destination recomputed from the row, as sending would. Errors:
// ErrPlaceholder, ErrRemoved, ErrNoPassword, ErrLocked (it has a
// password and there is no key), *OpenError.
func ProbeConfig(ctx context.Context, st SecretReader, sealer *secret.Sealer, cur store.Transport) (transport.Config, error) {
	switch {
	case cur.DisabledAt != nil:
		return transport.Config{}, ErrRemoved
	case cur.ConfiguredAt == nil:
		return transport.Config{}, ErrPlaceholder
	}
	cfg := transport.Config{Name: cur.Name, Host: cur.Host, Port: cur.Port, Username: cur.Username}
	if cur.Auth != store.TransportAuthPlain {
		return cfg, nil
	}
	if cur.SecretID == "" {
		return cfg, ErrNoPassword
	}
	if sealer == nil {
		return cfg, ErrLocked
	}
	sec, err := st.Secret(ctx, cur.SecretID)
	if err != nil {
		return cfg, err
	}
	aud, err := secret.SMTPS(cur.Username, cur.Host, cur.Port)
	if err != nil {
		return cfg, err
	}
	v, err := sealer.Open(ctx, sec.Record, aud)
	if err != nil {
		return cfg, &OpenError{Err: err}
	}
	defer v.Close()
	cfg.Password = string(v.Reveal())
	return cfg, nil
}

// OpenFailure explains why a secret did not open, in the terms of §17.3.
func OpenFailure(err error) string {
	switch {
	case errors.Is(err, secret.ErrLocked):
		return "locked: " + err.Error() + " (YUUKA_KEK_FILE, YUUKA_KEK_PREVIOUS_FILES)"
	case errors.Is(err, secret.ErrAudienceMismatch):
		return "audience mismatch: the destination changed after the password was sealed; set the password again"
	case errors.Is(err, secret.ErrTampered):
		return "tampered: the sealed value does not authenticate (altered, corrupted or moved from another row)"
	case errors.Is(err, secret.ErrUnsupportedVersion):
		return "sealed by a newer yuuka: " + err.Error()
	}
	return err.Error()
}

// ChainWidens reports whether moving an identity's transport chain from
// was to names lets its mail go anywhere it could not before (§17.6):
// another primary, or a transport that was not in the chain. Taking a
// fallback away, or reordering the fallbacks behind the same primary,
// only narrows where mail goes.
func ChainWidens(was, names []string) bool {
	if len(names) == 0 {
		return false
	}
	if len(was) == 0 || names[0] != was[0] {
		return true
	}
	for _, n := range names {
		if !slices.Contains(was, n) {
			return true
		}
	}
	return false
}
