package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Transports, their secrets and each identity's transport chain (§17,
// migration 0017). These are configuration, not mail: nothing here takes
// a Viewer, and no agent-facing surface reads them; agents only ever see
// a transport's name in a submission's result (§8.9).
//
// The store persists secret.Records and never encrypts or decrypts. What
// it does enforce is the shape §17.2 relies on: a transport's destination
// (host, port, auth, username) changes only together with a secret sealed
// for the new destination, so a write path that cannot seal cannot move a
// password to another server. A database writer who bypasses this package
// gets nothing either: the audience is recomputed from the row on open.

// Secret kinds (secrets.kind).
const SecretSMTPPassword = "smtp_password"

// Transport authentication methods (transports.auth).
const (
	TransportAuthNone  = "none"
	TransportAuthPlain = "plain"
)

// MaxTransportChain is how many transports an identity's chain may have:
// a primary and up to seven fallbacks.
const MaxTransportChain = 8

// transportCaps are the values transports.caps admits, as the
// configuration spells them (§10.2).
var transportCaps = []string{"message-id", "return-path", "dkim", "status-query"}

var (
	// ErrNoTransport: no enabled transport has this name.
	ErrNoTransport = errors.New("no such transport")
	// ErrStale: the row changed since the revision the caller read.
	ErrStale = errors.New("changed since it was read")
	// ErrNeedsSecret: the write changes where a secret would be sent, or
	// the transport has no secret, and no secret sealed for the new
	// destination came with it.
	ErrNeedsSecret = errors.New("the destination changed; the secret must be set again for it")
)

// StoredSecret is a secrets row.
type StoredSecret struct {
	secret.Record
	Revision  int64
	CreatedAt time.Time
	SealedAt  time.Time
}

// Transport is a transports row. A placeholder (ConfiguredAt nil) has only
// a name: 0017 made one for every name identities used, and serve makes
// one for every name the environment configures (EnsureTransports).
type Transport struct {
	ID   string
	Name string
	Kind string
	// Host is in secret.CanonicalHost form; "" for a placeholder.
	Host     string
	Port     int
	Auth     string
	Username string
	// SecretID is "" when it has none.
	SecretID     string
	Caps         []string
	ConfiguredAt *time.Time
	DisabledAt   *time.Time
	Revision     int64
	CreatedAt    time.Time
}

// TransportConfig is what configuring a transport sets. Kind is always
// smtp for now.
type TransportConfig struct {
	Name string
	// Host is stored as secret.CanonicalHost(Host).
	Host string
	Port int
	// Auth is TransportAuthNone or TransportAuthPlain; plain needs a
	// Username and a secret.
	Auth     string
	Username string
	// Caps are required, possibly empty: they decide which header fields
	// are stripped from every message (§10.2), so there is no default.
	Caps []string
}

// transportCols are read FROM transports t.
const transportCols = `t.id, t.name, t.kind, coalesce(t.host, ''), coalesce(t.port, 0), coalesce(t.auth, ''), t.username,
	coalesce(t.secret_id, ''), t.caps, t.configured_at, t.disabled_at, t.revision, t.created_at`

