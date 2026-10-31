package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// SystemPrincipalID is the id 0002 seeds for the system principal.
const SystemPrincipalID = "00000000000000000000000000"

var (
	// ErrNotFound: no row matched.
	ErrNotFound = errors.New("not found")
	// ErrSpent: a one-time secret was already used, expired or does not
	// exist; the caller learns nothing more specific.
	ErrSpent = errors.New("spent or expired")
	// ErrDuplicate: a unique key collided (e.g. a passkey registered twice).
	ErrDuplicate = errors.New("duplicate")
)

// Principal is a row of principals (§13.2).
type Principal struct {
	ID          string
	Kind        string
	Name        string
	DisplayName string
	OwnerID     string // "" when none
	CreatedAt   time.Time
	DisabledAt  *time.Time
}

// Credential is a row of credentials (§13.3) without the oauth ciphertext.
type Credential struct {
	ID          string
	PrincipalID string
	Kind        string
	Label       string
	SecretHash  []byte
	Scopes      []string
	CreatedAt   time.Time
	ExpiresAt   *time.Time
	LastUsedAt  *time.Time
	RevokedAt   *time.Time
}

// Passkey is a row of passkeys.
type Passkey struct {
	ID                string
	PrincipalID       string
	CredentialID      []byte
	PublicKey         []byte
	SignCount         uint32
	AAGUID            []byte
	Transports        []string
	UserVerified      bool
	BackupEligible    bool
	BackupState       bool
	AttestationType   string
	AttestationFormat string
	Label             string
	CreatedAt         time.Time
	LastUsedAt        *time.Time
}

// EnrollmentToken is a row of enrollment_tokens.
type EnrollmentToken struct {
	ID          string
	PrincipalID string
	SecretHash  []byte
	Reason      string // enroll | recovery
	ExpiresAt   time.Time
	UsedAt      *time.Time
}

// RecoveryCode is a new row of recovery_codes.
type RecoveryCode struct {
	ID   string
	Hash [32]byte
}

// Action is a row of actions (§8.8). Args and Result are marshaled to JSON
// and must never contain a secret.
type Action struct {
	ID           string
	Actor        string // "" = unauthenticated caller
	CredentialID string // "" = none
	Tool         string
	Args         any
	Result       any
	At           time.Time
}

// querier is what both a pool and a transaction offer.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func insertAction(ctx context.Context, q querier, a Action) error {
	args, err := json.Marshal(a.Args)
	if err != nil {
		return fmt.Errorf("action %s: args: %w", a.Tool, err)
	}
	if a.Args == nil {
		args = []byte("{}")
	}
	var result []byte
	if a.Result != nil {
		if result, err = json.Marshal(a.Result); err != nil {
			return fmt.Errorf("action %s: result: %w", a.Tool, err)
		}
	}
	_, err = q.Exec(ctx, `
		INSERT INTO actions (id, actor, credential_id, tool, args, result, at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		a.ID, nullable(a.Actor), nullable(a.CredentialID), a.Tool, args, result, a.At)
	if err != nil {
		return fmt.Errorf("insert action %s: %w", a.Tool, err)
	}
	return nil
}

// RecordAction appends one entry to the audit log.
func (db *DB) RecordAction(ctx context.Context, a Action) error {
	return insertAction(ctx, db.pool, a)
}

const principalCols = `id, kind, name, display_name, coalesce(owner_id, ''), created_at, disabled_at`

func scanPrincipal(row pgx.Row) (Principal, error) {
	var p Principal
	err := row.Scan(&p.ID, &p.Kind, &p.Name, &p.DisplayName, &p.OwnerID, &p.CreatedAt, &p.DisabledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// Principal returns the principal with id.
func (db *DB) Principal(ctx context.Context, id string) (Principal, error) {
	p, err := scanPrincipal(db.pool.QueryRow(ctx, `SELECT `+principalCols+` FROM principals WHERE id = $1`, id))
	if err != nil {
		return p, fmt.Errorf("principal %s: %w", id, err)
	}
	return p, nil
}

// PrincipalsOfKind lists the enabled principals of one kind by name.
func (db *DB) PrincipalsOfKind(ctx context.Context, kind string) ([]Principal, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT `+principalCols+` FROM principals
		WHERE kind = $1 AND disabled_at IS NULL ORDER BY name`, kind)
	if err != nil {
		return nil, fmt.Errorf("list %s principals: %w", kind, err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Principal, error) { return scanPrincipal(r) })
	if err != nil {
		return nil, fmt.Errorf("list %s principals: %w", kind, err)
	}
	return out, nil
}

