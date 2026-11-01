package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Pull accounts (§9.5, §10.1.3, migration 0019): their sources, how far
// each remote folder has been read, the commit of one pulled message,
// the failures a cursor passed, and the UIDVALIDITY remap.
//
// None of this takes a Viewer. The source configuration is for the
// host's CLI and human sessions (§13.10), and no agent-facing surface
// reads it (§9.4); the rest is the sync worker's, a system batch. What
// pulled mail any principal may read is decided by readableAccount like
// every other delivery.
//
// The invariant all of it serves is §3.5 for pulling: a cursor never
// passes mail that is neither committed nor recorded in pull_failures.
// The worker commits each message (CommitPull) before it moves a cursor
// past it (AdvanceHigh, AdvanceLow), and the cursor moves only by
// compare-and-set, so a worker that read stale state cannot move it
// backwards or across a UIDVALIDITY change.

// SecretIMAPPassword is the secrets.kind of a pull source's password,
// sealed for secret.IMAPS(username, host, port).
const SecretIMAPPassword = "imap_password"

// SourceAuthPlain is the only pull authentication so far (M3.5b adds
// xoauth2).
const SourceAuthPlain = "plain"

// PresetGmail selects the Gmail differences fixed in code (§10.1): sync
// \All only, require X-GM-EXT-1.
const PresetGmail = "gmail"

// Why a source is paused (account_sources.pause_reason).
const (
	PauseOperator   = "operator"
	PauseAuthFailed = "auth_failed"
)

// MinPollSeconds bounds how often a source may be polled.
const MinPollSeconds = 10

var (
	// ErrStaleMailbox: the folder's UIDVALIDITY (or the location being
	// rewritten) is no longer the one the caller read.
	ErrStaleMailbox = errors.New("the mailbox changed since it was read")
	// ErrSourceKey: a source key does not have the form of its folder's
	// key kind (§10.1).
	ErrSourceKey = errors.New("source key does not match the folder's key kind")
)

// ---------------------------------------------------------------------------
// Source keys

// KeyKind says how a folder's source keys are made (§10.1,
// source_mailboxes.key_kind). It is fixed on the folder's first sync.
type KeyKind int

const (
	// KeyLocation: loc:<uidvalidity>:<uid>:<mailbox>, for servers with
	// no stable id. It changes when the message moves or UIDVALIDITY
	// does.
	KeyLocation KeyKind = 1
	// KeyObjectID: oid:<EMAILID>, for servers with OBJECTID (RFC 8474).
	KeyObjectID KeyKind = 2
	// KeyGmail: gm:<X-GM-MSGID in 16 lowercase hex digits>.
	KeyGmail KeyKind = 3
)

// The source key forms are a one-way door (§10.1): (account_id,
// source_key) is the idempotency key, so once an account has mail
// under one spelling a different one would ingest it all again.

// GmailSourceKey is the source key of the Gmail message msgid
// (X-GM-MSGID).
func GmailSourceKey(msgid uint64) string { return fmt.Sprintf("gm:%016x", msgid) }

// ObjectIDSourceKey is the source key of the message whose EMAILID is
// emailID. It must be an RFC 8474 objectid: 1 to 255 of ALPHA, DIGIT,
// "_" and "-".
func ObjectIDSourceKey(emailID string) (string, error) {
	if !validObjectID(emailID) {
		return "", fmt.Errorf("EMAILID %q is not an RFC 8474 objectid", emailID)
	}
	return "oid:" + emailID, nil
}

// LocationSourceKey is the source key of the message at uid in mailbox
// under uidvalidity. The mailbox goes last, so it needs no escaping:
// the first two fields are numbers.
func LocationSourceKey(uidvalidity, uid uint32, mailbox string) string {
	return "loc:" + strconv.FormatUint(uint64(uidvalidity), 10) + ":" + strconv.FormatUint(uint64(uid), 10) + ":" + mailbox
}

// objectIDChars are the characters of an RFC 8474 objectid.
const objectIDChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"

func validObjectID(s string) bool {
	if len(s) == 0 || len(s) > 255 {
		return false
	}
	return strings.Trim(s, objectIDChars) == ""
}

// checkSourceKey checks that key has the form kind makes for the
// message at loc.
func checkSourceKey(kind KeyKind, key string, loc RemoteLocation) error {
	ok := false
	switch kind {
	case KeyLocation:
		ok = key == LocationSourceKey(loc.UIDValidity, loc.UID, loc.Mailbox)
	case KeyObjectID:
		id, found := strings.CutPrefix(key, "oid:")
		ok = found && validObjectID(id)
	case KeyGmail:
		hex, found := strings.CutPrefix(key, "gm:")
		ok = found && len(hex) == 16 && strings.Trim(hex, "0123456789abcdef") == ""
	}
	if !ok {
		return fmt.Errorf("%q for key kind %d: %w", key, kind, ErrSourceKey)
	}
	return nil
}

// RemoteLocation is where a pulled message is on the remote (§10.1):
// meaningful only as a whole, and not an identity, since it changes.
type RemoteLocation struct {
	Mailbox     string
	UIDValidity uint32
	UID         uint32
}

// ---------------------------------------------------------------------------
// Sources

// SourceConfig is what configuring a pull source sets (§9.5). Every
// field is stated; the defaults (a Gmail preset's budget, the poll
// interval) are the caller's, so that what was configured is what is
// stored.
type SourceConfig struct {
	// Address is the mailbox's own address, stored lowercase: pulled
	// deliveries are delivered to it (§9.3).
	Address string
	// Host is stored as secret.CanonicalHost(Host).
	Host     string
	Port     int
	Auth     string // SourceAuthPlain
	Username string
	Preset   string // "" or PresetGmail
	// Mailboxes are the folders to sync by their UTF-8 names; nil means
	// the default for the preset.
	Mailboxes []string
	// TrustedAuthservID is whose Authentication-Results this account's
	// mail trusts (§10.1.1); "" trusts none.
	TrustedAuthservID string
	MaxMessageBytes   int64
	// BackfillSince stops backfill at older mail; nil backfills all.
	BackfillSince *time.Time
	// DailyByteBudget is 0 for unlimited.
	DailyByteBudget int64
	PollSeconds     int
}

// Source is an account_sources row with its account's name.
type Source struct {
	AccountID   string
	AccountName string
	Kind        string
	SourceConfig
	SecretID    string // "" once removed
	PausedAt    *time.Time
	PauseReason string // "" when not paused
	DisabledAt  *time.Time
	Revision    int64
	CreatedAt   time.Time
}

// Zone and Prefix are the (zone, prefix) of the source's address, which
// its deliveries carry and `account add` claims (§9.3).
func (s Source) Zone() string { z, _ := splitAddress(s.Address); return z }

// Prefix: see Zone.
func (s Source) Prefix() string { _, p := splitAddress(s.Address); return p }

// Audience is the audience the source's secret must have been sealed
// for (§17.2).
func (s Source) Audience() (string, error) { return secret.IMAPS(s.Username, s.Host, s.Port) }

// splitAddress splits a lowercase address at its last "@".
func splitAddress(addr string) (zone, prefix string) {
	i := strings.LastIndexByte(addr, '@')
	if i < 0 {
		return "", addr
	}
	return addr[i+1:], addr[:i]
}