func scanTransport(row pgx.Row) (Transport, error) {
	var t Transport
	err := row.Scan(&t.ID, &t.Name, &t.Kind, &t.Host, &t.Port, &t.Auth, &t.Username,
		&t.SecretID, &t.Caps, &t.ConfiguredAt, &t.DisabledAt, &t.Revision, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

// EnsureTransports makes a placeholder for each name that has no row yet,
// and leaves existing rows, configured or disabled, alone. serve and
// `identity add` call it with the names the environment configures
// (YUUKA_TRANSPORTS), so an identity can name a transport that so far
// exists only there (§17.7). It needs no key and stores no secret, and
// any number of processes may run it at once.
func (db *DB) EnsureTransports(ctx context.Context, names []string) error {
	for _, n := range names {
		if !ValidName(n) {
			return fmt.Errorf("transport name %q: want [A-Za-z0-9][A-Za-z0-9._-]*, at most 64", n)
		}
	}
	if len(names) == 0 {
		return nil
	}
	ids := make([]string, len(names))
	now := time.Now()
	for k := range names {
		ids[k] = ulid.New(now)
	}
	_, err := db.pool.Exec(ctx, `
		INSERT INTO transports (id, name)
		SELECT * FROM unnest($1::text[], $2::text[])
		ON CONFLICT (name) DO NOTHING`, ids, names)
	if err != nil {
		return fmt.Errorf("ensure transports: %w", err)
	}
	return nil
}

// Transports lists transports by name, disabled ones too when all.
func (db *DB) Transports(ctx context.Context, all bool) ([]Transport, error) {
	rows, err := db.pool.Query(ctx, `SELECT `+transportCols+` FROM transports t
		WHERE $1 OR t.disabled_at IS NULL ORDER BY t.name`, all)
	if err != nil {
		return nil, fmt.Errorf("list transports: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Transport, error) { return scanTransport(r) })
	if err != nil {
		return nil, fmt.Errorf("list transports: %w", err)
	}
	return out, nil
}

// TransportByName returns the transport called name, disabled or not.
func (db *DB) TransportByName(ctx context.Context, name string) (Transport, error) {
	t, err := scanTransport(db.pool.QueryRow(ctx, `SELECT `+transportCols+` FROM transports t WHERE t.name = $1`, name))
	if err != nil {
		return t, fmt.Errorf("transport %s: %w", name, err)
	}
	return t, nil
}

// ConfigureTransport creates (revision 0) or rewrites (revision = the one
// read) the transport c.Name, and records a. Rewriting a placeholder
// configures it; rewriting a disabled transport enables it again.
//
// sealed is a new secret for it, or nil to keep the one it has. A plain
// transport needs one whenever its destination (host, port, auth or
// username) changes, and whenever it has none (new, placeholder,
// disabled); otherwise ErrNeedsSecret. sealed must be an smtp_password
// sealed for secret.SMTPS(c.Username, c.Host, c.Port) under a fresh id.
// It replaces the old secret, which is deleted; a secret row's audience
// never changes. Switching to auth none deletes the secret.
//
// ErrDuplicate if revision is 0 and the name exists; ErrNotFound if
// revision is not 0 and it does not; ErrStale if its revision moved on.
func (db *DB) ConfigureTransport(ctx context.Context, c TransportConfig, revision int64, sealed *secret.Record, a Action) (Transport, error) {
	c, err := checkTransportConfig(c)
	if err != nil {
		return Transport{}, err
	}
	if sealed != nil {
		if err := checkTransportSecret(c, *sealed); err != nil {
			return Transport{}, fmt.Errorf("transport %s: %w", c.Name, err)
		}
	}
	var out Transport
	err = pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		var cur Transport
		bump := 1
		if revision == 0 {
			// Inserted bare and configured below, in one revision.
			bump = 0
			cur = Transport{ID: ulid.New(a.At), Name: c.Name}
			_, err := tx.Exec(ctx, `INSERT INTO transports (id, name) VALUES ($1, $2)`, cur.ID, cur.Name)
			if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
				return ErrDuplicate
			}
			if err != nil {
				return err
			}
		} else {
			cur, err = scanTransport(tx.QueryRow(ctx, `SELECT `+transportCols+` FROM transports t WHERE t.name = $1 FOR UPDATE`, c.Name))
			if err != nil {
				return err
			}
			if cur.Revision != revision {
				return fmt.Errorf("revision %d, read %d: %w", cur.Revision, revision, ErrStale)
			}
		}
		secretID := cur.SecretID
		switch {
		case c.Auth == TransportAuthNone:
			secretID = ""
		case sealed != nil:
			secretID = sealed.ID
		case secretID == "" || cur.Host != c.Host || cur.Port != c.Port || cur.Auth != c.Auth || cur.Username != c.Username:
			return ErrNeedsSecret
		}
		if sealed != nil {
			if err := insertSecret(ctx, tx, *sealed, a.At); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `
			UPDATE transports SET host = $2, port = $3, auth = $4, username = $5, caps = $6, secret_id = $7,
				configured_at = coalesce(configured_at, $8), disabled_at = NULL, revision = revision + $9
			WHERE id = $1`,
			cur.ID, c.Host, c.Port, c.Auth, c.Username, c.Caps, nullable(secretID), a.At, bump)
		if err != nil {
			return err
		}
		if cur.SecretID != "" && cur.SecretID != secretID {
			if _, err := tx.Exec(ctx, `DELETE FROM secrets WHERE id = $1`, cur.SecretID); err != nil {
				return err
			}
		}
		if err := insertAction(ctx, tx, a); err != nil {
			return err
		}
		out, err = scanTransport(tx.QueryRow(ctx, `SELECT `+transportCols+` FROM transports t WHERE t.id = $1`, cur.ID))
		return err
	})
	if err != nil {
		return Transport{}, fmt.Errorf("configure transport %s: %w", c.Name, err)
	}
	return out, nil
}

// DisableTransport is `transport rm`: it disables the transport name at
// revision, erases its secret and records a. The row stays, so the
// names in send_attempts.transport still resolve; identities whose chain
// names it keep naming it (§10.2.2 says what sending does with that).
// ErrNotFound if there is no enabled transport of that name; ErrStale
// if its revision moved on.
func (db *DB) DisableTransport(ctx context.Context, name string, revision int64, a Action) error {
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		cur, err := scanTransport(tx.QueryRow(ctx, `SELECT `+transportCols+` FROM transports t
			WHERE t.name = $1 AND t.disabled_at IS NULL FOR UPDATE`, name))
		if err != nil {
			return err
		}
		if cur.Revision != revision {
			return fmt.Errorf("revision %d, read %d: %w", cur.Revision, revision, ErrStale)
		}
		if _, err := tx.Exec(ctx, `UPDATE transports SET disabled_at = $2, secret_id = NULL, revision = revision + 1
			WHERE id = $1`, cur.ID, a.At); err != nil {
			return err
		}
		if cur.SecretID != "" {
			if _, err := tx.Exec(ctx, `DELETE FROM secrets WHERE id = $1`, cur.SecretID); err != nil {
				return err
			}
		}
		return insertAction(ctx, tx, a)
	})
	if err != nil {
		return fmt.Errorf("disable transport %s: %w", name, err)
	}
	return nil
}