// EnsurePrincipal returns the principal (kind, name), creating it from p
// if it does not exist.
func (db *DB) EnsurePrincipal(ctx context.Context, p Principal) (Principal, error) {
	_, err := db.pool.Exec(ctx, `
		INSERT INTO principals (id, kind, name, display_name, owner_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (kind, name) DO NOTHING`,
		p.ID, p.Kind, p.Name, p.DisplayName, nullable(p.OwnerID), p.CreatedAt)
	if err != nil {
		return Principal{}, fmt.Errorf("create principal %s:%s: %w", p.Kind, p.Name, err)
	}
	got, err := scanPrincipal(db.pool.QueryRow(ctx,
		`SELECT `+principalCols+` FROM principals WHERE kind = $1 AND name = $2`, p.Kind, p.Name))
	if err != nil {
		return got, fmt.Errorf("principal %s:%s: %w", p.Kind, p.Name, err)
	}
	return got, nil
}

// CredentialPrincipal is a credential with the principal fields that
// authentication needs.
type CredentialPrincipal struct {
	Credential
	PrincipalKind     string
	PrincipalName     string
	PrincipalDisabled bool
}

// CredentialForAuth loads a credential and its principal by credential id.
func (db *DB) CredentialForAuth(ctx context.Context, id string) (CredentialPrincipal, error) {
	var c CredentialPrincipal
	err := db.pool.QueryRow(ctx, `
		SELECT c.id, c.principal_id, c.kind, c.label, c.secret_hash, c.scopes,
		       c.created_at, c.expires_at, c.last_used_at, c.revoked_at,
		       p.kind, p.name, p.disabled_at IS NOT NULL
		FROM credentials c JOIN principals p ON p.id = c.principal_id
		WHERE c.id = $1`, id).Scan(
		&c.ID, &c.PrincipalID, &c.Kind, &c.Label, &c.SecretHash, &c.Scopes,
		&c.CreatedAt, &c.ExpiresAt, &c.LastUsedAt, &c.RevokedAt,
		&c.PrincipalKind, &c.PrincipalName, &c.PrincipalDisabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, fmt.Errorf("credential %s: %w", id, err)
	}
	return c, nil
}

// TouchCredential records a use. It writes at most once per minute per
// credential so that a busy client does not turn every request into a
// row update.
func (db *DB) TouchCredential(ctx context.Context, id string, at time.Time) error {
	_, err := db.pool.Exec(ctx, `
		UPDATE credentials SET last_used_at = $2
		WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < $2::timestamptz - interval '1 minute')`, id, at)
	if err != nil {
		return fmt.Errorf("touch credential %s: %w", id, err)
	}
	return nil
}

func insertCredential(ctx context.Context, q querier, c Credential) error {
	_, err := q.Exec(ctx, `
		INSERT INTO credentials (id, principal_id, kind, label, secret_hash, scopes, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		c.ID, c.PrincipalID, c.Kind, c.Label, c.SecretHash, c.Scopes, c.CreatedAt, c.ExpiresAt)
	if err != nil {
		return fmt.Errorf("insert credential: %w", err)
	}
	return nil
}

// CreateCredential stores a verifiable credential and records a.
func (db *DB) CreateCredential(ctx context.Context, c Credential, a Action) error {
	return pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if err := insertCredential(ctx, tx, c); err != nil {
			return err
		}
		return insertAction(ctx, tx, a)
	})
}

// RevokeCredential revokes credential id and records a. It returns
// ErrNotFound if the credential does not exist or is already revoked.
func (db *DB) RevokeCredential(ctx context.Context, id string, a Action) error {
	return pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE credentials SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`, id, a.At)
		if err != nil {
			return fmt.Errorf("revoke credential %s: %w", id, err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return insertAction(ctx, tx, a)
	})
}