const sourceCols = `s.account_id, a.name, s.kind, s.address, s.host, s.port, s.auth, s.username, coalesce(s.preset, ''),
	s.mailboxes, coalesce(s.trusted_authserv_id, ''), s.max_message_bytes, s.backfill_since, coalesce(s.daily_byte_budget, 0),
	s.poll_seconds, coalesce(s.secret_id, ''), s.paused_at, coalesce(s.pause_reason, ''), s.disabled_at, s.revision, s.created_at`

const sourceFrom = `account_sources s JOIN accounts a ON a.id = s.account_id`

func scanSourceInto(row pgx.Row, s *Source, more ...any) error {
	c := &s.SourceConfig
	err := row.Scan(append([]any{
		&s.AccountID, &s.AccountName, &s.Kind, &c.Address, &c.Host, &c.Port, &c.Auth, &c.Username, &c.Preset,
		&c.Mailboxes, &c.TrustedAuthservID, &c.MaxMessageBytes, &c.BackfillSince, &c.DailyByteBudget,
		&c.PollSeconds, &s.SecretID, &s.PausedAt, &s.PauseReason, &s.DisabledAt, &s.Revision, &s.CreatedAt,
	}, more...)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func scanSource(row pgx.Row) (Source, error) {
	var s Source
	err := scanSourceInto(row, &s)
	return s, err
}

// CheckSourceConfig checks c as AddPullAccount and ConfigureSource do
// and returns it as it would be stored (address lowercase, host
// canonical), so a caller can tell what a write changes (§13.10) before
// making it.
func CheckSourceConfig(c SourceConfig) (SourceConfig, error) {
	c.Address = strings.ToLower(c.Address)
	if i := strings.LastIndexByte(c.Address, '@'); i <= 0 || i == len(c.Address)-1 ||
		strings.Count(c.Address, "@") != 1 || strings.IndexFunc(c.Address, unicode.IsSpace) >= 0 ||
		len(c.Address) > 320 || !utf8.ValidString(c.Address) {
		return c, fmt.Errorf("source address %q: want local@domain", c.Address)
	}
	h, err := secret.CanonicalHost(c.Host)
	if err != nil {
		return c, fmt.Errorf("source %s: %w", c.Address, err)
	}
	c.Host = h
	switch {
	case c.Port < 1 || c.Port > 65535:
		return c, fmt.Errorf("source %s: port %d out of range", c.Address, c.Port)
	case c.Auth != SourceAuthPlain:
		return c, fmt.Errorf("source %s: auth %q: want %s", c.Address, c.Auth, SourceAuthPlain)
	case c.Username == "":
		return c, fmt.Errorf("source %s: auth plain needs a username", c.Address)
	case c.Preset != "" && c.Preset != PresetGmail:
		return c, fmt.Errorf("source %s: preset %q: want %s or none", c.Address, c.Preset, PresetGmail)
	case c.MaxMessageBytes <= 0:
		return c, fmt.Errorf("source %s: max message bytes %d: want more than 0", c.Address, c.MaxMessageBytes)
	case c.DailyByteBudget < 0:
		return c, fmt.Errorf("source %s: daily byte budget %d: want 0 (unlimited) or more", c.Address, c.DailyByteBudget)
	case c.PollSeconds < MinPollSeconds || c.PollSeconds > 86400:
		return c, fmt.Errorf("source %s: poll interval %ds: want %d to 86400", c.Address, c.PollSeconds, MinPollSeconds)
	case len(c.TrustedAuthservID) > 255 || strings.IndexFunc(c.TrustedAuthservID, func(r rune) bool {
		return r <= ' ' || r == 0x7f || r == utf8.RuneError
	}) >= 0:
		return c, fmt.Errorf("source %s: authserv-id %q: want a token of at most 255 bytes", c.Address, c.TrustedAuthservID)
	}
	if c.Mailboxes != nil {
		if len(c.Mailboxes) == 0 {
			return c, fmt.Errorf("source %s: no mailboxes; leave them unset for the default", c.Address)
		}
		for k, m := range c.Mailboxes {
			if m == "" || len(m) > 1024 || !utf8.ValidString(m) || strings.ContainsRune(m, 0) {
				return c, fmt.Errorf("source %s: mailbox %q: want a UTF-8 name of at most 1024 bytes", c.Address, m)
			}
			if slices.Contains(c.Mailboxes[:k], m) {
				return c, fmt.Errorf("source %s: mailbox %q named twice", c.Address, m)
			}
		}
	}
	return c, nil
}

// checkSourceSecret checks that s was sealed for c's destination. Like
// checkTransportSecret it is not a security boundary (opening recomputes
// the audience), but it reports a wrong seal now rather than a source
// that cannot log in later.
func checkSourceSecret(c SourceConfig, s secret.Record) error {
	if s.Kind != SecretIMAPPassword {
		return fmt.Errorf("secret kind %q, want %s", s.Kind, SecretIMAPPassword)
	}
	want, err := secret.IMAPS(c.Username, c.Host, c.Port)
	if err != nil {
		return err
	}
	if s.Audience != want {
		return fmt.Errorf("secret sealed for %q, not for this source's %q", s.Audience, want)
	}
	return nil
}

// AddPullAccount is `account add`: it creates the pull account name with
// source c and its password sealed, gives reader (the person adding it)
// an account_access row that lets them read it, and records a. A pull
// account is not readable by default (§13.8): adding a mailbox does not
// hand it to everyone else. Claiming the address's prefix (§9.3) is
// SetPrefixState's, and the caller's to do as well.
//
// sealed must be an imap_password sealed for secret.IMAPS(c.Username,
// c.Host, c.Port) under a fresh id. ErrDuplicate if an account has the
// name (removed accounts keep theirs) or an enabled source has the
// address; ErrNotFound if reader is no principal.
func (db *DB) AddPullAccount(ctx context.Context, name string, c SourceConfig, sealed secret.Record, reader string, a Action) (Source, error) {
	if !ValidName(name) {
		return Source{}, fmt.Errorf("account name %q: want [A-Za-z0-9][A-Za-z0-9._-]*, at most 64", name)
	}
	c, err := CheckSourceConfig(c)
	if err != nil {
		return Source{}, err
	}
	if err := checkSourceSecret(c, sealed); err != nil {
		return Source{}, fmt.Errorf("account %s: %w", name, err)
	}
	var out Source
	err = pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM principals WHERE id = $1`, reader).Scan(&one); errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("principal %s: %w", reader, ErrNotFound)
		} else if err != nil {
			return err
		}
		id := ulid.New(a.At)
		if _, err := tx.Exec(ctx, `INSERT INTO accounts (id, name, kind, default_read, created_at) VALUES ($1, $2, $3, false, $4)`,
			id, name, AccountIMAP, a.At); err != nil {
			return uniqueAsDuplicate(err)
		}
		if err := insertSecret(ctx, tx, sealed, a.At); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO account_sources (account_id, kind, address, host, port, auth, username, secret_id, preset, mailboxes,
				trusted_authserv_id, max_message_bytes, backfill_since, daily_byte_budget, poll_seconds, created_at)
			VALUES ($1, 'imap', $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
			id, c.Address, c.Host, c.Port, c.Auth, c.Username, sealed.ID, nullable(c.Preset), c.Mailboxes,
			nullable(c.TrustedAuthservID), c.MaxMessageBytes, c.BackfillSince, nullableInt(c.DailyByteBudget), c.PollSeconds, a.At); err != nil {
			return uniqueAsDuplicate(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO account_access (account_id, principal_id, can_read) VALUES ($1, $2, true)`,
			id, reader); err != nil {
			return err
		}
		if err := insertAction(ctx, tx, a); err != nil {
			return err
		}
		out, err = scanSource(tx.QueryRow(ctx, `SELECT `+sourceCols+` FROM `+sourceFrom+` WHERE s.account_id = $1`, id))
		return err
	})
	if err != nil {
		return Source{}, fmt.Errorf("add account %s: %w", name, err)
	}
	return out, nil
}