func checkTransportConfig(c TransportConfig) (TransportConfig, error) {
	if !ValidName(c.Name) {
		return c, fmt.Errorf("transport name %q: want [A-Za-z0-9][A-Za-z0-9._-]*, at most 64", c.Name)
	}
	h, err := secret.CanonicalHost(c.Host)
	if err != nil {
		return c, fmt.Errorf("transport %s: %w", c.Name, err)
	}
	c.Host = h
	if c.Port < 1 || c.Port > 65535 {
		return c, fmt.Errorf("transport %s: port %d out of range", c.Name, c.Port)
	}
	switch c.Auth {
	case TransportAuthPlain:
		if c.Username == "" {
			return c, fmt.Errorf("transport %s: auth plain needs a username", c.Name)
		}
	case TransportAuthNone:
		if c.Username != "" {
			return c, fmt.Errorf("transport %s: a username needs auth plain", c.Name)
		}
	default:
		return c, fmt.Errorf("transport %s: auth %q: want none or plain", c.Name, c.Auth)
	}
	if c.Caps == nil {
		return c, fmt.Errorf("transport %s: caps are required (possibly none)", c.Name)
	}
	caps := make([]string, 0, len(c.Caps))
	for _, x := range c.Caps {
		if !slices.Contains(transportCaps, x) {
			return c, fmt.Errorf("transport %s: unknown cap %q", c.Name, x)
		}
		if !slices.Contains(caps, x) {
			caps = append(caps, x)
		}
	}
	c.Caps = caps
	return c, nil
}

// checkTransportSecret checks that s was sealed for c's destination. It
// is not a security boundary (opening recomputes the audience anyway),
// but it turns a wrong seal into an error now rather than a locked
// transport later.
func checkTransportSecret(c TransportConfig, s secret.Record) error {
	if c.Auth != TransportAuthPlain {
		return errors.New("a secret needs auth plain")
	}
	if s.Kind != SecretSMTPPassword {
		return fmt.Errorf("secret kind %q, want %s", s.Kind, SecretSMTPPassword)
	}
	want, err := secret.SMTPS(c.Username, c.Host, c.Port)
	if err != nil {
		return err
	}
	if s.Audience != want {
		return fmt.Errorf("secret sealed for %q, not for this transport's %q", s.Audience, want)
	}
	return nil
}