// Enrollment is what `yuuka admin enroll` writes in one transaction.
type Enrollment struct {
	Token EnrollmentToken
	// Codes replace every recovery code of the token's principal.
	Codes  []RecoveryCode
	Action Action
}

// IssueEnrollment stores a new enrollment token, rotates the recovery
// codes and records the action.
func (db *DB) IssueEnrollment(ctx context.Context, e Enrollment) error {
	return pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if err := insertEnrollmentToken(ctx, tx, e.Token, e.Action.At); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM recovery_codes WHERE principal_id = $1`, e.Token.PrincipalID); err != nil {
			return fmt.Errorf("delete recovery codes: %w", err)
		}
		for _, c := range e.Codes {
			_, err := tx.Exec(ctx, `
				INSERT INTO recovery_codes (id, principal_id, code_hash, created_at) VALUES ($1, $2, $3, $4)`,
				c.ID, e.Token.PrincipalID, c.Hash[:], e.Action.At)
			if err != nil {
				return fmt.Errorf("insert recovery code: %w", err)
			}
		}
		return insertAction(ctx, tx, e.Action)
	})
}

func insertEnrollmentToken(ctx context.Context, q querier, t EnrollmentToken, at time.Time) error {
	_, err := q.Exec(ctx, `
		INSERT INTO enrollment_tokens (id, principal_id, secret_hash, reason, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		t.ID, t.PrincipalID, t.SecretHash, t.Reason, at, t.ExpiresAt)
	if err != nil {
		return fmt.Errorf("insert enrollment token: %w", err)
	}
	return nil
}

// EnrollmentTokenByID loads an enrollment token, used or not.
func (db *DB) EnrollmentTokenByID(ctx context.Context, id string) (EnrollmentToken, error) {
	var t EnrollmentToken
	err := db.pool.QueryRow(ctx, `
		SELECT id, principal_id, secret_hash, reason, expires_at, used_at
		FROM enrollment_tokens WHERE id = $1`, id).Scan(
		&t.ID, &t.PrincipalID, &t.SecretHash, &t.Reason, &t.ExpiresAt, &t.UsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, fmt.Errorf("enrollment token %s: %w", id, err)
	}
	return t, nil
}

// RecoveryUse is what spending a recovery code writes.
type RecoveryUse struct {
	CodeHash [32]byte
	// Token is the enrollment token the code is exchanged for; its
	// PrincipalID is filled in from the code.
	Token EnrollmentToken
	// Action is recorded with Actor set to the code's principal and
	// Args extended with the code id.
	Action Action
}

// UseRecoveryCode burns the unused code with the given hash and, in the
// same transaction, issues a recovery enrollment token for its principal.
// It returns the principal id, or ErrSpent if no unused code matches.
func (db *DB) UseRecoveryCode(ctx context.Context, u RecoveryUse) (string, error) {
	var principal string
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		var codeID string
		err := tx.QueryRow(ctx, `
			UPDATE recovery_codes rc SET used_at = $2
			FROM principals p
			WHERE rc.code_hash = $1 AND rc.used_at IS NULL
			  AND p.id = rc.principal_id AND p.disabled_at IS NULL
			RETURNING rc.id, rc.principal_id`, u.CodeHash[:], u.Action.At).Scan(&codeID, &principal)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSpent
		}
		if err != nil {
			return fmt.Errorf("burn recovery code: %w", err)
		}
		u.Token.PrincipalID = principal
		if err := insertEnrollmentToken(ctx, tx, u.Token, u.Action.At); err != nil {
			return err
		}
		u.Action.Actor = principal
		u.Action.Args = map[string]any{"recovery_code_id": codeID, "enrollment_token_id": u.Token.ID, "request": u.Action.Args}
		return insertAction(ctx, tx, u.Action)
	})
	if err != nil {
		return "", err
	}
	return principal, nil
}

// UnusedRecoveryCodes counts the codes a principal has left.
func (db *DB) UnusedRecoveryCodes(ctx context.Context, principalID string) (int, error) {
	var n int
	err := db.pool.QueryRow(ctx, `
		SELECT count(*) FROM recovery_codes WHERE principal_id = $1 AND used_at IS NULL`, principalID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count recovery codes: %w", err)
	}
	return n, nil
}