// ConfigureSource is `account set`: it rewrites the enabled source of
// account name at revision with c, and records a.
//
// sealed is a new password, or nil to keep the one it has. A change of
// destination (host, port, auth or username) needs one, sealed for the
// new destination under a fresh id (ErrNeedsSecret); it replaces the old
// secret, which is deleted, as for transports (§17.4). A new password
// also ends a pause for auth_failed (§10.1.3); an operator's pause stays.
//
// ErrNotFound if the account has no enabled source; ErrStale if its
// revision moved on; ErrDuplicate if another enabled source has the
// address.
func (db *DB) ConfigureSource(ctx context.Context, name string, c SourceConfig, revision int64, sealed *secret.Record, a Action) (Source, error) {
	c, err := CheckSourceConfig(c)
	if err != nil {
		return Source{}, err
	}
	if sealed != nil {
		if err := checkSourceSecret(c, *sealed); err != nil {
			return Source{}, fmt.Errorf("account %s: %w", name, err)
		}
	}
	var out Source
	err = pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		cur, err := lockSource(ctx, tx, name, revision)
		if err != nil {
			return err
		}
		secretID := cur.SecretID
		if sealed != nil {
			if err := insertSecret(ctx, tx, *sealed, a.At); err != nil {
				return err
			}
			secretID = sealed.ID
		} else if cur.Host != c.Host || cur.Port != c.Port || cur.Auth != c.Auth || cur.Username != c.Username {
			return ErrNeedsSecret
		}
		if _, err := tx.Exec(ctx, `
			UPDATE account_sources SET address = $2, host = $3, port = $4, auth = $5, username = $6, secret_id = $7,
				preset = $8, mailboxes = $9, trusted_authserv_id = $10, max_message_bytes = $11, backfill_since = $12,
				daily_byte_budget = $13, poll_seconds = $14,
				paused_at = CASE WHEN $15 AND pause_reason = 'auth_failed' THEN NULL ELSE paused_at END,
				pause_reason = CASE WHEN $15 AND pause_reason = 'auth_failed' THEN NULL ELSE pause_reason END,
				revision = revision + 1
			WHERE account_id = $1`,
			cur.AccountID, c.Address, c.Host, c.Port, c.Auth, c.Username, secretID, nullable(c.Preset), c.Mailboxes,
			nullable(c.TrustedAuthservID), c.MaxMessageBytes, c.BackfillSince, nullableInt(c.DailyByteBudget), c.PollSeconds,
			sealed != nil); err != nil {
			return uniqueAsDuplicate(err)
		}
		if cur.SecretID != secretID {
			if _, err := tx.Exec(ctx, `DELETE FROM secrets WHERE id = $1`, cur.SecretID); err != nil {
				return err
			}
		}
		if err := insertAction(ctx, tx, a); err != nil {
			return err
		}
		out, err = scanSource(tx.QueryRow(ctx, `SELECT `+sourceCols+` FROM `+sourceFrom+` WHERE s.account_id = $1`, cur.AccountID))
		return err
	})
	if err != nil {
		return Source{}, fmt.Errorf("configure account %s: %w", name, err)
	}
	return out, nil
}

// DisableSource is `account rm`: it disables the source of account name
// at revision, erases its password, marks the account removed and
// records a. The account, its deliveries, raw objects, folder cursors and
// failures stay, and the name is never reused (§9.5). ErrNotFound if
// the account has no enabled source; ErrStale if its revision moved on.
func (db *DB) DisableSource(ctx context.Context, name string, revision int64, a Action) error {
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		cur, err := lockSource(ctx, tx, name, revision)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE account_sources SET disabled_at = $2, secret_id = NULL,
				sync_requested_at = NULL, retry_requested_at = NULL, revision = revision + 1
			WHERE account_id = $1`, cur.AccountID, a.At); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM secrets WHERE id = $1`, cur.SecretID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE accounts SET disabled_at = $2 WHERE id = $1`, cur.AccountID, a.At); err != nil {
			return err
		}
		return insertAction(ctx, tx, a)
	})
	if err != nil {
		return fmt.Errorf("remove account %s: %w", name, err)
	}
	return nil
}

// PauseSource pauses the enabled source of account name at revision for
// reason, and records a. The operator pauses with PauseOperator; the
// worker with PauseAuthFailed when login is refused, at the revision it
// logged in with, so a password changed meanwhile is not paused for the
// old one's failure (ErrStale). An auth_failed pause never replaces an
// operator's, which only resume ends. ErrNotFound if the account has no
// enabled source.
func (db *DB) PauseSource(ctx context.Context, name string, revision int64, reason string, a Action) error {
	if reason != PauseOperator && reason != PauseAuthFailed {
		return fmt.Errorf("pause reason %q", reason)
	}
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		cur, err := lockSource(ctx, tx, name, revision)
		if err != nil {
			return err
		}
		if cur.PauseReason == reason || cur.PauseReason == PauseOperator && reason == PauseAuthFailed {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE account_sources SET paused_at = coalesce(paused_at, $2), pause_reason = $3,
				revision = revision + 1
			WHERE account_id = $1`, cur.AccountID, a.At, reason); err != nil {
			return err
		}
		return insertAction(ctx, tx, a)
	})
	if err != nil {
		return fmt.Errorf("pause account %s: %w", name, err)
	}
	return nil
}