func insertSecret(ctx context.Context, tx pgx.Tx, r secret.Record, at time.Time) error {
	if !ulid.Valid(r.ID) {
		return fmt.Errorf("secret id %q is not a ULID", r.ID)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO secrets (id, kind, audience, wrapper, key_ref, wrapped_dek, payload, created_at, sealed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`,
		r.ID, r.Kind, r.Audience, r.Wrapper, r.KeyRef, r.WrappedDEK, r.Payload, at)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		return fmt.Errorf("secret %s: %w", r.ID, ErrDuplicate)
	}
	if err != nil {
		return fmt.Errorf("insert secret: %w", err)
	}
	return nil
}

const secretCols = `id, kind, audience, wrapper, key_ref, wrapped_dek, payload, revision, created_at, sealed_at`

func scanSecret(row pgx.Row) (StoredSecret, error) {
	var s StoredSecret
	err := row.Scan(&s.ID, &s.Kind, &s.Audience, &s.Wrapper, &s.KeyRef, &s.WrappedDEK, &s.Payload,
		&s.Revision, &s.CreatedAt, &s.SealedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, ErrNotFound
	}
	return s, err
}

// Secret returns the secret id.
func (db *DB) Secret(ctx context.Context, id string) (StoredSecret, error) {
	s, err := scanSecret(db.pool.QueryRow(ctx, `SELECT `+secretCols+` FROM secrets WHERE id = $1`, id))
	if err != nil {
		return s, fmt.Errorf("secret %s: %w", id, err)
	}
	return s, nil
}

// Secrets lists every secret, oldest first, for `secret ls`, `check` and
// `rewrap`.
func (db *DB) Secrets(ctx context.Context) ([]StoredSecret, error) {
	rows, err := db.pool.Query(ctx, `SELECT `+secretCols+` FROM secrets ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (StoredSecret, error) { return scanSecret(r) })
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	return out, nil
}

// UpdateSecret overwrites secret r.ID at revision with r, and records a:
// a new value for the same destination (r sealed again under the same id),
// or the same value rewrapped (`secret rewrap`). Kind and audience must be
// those of the row: a new destination is a new secret, through its
// owner (ConfigureTransport). sealed_at moves only when the payload
// changes. It returns the new revision. ErrNotFound, or ErrStale if the
// revision moved on.
func (db *DB) UpdateSecret(ctx context.Context, r secret.Record, revision int64, a Action) (int64, error) {
	var next int64
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		cur, err := scanSecret(tx.QueryRow(ctx, `SELECT `+secretCols+` FROM secrets WHERE id = $1 FOR UPDATE`, r.ID))
		if err != nil {
			return err
		}
		if cur.Revision != revision {
			return fmt.Errorf("revision %d, read %d: %w", cur.Revision, revision, ErrStale)
		}
		if cur.Kind != r.Kind || cur.Audience != r.Audience {
			return errors.New("a secret's kind and audience never change; set a new secret through its owner")
		}
		sealedAt := cur.SealedAt
		if !bytes.Equal(cur.Payload, r.Payload) {
			sealedAt = a.At
		}
		err = tx.QueryRow(ctx, `
			UPDATE secrets SET wrapper = $2, key_ref = $3, wrapped_dek = $4, payload = $5, sealed_at = $6,
				revision = revision + 1
			WHERE id = $1 RETURNING revision`,
			r.ID, r.Wrapper, r.KeyRef, r.WrappedDEK, r.Payload, sealedAt).Scan(&next)
		if err != nil {
			return err
		}
		return insertAction(ctx, tx, a)
	})
	if err != nil {
		return 0, fmt.Errorf("update secret %s: %w", r.ID, err)
	}
	return next, nil
}

// SetIdentityTransports replaces the transport chain of the enabled
// identity id with names, primary first, and records a. The names must
// be enabled transports, placeholders included (ErrNoTransport), at most
// MaxTransportChain and each once. ErrNotFound if the identity is not
// enabled.
func (db *DB) SetIdentityTransports(ctx context.Context, id string, names []string, a Action) error {
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		var one int
		err := tx.QueryRow(ctx, `SELECT 1 FROM identities WHERE id = $1 AND disabled_at IS NULL FOR UPDATE`, id).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := setChain(ctx, tx, id, names); err != nil {
			return err
		}
		return insertAction(ctx, tx, a)
	})
	if err != nil {
		return fmt.Errorf("set transports of identity %s: %w", id, err)
	}
	return nil
}

// setChain writes identity id's chain as names, positions 0..n-1.
// Contiguity and the bound are checked here: the table can only bound
// each position.
func setChain(ctx context.Context, tx pgx.Tx, id string, names []string) error {
	switch {
	case len(names) == 0:
		return errors.New("an identity needs at least one transport")
	case len(names) > MaxTransportChain:
		return fmt.Errorf("%d transports, at most %d", len(names), MaxTransportChain)
	}
	for k, n := range names {
		if slices.Contains(names[:k], n) {
			return fmt.Errorf("transport %s is named twice", n)
		}
	}
	rows, err := tx.Query(ctx, `
		SELECT n.name, t.id FROM unnest($1::text[]) WITH ORDINALITY AS n (name, pos)
		LEFT JOIN transports t ON t.name = n.name AND t.disabled_at IS NULL
		ORDER BY n.pos`, names)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(names))
	var name string
	var tid *string
	_, err = pgx.ForEachRow(rows, []any{&name, &tid}, func() error {
		if tid == nil {
			return fmt.Errorf("transport %s: %w", name, ErrNoTransport)
		}
		ids = append(ids, *tid)
		return nil
	})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM identity_transports WHERE identity_id = $1`, id); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO identity_transports (identity_id, position, transport_id)
		SELECT $1, pos - 1, tid FROM unnest($2::text[]) WITH ORDINALITY AS c (tid, pos)`, id, ids)
	return err
}

// IdentityTransports returns the chain of identity id, primary first.
func (db *DB) IdentityTransports(ctx context.Context, id string) ([]Transport, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT `+transportCols+`
		FROM identity_transports it JOIN transports t ON t.id = it.transport_id
		WHERE it.identity_id = $1 ORDER BY it.position`, id)
	if err != nil {
		return nil, fmt.Errorf("transports of identity %s: %w", id, err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Transport, error) { return scanTransport(r) })
	if err != nil {
		return nil, fmt.Errorf("transports of identity %s: %w", id, err)
	}
	return out, nil
}