const passkeyCols = `id, principal_id, credential_id, public_key, sign_count, coalesce(aaguid, ''::bytea), transports,
	user_verified, backup_eligible, backup_state, attestation_type, attestation_format, label, created_at, last_used_at`

func scanPasskey(row pgx.Row) (Passkey, error) {
	var p Passkey
	var count int64
	err := row.Scan(&p.ID, &p.PrincipalID, &p.CredentialID, &p.PublicKey, &count, &p.AAGUID, &p.Transports,
		&p.UserVerified, &p.BackupEligible, &p.BackupState, &p.AttestationType, &p.AttestationFormat,
		&p.Label, &p.CreatedAt, &p.LastUsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	p.SignCount = uint32(count) // the column CHECK keeps it in range
	return p, err
}

// Passkeys lists the unrevoked passkeys of a principal.
func (db *DB) Passkeys(ctx context.Context, principalID string) ([]Passkey, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT `+passkeyCols+` FROM passkeys
		WHERE principal_id = $1 AND revoked_at IS NULL ORDER BY id`, principalID)
	if err != nil {
		return nil, fmt.Errorf("list passkeys: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Passkey, error) { return scanPasskey(r) })
	if err != nil {
		return nil, fmt.Errorf("list passkeys: %w", err)
	}
	return out, nil
}

// NewPasskey is what a finished registration writes.
type NewPasskey struct {
	Passkey Passkey
	// EnrollmentTokenID, if set, is consumed in the same transaction; the
	// registration fails with ErrSpent if it was used or has expired.
	EnrollmentTokenID string
	Action            Action
}

// AddPasskey stores a registered passkey.
func (db *DB) AddPasskey(ctx context.Context, n NewPasskey) error {
	p := n.Passkey
	return pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO passkeys (id, principal_id, credential_id, public_key, sign_count, aaguid, transports,
			                      user_verified, backup_eligible, backup_state, attestation_type, attestation_format,
			                      label, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			p.ID, p.PrincipalID, p.CredentialID, p.PublicKey, int64(p.SignCount), p.AAGUID, p.Transports,
			p.UserVerified, p.BackupEligible, p.BackupState, p.AttestationType, p.AttestationFormat,
			p.Label, n.Action.At)
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
			return ErrDuplicate
		}
		if err != nil {
			return fmt.Errorf("insert passkey: %w", err)
		}
		if n.EnrollmentTokenID != "" {
			tag, err := tx.Exec(ctx, `
				UPDATE enrollment_tokens SET used_at = $2, passkey_id = $3
				WHERE id = $1 AND used_at IS NULL AND expires_at > $2`,
				n.EnrollmentTokenID, n.Action.At, p.ID)
			if err != nil {
				return fmt.Errorf("consume enrollment token: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return ErrSpent
			}
		}
		return insertAction(ctx, tx, n.Action)
	})
}

// PasskeyByCredentialID finds an unrevoked passkey by its WebAuthn
// credential id.
func (db *DB) PasskeyByCredentialID(ctx context.Context, credentialID []byte) (Passkey, error) {
	p, err := scanPasskey(db.pool.QueryRow(ctx, `
		SELECT `+passkeyCols+` FROM passkeys WHERE credential_id = $1 AND revoked_at IS NULL`, credentialID))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return p, fmt.Errorf("passkey: %w", err)
	}
	return p, err
}

// Login is what a successful passkey login writes.
type Login struct {
	PasskeyID    string
	SignCount    uint32
	BackupState  bool
	UserVerified bool
	Session      Credential
	Action       Action
}

// CompleteLogin updates the passkey's counters, stores the new session
// credential and records the login, all or nothing.
func (db *DB) CompleteLogin(ctx context.Context, l Login) error {
	return pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE passkeys SET sign_count = $2, backup_state = $3, user_verified = $4, last_used_at = $5
			WHERE id = $1 AND revoked_at IS NULL`,
			l.PasskeyID, int64(l.SignCount), l.BackupState, l.UserVerified, l.Action.At)
		if err != nil {
			return fmt.Errorf("update passkey: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if err := insertCredential(ctx, tx, l.Session); err != nil {
			return err
		}
		return insertAction(ctx, tx, l.Action)
	})
}