// ResumeSource ends the pause of the enabled source of account name at
// revision, whatever its reason, and records a; a source that is not
// paused is left alone. ErrNotFound if the account has no enabled
// source; ErrStale if its revision moved on.
func (db *DB) ResumeSource(ctx context.Context, name string, revision int64, a Action) error {
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		cur, err := lockSource(ctx, tx, name, revision)
		if err != nil || cur.PausedAt == nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE account_sources SET paused_at = NULL, pause_reason = NULL, revision = revision + 1
			WHERE account_id = $1`, cur.AccountID); err != nil {
			return err
		}
		return insertAction(ctx, tx, a)
	})
	if err != nil {
		return fmt.Errorf("resume account %s: %w", name, err)
	}
	return nil
}

// lockSource locks the enabled source of account name and checks that
// it is still at revision.
func lockSource(ctx context.Context, tx pgx.Tx, name string, revision int64) (Source, error) {
	cur, err := scanSource(tx.QueryRow(ctx, `SELECT `+sourceCols+` FROM `+sourceFrom+`
		WHERE a.name = $1 AND s.disabled_at IS NULL FOR UPDATE OF s`, name))
	if err != nil {
		return cur, err
	}
	if cur.Revision != revision {
		return cur, fmt.Errorf("revision %d, read %d: %w", cur.Revision, revision, ErrStale)
	}
	return cur, nil
}

// SourceByName returns the source of account name, removed or not.
// ErrNotFound if the account has none (a Cloudflare account).
func (db *DB) SourceByName(ctx context.Context, name string) (Source, error) {
	s, err := scanSource(db.pool.QueryRow(ctx, `SELECT `+sourceCols+` FROM `+sourceFrom+` WHERE a.name = $1`, name))
	if err != nil {
		return s, fmt.Errorf("source of account %s: %w", name, err)
	}
	return s, nil
}

// Sources lists the sources by account name, removed ones too when all.
func (db *DB) Sources(ctx context.Context, all bool) ([]Source, error) {
	rows, err := db.pool.Query(ctx, `SELECT `+sourceCols+` FROM `+sourceFrom+`
		WHERE $1 OR s.disabled_at IS NULL ORDER BY a.name`, all)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Source, error) { return scanSource(r) })
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	return out, nil
}

// SourceWithSecret is an enabled source and its password's secret, for
// the source manager and its workers (§10.1.3), which open it.
type SourceWithSecret struct {
	Source
	Secret StoredSecret
}

// EnabledSources returns every enabled source with its secret, by
// account name, paused ones included: the manager stops a worker whose
// source is paused, and starts it again on resume, by the revisions.
func (db *DB) EnabledSources(ctx context.Context) ([]SourceWithSecret, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT `+sourceCols+`, x.id, x.kind, x.audience, x.wrapper, x.key_ref, x.wrapped_dek, x.payload,
			x.revision, x.created_at, x.sealed_at
		FROM `+sourceFrom+` JOIN secrets x ON x.id = s.secret_id
		WHERE s.disabled_at IS NULL
		ORDER BY a.name`)
	if err != nil {
		return nil, fmt.Errorf("enabled sources: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (SourceWithSecret, error) {
		var s SourceWithSecret
		x := &s.Secret
		err := scanSourceInto(r, &s.Source, &x.ID, &x.Kind, &x.Audience, &x.Wrapper, &x.KeyRef, &x.WrappedDEK, &x.Payload,
			&x.Revision, &x.CreatedAt, &x.SealedAt)
		return s, err
	})
	if err != nil {
		return nil, fmt.Errorf("enabled sources: %w", err)
	}
	return out, nil
}

// TrustedAuthservIDs maps the id of every account with a source, removed
// ones included, to its trusted authserv-id (§10.1.1), "" for none.
// Ingest and reparse fold a delivery's auth with its own account's
// value (§4.2); accounts absent from the map are Cloudflare accounts,
// whose value is the host's YUUKA_TRUSTED_AUTHSERV_ID.
func (db *DB) TrustedAuthservIDs(ctx context.Context) (map[string]string, error) {
	rows, err := db.pool.Query(ctx, `SELECT account_id, coalesce(trusted_authserv_id, '') FROM account_sources`)
	if err != nil {
		return nil, fmt.Errorf("trusted authserv-ids: %w", err)
	}
	out := map[string]string{}
	var id, authserv string
	if _, err := pgx.ForEachRow(rows, []any{&id, &authserv}, func() error {
		out[id] = authserv
		return nil
	}); err != nil {
		return nil, fmt.Errorf("trusted authserv-ids: %w", err)
	}
	return out, nil
}

// RequestSync is `account sync`: it asks serve to run a round of the
// enabled source of account name now, and with retryFailed to fetch its
// open failures again (§10.1.3). The CLI never pulls itself. ErrNotFound
// if the account has no enabled source.
func (db *DB) RequestSync(ctx context.Context, name string, retryFailed bool, at time.Time) error {
	tag, err := db.pool.Exec(ctx, `
		UPDATE account_sources s SET sync_requested_at = $2,
			retry_requested_at = CASE WHEN $3 THEN $2 ELSE s.retry_requested_at END
		FROM accounts a
		WHERE a.id = s.account_id AND a.name = $1 AND s.disabled_at IS NULL`, name, at, retryFailed)
	if err == nil && tag.RowsAffected() == 0 {
		err = ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("request sync of account %s: %w", name, err)
	}
	return nil
}

// SyncRequest is what TakeSyncRequest found.
type SyncRequest struct {
	Sync        bool // a round was asked for
	RetryFailed bool // with the open failures fetched again
}

// TakeSyncRequest reads and clears the requests for account id, at most
// once each: the worker calls it between rounds.
func (db *DB) TakeSyncRequest(ctx context.Context, accountID string) (SyncRequest, error) {
	var r SyncRequest
	err := db.pool.QueryRow(ctx, `
		WITH old AS (SELECT account_id, sync_requested_at, retry_requested_at FROM account_sources
			WHERE account_id = $1 FOR UPDATE)
		UPDATE account_sources s SET sync_requested_at = NULL, retry_requested_at = NULL
		FROM old WHERE s.account_id = old.account_id
		RETURNING old.sync_requested_at IS NOT NULL, old.retry_requested_at IS NOT NULL`, accountID).Scan(&r.Sync, &r.RetryFailed)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	if err != nil {
		return r, fmt.Errorf("take sync request of %s: %w", accountID, err)
	}
	return r, nil
}

// AddBudget counts n more bytes fetched for account id on day (its UTC
// date) and returns the total for that day; a new day starts again from
// n. The daily budget is checked against it (§10.1.3), so a restart of
// serve does not reset it.
func (db *DB) AddBudget(ctx context.Context, accountID string, day time.Time, n int64) (int64, error) {
	if n < 0 {
		return 0, fmt.Errorf("budget: %d bytes", n)
	}
	d := day.UTC().Format(time.DateOnly)
	var used int64
	err := db.pool.QueryRow(ctx, `
		UPDATE account_sources SET
			budget_used = CASE WHEN budget_day = $2::date THEN budget_used + $3 ELSE $3 END,
			budget_day = $2::date
		WHERE account_id = $1
		RETURNING budget_used`, accountID, d, n).Scan(&used)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("budget of %s: %w", accountID, err)
	}
	return used, nil
}

// ---------------------------------------------------------------------------
// The worker's lock

// pullLockKey is the first key of the session advisory lock a sync
// worker holds on its account (§10.1.3); the second is hashtext of the
// account id. Two accounts whose ids hash alike would share a lock and
// take turns, which delays one, never loses mail. Distinct from
// grantLockKey and the single-key locks (migrationLock, contentLock),
// which live in another key space.
const pullLockKey = 0x79706c // "ypl"

// PullLock is a held worker lock: it lives on its own connection, as a
// session lock must, and lasts until Release or until that connection
// dies.
type PullLock struct {
	conn    *pgxpool.Conn
	account string
}

// LockPullAccount takes the worker lock of account id without waiting.
// ok is false, with no lock, when another worker (another serve) holds
// it: that worker pulls this account, and this one skips the round.
func (db *DB) LockPullAccount(ctx context.Context, accountID string) (lock *PullLock, ok bool, err error) {
	conn, err := db.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("pull lock %s: %w", accountID, err)
	}
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, hashtext($2))`, int32(pullLockKey), accountID).Scan(&ok); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("pull lock %s: %w", accountID, err)
	}
	if !ok {
		conn.Release()
		return nil, false, nil
	}
	return &PullLock{conn: conn, account: accountID}, true, nil
}

// Check reports whether the lock is still held: only this session
// unlocks it, so it is held as long as the session is alive. A worker
// checks it before each batch; once it fails, another worker may hold
// the lock.
func (l *PullLock) Check(ctx context.Context) error {
	if _, err := l.conn.Exec(ctx, `SELECT 1`); err != nil {
		return fmt.Errorf("pull lock %s: %w", l.account, err)
	}
	return nil
}

// Release gives the lock up and returns its connection to the pool. If
// the unlock fails the connection is closed, which releases it as well.
func (l *PullLock) Release() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := l.conn.Exec(ctx, `SELECT pg_advisory_unlock($1, hashtext($2))`, int32(pullLockKey), l.account); err != nil {
		_ = l.conn.Hijack().Close(ctx)
		return
	}
	l.conn.Release()
}

// ---------------------------------------------------------------------------
// Folders and cursors

// Mailbox is a source_mailboxes row: how far one remote folder has been
// read. Every UID in [Low, High] is committed or failed; Low is 0 while
// backfill has not started (everything up to High is still its) and 1
// once it reached the bottom.
type Mailbox struct {
	Name              string
	SpecialUse        string
	KeyKind           KeyKind
	UIDValidity       uint32
	High              uint32
	Low               uint32
	UIDNextSeen       uint32 // 0: none yet
	MessagesSeen      uint32
	RescanUIDValidity uint32 // 0: no rescan pending
	DraftsSkipped     int64
	LastRoundAt       *time.Time
	CreatedAt         time.Time
}

const mailboxCols = `name, coalesce(special_use, ''), key_kind, uidvalidity, high_uid, coalesce(low_uid, 0),
	coalesce(uidnext_seen, 0), coalesce(messages_seen, 0), coalesce(rescan_uidvalidity, 0), drafts_skipped, last_round_at, created_at`

func scanMailbox(row pgx.Row) (Mailbox, error) {
	var m Mailbox
	var uv, high, low, next, msgs, rescan int64
	err := row.Scan(&m.Name, &m.SpecialUse, &m.KeyKind, &uv, &high, &low, &next, &msgs, &rescan, &m.DraftsSkipped, &m.LastRoundAt, &m.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrNotFound
	}
	m.UIDValidity, m.High, m.Low, m.UIDNextSeen, m.MessagesSeen, m.RescanUIDValidity =
		uint32(uv), uint32(high), uint32(low), uint32(next), uint32(msgs), uint32(rescan) //nolint:gosec // the columns' CHECKs bound them to 32 bits
	return m, err
}

// SourceMailboxes lists the folders of account id by name.
func (db *DB) SourceMailboxes(ctx context.Context, accountID string) ([]Mailbox, error) {
	rows, err := db.pool.Query(ctx, `SELECT `+mailboxCols+` FROM source_mailboxes WHERE account_id = $1 ORDER BY name`, accountID)
	if err != nil {
		return nil, fmt.Errorf("mailboxes of %s: %w", accountID, err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Mailbox, error) { return scanMailbox(r) })
	if err != nil {
		return nil, fmt.Errorf("mailboxes of %s: %w", accountID, err)
	}
	return out, nil
}

// MailboxStart is a folder seen for the first time.
type MailboxStart struct {
	Name        string
	SpecialUse  string
	KeyKind     KeyKind
	UIDValidity uint32
	// High is where catch-up starts: UIDNEXT-1 as the round saw it, so
	// the mail already there is the backfill's (newest first, §10.1.2).
	High uint32
}

// StartMailbox records folder m of account id, with its key kind fixed
// from now on (§10.1), and returns its row. A folder already recorded is
// returned as it is, whatever m says.
func (db *DB) StartMailbox(ctx context.Context, accountID string, m MailboxStart) (Mailbox, error) {
	switch {
	case m.KeyKind < KeyLocation || m.KeyKind > KeyGmail:
		return Mailbox{}, fmt.Errorf("mailbox %s: key kind %d", m.Name, m.KeyKind)
	case m.UIDValidity == 0:
		return Mailbox{}, fmt.Errorf("mailbox %s: UIDVALIDITY 0", m.Name)
	}
	if _, err := db.pool.Exec(ctx, `
		INSERT INTO source_mailboxes (account_id, name, special_use, key_kind, uidvalidity, high_uid)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (account_id, name) DO NOTHING`,
		accountID, m.Name, nullable(m.SpecialUse), int(m.KeyKind), int64(m.UIDValidity), int64(m.High)); err != nil {
		return Mailbox{}, fmt.Errorf("start mailbox %s: %w", m.Name, err)
	}
	out, err := scanMailbox(db.pool.QueryRow(ctx, `SELECT `+mailboxCols+` FROM source_mailboxes
		WHERE account_id = $1 AND name = $2`, accountID, m.Name))
	if err != nil {
		return out, fmt.Errorf("start mailbox %s: %w", m.Name, err)
	}
	return out, nil
}

// AdvanceHigh moves the catch-up cursor of folder mailbox up to to, and
// adds drafts, the \Draft mail it passed without storing (§10.1). It
// writes only if the folder is still at uidvalidity and to is higher
// than the cursor; moved says whether it did. The caller has committed
// (or recorded as failed) every UID up to to before calling it.
func (db *DB) AdvanceHigh(ctx context.Context, accountID, mailbox string, uidvalidity, to uint32, drafts int) (moved bool, err error) {
	tag, err := db.pool.Exec(ctx, `
		UPDATE source_mailboxes SET high_uid = $4, drafts_skipped = drafts_skipped + $5
		WHERE account_id = $1 AND name = $2 AND uidvalidity = $3 AND high_uid < $4`,
		accountID, mailbox, int64(uidvalidity), int64(to), drafts)
	if err != nil {
		return false, fmt.Errorf("advance %s high to %d: %w", mailbox, to, err)
	}
	return tag.RowsAffected() == 1, nil
}

// AdvanceLow moves the backfill cursor of folder mailbox down to to (1:
// the bottom) and adds drafts, as AdvanceHigh does upwards: only if the
// folder is still at uidvalidity and to is below where backfill got.
func (db *DB) AdvanceLow(ctx context.Context, accountID, mailbox string, uidvalidity, to uint32, drafts int) (moved bool, err error) {
	if to == 0 {
		return false, fmt.Errorf("advance %s low to 0", mailbox)
	}
	tag, err := db.pool.Exec(ctx, `
		UPDATE source_mailboxes SET low_uid = $4, drafts_skipped = drafts_skipped + $5
		WHERE account_id = $1 AND name = $2 AND uidvalidity = $3 AND coalesce(low_uid, high_uid + 1) > $4`,
		accountID, mailbox, int64(uidvalidity), int64(to), drafts)
	if err != nil {
		return false, fmt.Errorf("advance %s low to %d: %w", mailbox, to, err)
	}
	return tag.RowsAffected() == 1, nil
}

// RecordMailboxStatus records what STATUS said of folder mailbox at the
// end of a round that finished it, so the next round can skip it while
// nothing changed (§10.1.2). Only for the uidvalidity the round read.
func (db *DB) RecordMailboxStatus(ctx context.Context, accountID, mailbox string, uidvalidity, uidnext, messages uint32, at time.Time) (bool, error) {
	tag, err := db.pool.Exec(ctx, `
		UPDATE source_mailboxes SET uidnext_seen = $4, messages_seen = $5, last_round_at = $6
		WHERE account_id = $1 AND name = $2 AND uidvalidity = $3`,
		accountID, mailbox, int64(uidvalidity), nullableUID(uidnext), int64(messages), at)
	if err != nil {
		return false, fmt.Errorf("record status of %s: %w", mailbox, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ResetMailbox starts folder mailbox over under its new UIDVALIDITY
// (§10.1.3), if it is still at was: catch-up restarts at high, backfill
// has not started, and a location-keyed folder remembers was as the
// rescan to finish (FinishRescan). Its deliveries keep their locations
// until Relocate rewrites them; one rescan at a time, so a folder reset
// again before it finished leaves the older generation's deliveries
// where they were, still unique and committed, only with a stale
// location.
func (db *DB) ResetMailbox(ctx context.Context, accountID, mailbox string, was, uidvalidity, high uint32) (bool, error) {
	if uidvalidity == 0 || uidvalidity == was {
		return false, fmt.Errorf("reset %s: UIDVALIDITY %d after %d", mailbox, uidvalidity, was)
	}
	tag, err := db.pool.Exec(ctx, `
		UPDATE source_mailboxes SET uidvalidity = $4, high_uid = $5, low_uid = NULL, uidnext_seen = NULL, messages_seen = NULL,
			rescan_uidvalidity = CASE WHEN key_kind = 1 THEN $3 END
		WHERE account_id = $1 AND name = $2 AND uidvalidity = $3`,
		accountID, mailbox, int64(was), int64(uidvalidity), int64(high))
	if err != nil {
		return false, fmt.Errorf("reset %s: %w", mailbox, err)
	}
	return tag.RowsAffected() == 1, nil
}

// FinishRescan records that every delivery under the old UIDVALIDITY
// rescan of folder mailbox has been matched or given up on.
func (db *DB) FinishRescan(ctx context.Context, accountID, mailbox string, rescan uint32) (bool, error) {
	tag, err := db.pool.Exec(ctx, `UPDATE source_mailboxes SET rescan_uidvalidity = NULL
		WHERE account_id = $1 AND name = $2 AND rescan_uidvalidity = $3`, accountID, mailbox, int64(rescan))
	if err != nil {
		return false, fmt.Errorf("finish rescan of %s: %w", mailbox, err)
	}
	return tag.RowsAffected() == 1, nil
}

func nullableUID(n uint32) any {
	if n == 0 {
		return nil
	}
	return int64(n)
}

// ---------------------------------------------------------------------------
// What is already there

// PulledDelivery is a pulled delivery's identity and remote location.
type PulledDelivery struct {
	ID        string
	SourceKey string
	RemoteLocation
}

const pulledCols = `d.id, d.source_key, d.remote_mailbox, d.remote_uidvalidity, d.remote_uid`

func scanPulled(row pgx.CollectableRow) (PulledDelivery, error) {
	var p PulledDelivery
	var uv, uid int64
	err := row.Scan(&p.ID, &p.SourceKey, &p.Mailbox, &uv, &uid)
	p.UIDValidity, p.UID = uint32(uv), uint32(uid) //nolint:gosec // the columns hold IMAP's 32-bit values
	return p, err
}

// KnownSourceKeys returns the deliveries of account id that have one of
// keys, by key: the one query a round makes to skip what it already has
// before fetching any body (§10.1.3).
func (db *DB) KnownSourceKeys(ctx context.Context, accountID string, keys []string) (map[string]PulledDelivery, error) {
	rows, err := db.pool.Query(ctx, `SELECT `+pulledCols+` FROM deliveries d
		WHERE d.account_id = $1 AND d.source_key = ANY ($2) AND d.remote_mailbox IS NOT NULL`, accountID, keys)
	if err != nil {
		return nil, fmt.Errorf("known source keys of %s: %w", accountID, err)
	}
	found, err := pgx.CollectRows(rows, scanPulled)
	if err != nil {
		return nil, fmt.Errorf("known source keys of %s: %w", accountID, err)
	}
	out := make(map[string]PulledDelivery, len(found))
	for _, p := range found {
		out[p.SourceKey] = p
	}
	return out, nil
}

// PulledUIDs returns the UIDs of the deliveries of account id located in
// folder mailbox under uidvalidity, ascending: what the weekly
// reconciliation compares with the server's list (§10.1.3).
func (db *DB) PulledUIDs(ctx context.Context, accountID, mailbox string, uidvalidity uint32) ([]uint32, error) {
	rows, err := db.pool.Query(ctx, `SELECT remote_uid FROM deliveries
		WHERE account_id = $1 AND remote_mailbox = $2 AND remote_uidvalidity = $3
		ORDER BY remote_uid`, accountID, mailbox, int64(uidvalidity))
	if err != nil {
		return nil, fmt.Errorf("pulled UIDs of %s: %w", mailbox, err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (uint32, error) {
		var uid int64
		err := r.Scan(&uid)
		return uint32(uid), err //nolint:gosec // the column holds IMAP's 32-bit UIDs
	})
	if err != nil {
		return nil, fmt.Errorf("pulled UIDs of %s: %w", mailbox, err)
	}
	return out, nil
}

// FromOutbox reports whether message id is mail we sent: a sent copy
// that an outbox row points at folds into it (§4.6). A pulled delivery
// of our own mail, which Gmail keeps when its SMTP sent it, is never
// notified (§10.1.4).
func (db *DB) FromOutbox(ctx context.Context, id string) (bool, error) {
	var ok bool
	err := db.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM messages m JOIN raw_objects sr ON sr.content_hash = m.content_hash
		WHERE m.id = $1 AND `+isSentCopy+`)`, id).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("from outbox %s: %w", id, err)
	}
	return ok, nil
}

// RescanMatches returns the deliveries of account id still located in
// folder mailbox under the old uidvalidity whose raw has sha256 sum: the
// candidates a location-keyed rescan rewrites in place (§10.1.3).
func (db *DB) RescanMatches(ctx context.Context, accountID, mailbox string, uidvalidity uint32, sum [32]byte) ([]PulledDelivery, error) {
	rows, err := db.pool.Query(ctx, `SELECT `+pulledCols+`
		FROM deliveries d JOIN raw_objects r ON r.id = d.raw_id
		WHERE d.account_id = $1 AND d.remote_mailbox = $2 AND d.remote_uidvalidity = $3 AND r.sha256 = $4
		ORDER BY d.remote_uid, d.id`, accountID, mailbox, int64(uidvalidity), sum[:])
	if err != nil {
		return nil, fmt.Errorf("rescan matches in %s: %w", mailbox, err)
	}
	out, err := pgx.CollectRows(rows, scanPulled)
	if err != nil {
		return nil, fmt.Errorf("rescan matches in %s: %w", mailbox, err)
	}
	return out, nil
}

// Relocate rewrites the remote location of delivery from.ID of account
// id to to, and its source key to to.SourceKey, if they are still what
// from says (§10.1.3). This is the only path that changes a delivery's
// location or source key, both delivery facts. A stable key (gm:, oid:)
// stays as it is; a location key must be rewritten to the new
// location's. moved is false when the delivery has moved on since it was
// read; ErrDuplicate when another delivery already has the new key.
func (db *DB) Relocate(ctx context.Context, accountID string, from, to PulledDelivery) (moved bool, err error) {
	if to.ID != "" && to.ID != from.ID {
		return false, fmt.Errorf("relocate %s: to names delivery %s", from.ID, to.ID)
	}
	if strings.HasPrefix(from.SourceKey, "loc:") {
		if err := checkSourceKey(KeyLocation, from.SourceKey, from.RemoteLocation); err != nil {
			return false, err
		}
		if err := checkSourceKey(KeyLocation, to.SourceKey, to.RemoteLocation); err != nil {
			return false, err
		}
	} else if to.SourceKey != from.SourceKey {
		return false, fmt.Errorf("relocate %s: a stable source key never changes (%q to %q)", from.ID, from.SourceKey, to.SourceKey)
	}
	if to.UIDValidity == 0 || to.UID == 0 || to.Mailbox == "" {
		return false, fmt.Errorf("relocate %s: no location", from.ID)
	}
	tag, err := db.pool.Exec(ctx, `
		UPDATE deliveries SET source_key = $7, remote_mailbox = $8, remote_uidvalidity = $9, remote_uid = $10
		WHERE id = $2 AND account_id = $1 AND source_key = $3
			AND remote_mailbox = $4 AND remote_uidvalidity = $5 AND remote_uid = $6`,
		accountID, from.ID, from.SourceKey, from.Mailbox, int64(from.UIDValidity), int64(from.UID),
		to.SourceKey, to.Mailbox, int64(to.UIDValidity), int64(to.UID))
	if err != nil {
		return false, fmt.Errorf("relocate %s: %w", from.ID, uniqueAsDuplicate(err))
	}
	return tag.RowsAffected() == 1, nil
}

// ---------------------------------------------------------------------------
// Commit

// Omitted says why a pulled delivery has only its header
// (deliveries.remote_omitted, §4.4).
type Omitted int

const (
	// OmittedTooLarge: larger than the source's max_message_bytes.
	OmittedTooLarge Omitted = 1
	// OmittedUnfetchable: the body could not be fetched.
	OmittedUnfetchable Omitted = 2
)

// Pull is everything the commit of one pulled message writes: its raw
// object under pull/ (§2.2) and its delivery.
type Pull struct {
	RawID     string
	R2Key     string
	LocalPath string
	Size      int64
	SHA256    [32]byte
	// ContentHash is nil for a header-only raw (§4.4), which then has
	// Omitted set; a full raw has it and no Omitted.
	ContentHash *[32]byte
	Omitted     Omitted
	// InternalDate is the remote INTERNALDATE: the received_at of both
	// rows (§4).
	InternalDate time.Time

	DeliveryID string
	AccountID  string
	SourceKey  string
	RemoteLocation
	// Labels is Gmail's X-GM-LABELS at the time of the pull; nil when
	// the server has no labels.
	Labels []string
}

// CommitPull is the commit point of one pulled message (§10.1.3): one
// transaction that records its raw object and either a delivery or the
// fact that its source key was already delivered, which marks the raw
// duplicate_of that delivery's, as CommitIngest does for push. The
// delivery is to the source's address, with no envelope sender; its
// folder must be recorded (StartMailbox), still at p.UIDValidity
// (ErrStaleMailbox), and p.SourceKey must be of the folder's key kind
// (ErrSourceKey).
//
// The cursor moves after this, outside it (AdvanceHigh), so a crash in
// between leaves a committed delivery the next round skips by its key.
func (db *DB) CommitPull(ctx context.Context, p Pull) (Outcome, error) {
	completeness := 1
	var contentHash []byte
	switch {
	case p.ContentHash != nil && p.Omitted == 0:
		contentHash = p.ContentHash[:]
	case p.ContentHash == nil && (p.Omitted == OmittedTooLarge || p.Omitted == OmittedUnfetchable):
		completeness = 2
	default:
		return 0, fmt.Errorf("commit pull %s: a header-only raw has no content hash and says why (omitted %d)", p.RawID, p.Omitted)
	}
	var out Outcome
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		var address string
		var kind KeyKind
		var uidvalidity int64
		err := tx.QueryRow(ctx, `
			SELECT s.address, m.key_kind, m.uidvalidity
			FROM account_sources s JOIN source_mailboxes m ON m.account_id = s.account_id
			WHERE s.account_id = $1 AND m.name = $2`, p.AccountID, p.Mailbox).Scan(&address, &kind, &uidvalidity)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("mailbox %q of account %s: %w", p.Mailbox, p.AccountID, ErrNotFound)
		}
		if err != nil {
			return err
		}
		if uidvalidity != int64(p.UIDValidity) {
			return fmt.Errorf("mailbox %q is at UIDVALIDITY %d, not %d: %w", p.Mailbox, uidvalidity, p.UIDValidity, ErrStaleMailbox)
		}
		if p.UID == 0 {
			return fmt.Errorf("mailbox %q: UID 0", p.Mailbox)
		}
		if err := checkSourceKey(kind, p.SourceKey, p.RemoteLocation); err != nil {
			return err
		}

		tag, err := tx.Exec(ctx, `
			INSERT INTO raw_objects (id, r2_key, local_path, size, sha256, received_at, completeness, content_hash)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT DO NOTHING`,
			p.RawID, p.R2Key, p.LocalPath, p.Size, p.SHA256[:], p.InternalDate, completeness, contentHash)
		if err != nil {
			return fmt.Errorf("insert raw_objects: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return alreadyIngested(ctx, tx, p.RawID, p.R2Key, p.SHA256, &out)
		}

		zone, prefix := splitAddress(address)
		var omitted any
		if p.Omitted != 0 {
			omitted = int(p.Omitted)
		}
		tag, err = tx.Exec(ctx, `
			INSERT INTO deliveries (id, account_id, source_key, raw_id, zone, prefix, envelope_from, envelope_to, received_at,
				remote_mailbox, remote_uidvalidity, remote_uid, remote_state, remote_labels, remote_omitted)
			VALUES ($1, $2, $3, $4, $5, $6, NULL, $7, $8, $9, $10, $11, 1, $12, $13)
			ON CONFLICT (account_id, source_key) DO NOTHING`,
			p.DeliveryID, p.AccountID, p.SourceKey, p.RawID, zone, prefix, address, p.InternalDate,
			p.Mailbox, int64(p.UIDValidity), int64(p.UID), p.Labels, omitted)
		if err != nil {
			return fmt.Errorf("insert deliveries: %w", err)
		}
		if tag.RowsAffected() == 1 {
			out = Delivered
			return nil
		}
		// As in CommitIngest: READ COMMITTED lets this statement see the
		// delivery the conflicting transaction committed.
		tag, err = tx.Exec(ctx, `
			UPDATE raw_objects r SET duplicate_of = d.raw_id
			FROM deliveries d
			WHERE r.id = $1 AND d.account_id = $2 AND d.source_key = $3`,
			p.RawID, p.AccountID, p.SourceKey)
		if err != nil {
			return fmt.Errorf("mark duplicate: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("mark duplicate: delivery %q vanished", p.SourceKey)
		}
		out = Redelivery
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("commit pull %s: %w", p.RawID, err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Failures

// PullFailure is a pull_failures row: a message that could not be
// fetched (§10.1.3).
type PullFailure struct {
	AccountID string
	RemoteLocation
	// Size is RFC822.SIZE as the server said it; -1 when it did not.
	Size int64
	// Detail is what went wrong, partly in the server's words: untrusted,
	// to be shown as data only.
	Detail     string
	Attempts   int
	FirstAt    time.Time
	LastAt     time.Time
	ResolvedAt *time.Time
}

// maxFailureDetail bounds pull_failures.detail.
const maxFailureDetail = 1024

// cleanDetail makes an arbitrary error text storable: valid UTF-8, no
// NUL (which Postgres text cannot hold), at most maxFailureDetail bytes,
// cut at a rune boundary. A shortened detail is still a true one.
func cleanDetail(s string) string {
	s = strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\x00", "�")
	if len(s) <= maxFailureDetail {
		return s
	}
	cut := maxFailureDetail
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// RecordPullFailure records one more failed attempt at the message at
// f's location at f.LastAt, and returns how many there have been. A
// resolved failure that fails again is open again. The worker passes
// the message (moves the cursor over it) only once this has recorded
// enough attempts, so nothing is passed unrecorded (§10.1.3).
func (db *DB) RecordPullFailure(ctx context.Context, f PullFailure) (attempts int, err error) {
	if f.UIDValidity == 0 || f.UID == 0 {
		return 0, fmt.Errorf("pull failure in %s: no location", f.Mailbox)
	}
	var size any
	if f.Size >= 0 {
		size = f.Size
	}
	err = db.pool.QueryRow(ctx, `
		INSERT INTO pull_failures (account_id, mailbox, uidvalidity, uid, size, detail, attempts, first_at, last_at)
		VALUES ($1, $2, $3, $4, $5, $6, 1, $7, $7)
		ON CONFLICT (account_id, mailbox, uidvalidity, uid) DO UPDATE SET
			size = coalesce(excluded.size, pull_failures.size), detail = excluded.detail,
			attempts = pull_failures.attempts + 1, last_at = greatest(pull_failures.last_at, excluded.last_at),
			resolved_at = NULL
		RETURNING attempts`,
		f.AccountID, f.Mailbox, int64(f.UIDValidity), int64(f.UID), size, cleanDetail(f.Detail), f.LastAt).Scan(&attempts)
	if err != nil {
		return 0, fmt.Errorf("record pull failure %s/%d: %w", f.Mailbox, f.UID, err)
	}
	return attempts, nil
}

// ResolvePullFailure marks the failure at loc of account id resolved:
// the message was committed after all. A location with no open failure
// is left alone.
func (db *DB) ResolvePullFailure(ctx context.Context, accountID string, loc RemoteLocation, at time.Time) error {
	_, err := db.pool.Exec(ctx, `UPDATE pull_failures SET resolved_at = $5
		WHERE account_id = $1 AND mailbox = $2 AND uidvalidity = $3 AND uid = $4 AND resolved_at IS NULL`,
		accountID, loc.Mailbox, int64(loc.UIDValidity), int64(loc.UID), at)
	if err != nil {
		return fmt.Errorf("resolve pull failure %s/%d: %w", loc.Mailbox, loc.UID, err)
	}
	return nil
}

// PullFailures lists the failures of account id by location, only the
// open ones unless all: what `account ls` counts and what
// `account sync --retry-failed` fetches again.
func (db *DB) PullFailures(ctx context.Context, accountID string, all bool) ([]PullFailure, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT account_id, mailbox, uidvalidity, uid, coalesce(size, -1), detail, attempts, first_at, last_at, resolved_at
		FROM pull_failures WHERE account_id = $1 AND ($2 OR resolved_at IS NULL)
		ORDER BY mailbox, uidvalidity, uid`, accountID, all)
	if err != nil {
		return nil, fmt.Errorf("pull failures of %s: %w", accountID, err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (PullFailure, error) {
		var f PullFailure
		var uv, uid int64
		err := r.Scan(&f.AccountID, &f.Mailbox, &uv, &uid, &f.Size, &f.Detail, &f.Attempts, &f.FirstAt, &f.LastAt, &f.ResolvedAt)
		f.UIDValidity, f.UID = uint32(uv), uint32(uid) //nolint:gosec // the columns' CHECKs bound them to 32 bits
		return f, err
	})
	if err != nil {
		return nil, fmt.Errorf("pull failures of %s: %w", accountID, err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Trouble

// SourceTrouble is what the account.trouble pushes that survive a restart
// are decided from (§10.1.3), for one enabled source.
type SourceTrouble struct {
	AccountID   string
	AccountName string
	// Refused is the paused_at of an auth_failed pause not told of yet.
	Refused *time.Time
	// Failures is how many open failures the cursor has passed;
	// FailuresTold how many it had when last told of, at FailuresToldAt.
	Failures       int
	FailuresTold   int
	FailuresToldAt *time.Time
}

// SourceTroubles reads every enabled source's SourceTrouble. A failure
// counts once it has maxAttempts attempts: before that it is still being
// retried and the cursor waits for it.
func (db *DB) SourceTroubles(ctx context.Context, maxAttempts int) ([]SourceTrouble, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT s.account_id, a.name,
			CASE WHEN s.pause_reason = 'auth_failed' AND s.refused_told_at IS DISTINCT FROM s.paused_at THEN s.paused_at END,
			(SELECT count(*) FROM pull_failures f WHERE f.account_id = s.account_id AND f.resolved_at IS NULL AND f.attempts >= $1),
			s.failures_told, s.failures_told_at
		FROM `+sourceFrom+`
		WHERE s.disabled_at IS NULL
		ORDER BY a.name`, maxAttempts)
	if err != nil {
		return nil, fmt.Errorf("source troubles: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (SourceTrouble, error) {
		var t SourceTrouble
		err := r.Scan(&t.AccountID, &t.AccountName, &t.Refused, &t.Failures, &t.FailuresTold, &t.FailuresToldAt)
		return t, err
	})
	if err != nil {
		return nil, fmt.Errorf("source troubles: %w", err)
	}
	return out, nil
}

// RecordRefusalTold records that the pause of account id at pausedAt has
// been told of.
func (db *DB) RecordRefusalTold(ctx context.Context, accountID string, pausedAt time.Time) error {
	if _, err := db.pool.Exec(ctx, `UPDATE account_sources SET refused_told_at = $2 WHERE account_id = $1`, accountID, pausedAt); err != nil {
		return fmt.Errorf("record refusal told of %s: %w", accountID, err)
	}
	return nil
}

// RecordFailuresTold records that n failures of account id have been told
// of, at told; a nil told only lowers the count and keeps the time, which
// is what limits the pushes to one a day.
func (db *DB) RecordFailuresTold(ctx context.Context, accountID string, n int, told *time.Time) error {
	if _, err := db.pool.Exec(ctx, `UPDATE account_sources SET failures_told = $2, failures_told_at = coalesce($3, failures_told_at)
		WHERE account_id = $1`, accountID, n, told); err != nil {
		return fmt.Errorf("record failures told of %s: %w", accountID, err)
	}
	return nil
}

func nullableInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}
