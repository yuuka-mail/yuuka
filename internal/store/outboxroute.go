package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/secret"
)

// What the executor reads to decide which transports it can send through
// (§10.2.2, §17.5). It resolves every transport at each claim, from these
// rows and their revisions, and claims only through what it resolved at
// those revisions: nothing is reloaded, nothing is notified, a changed
// transport or secret simply stops matching.

// SendingTransport is a transport with its secret, if it has one, as the
// executor resolves it.
type SendingTransport struct {
	Transport
	// Secret is nil when the transport has none (a placeholder, auth
	// none, disabled).
	Secret *StoredSecret
}

// SecretRevision is the revision of t's secret, 0 without one: with the
// transport's own revision, what a client built from t is cached by.
func (t SendingTransport) SecretRevision() int64 {
	if t.Secret == nil {
		return 0
	}
	return t.Secret.Revision
}

// SendingTransports lists every transport, disabled ones too, with its
// secret.
func (db *DB) SendingTransports(ctx context.Context) ([]SendingTransport, error) {
	rows, err := db.pool.Query(ctx, `SELECT `+transportCols+`,
		s.id, s.kind, s.audience, s.wrapper, s.key_ref, s.wrapped_dek, s.payload, s.revision, s.created_at, s.sealed_at
		FROM transports t LEFT JOIN secrets s ON s.id = t.secret_id ORDER BY t.name`)
	if err != nil {
		return nil, fmt.Errorf("sending transports: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (SendingTransport, error) {
		var t SendingTransport
		var id, kind, aud, wrapper, keyRef *string
		var dek, payload []byte
		var rev *int64
		var created, sealed *time.Time
		err := r.Scan(&t.ID, &t.Name, &t.Kind, &t.Host, &t.Port, &t.Auth, &t.Username,
			&t.SecretID, &t.Caps, &t.ConfiguredAt, &t.DisabledAt, &t.Revision, &t.CreatedAt,
			&id, &kind, &aud, &wrapper, &keyRef, &dek, &payload, &rev, &created, &sealed)
		if err != nil || id == nil {
			return t, err
		}
		t.Secret = &StoredSecret{
			Record: secret.Record{
				ID: *id, Kind: *kind, Audience: *aud, Wrapper: *wrapper, KeyRef: *keyRef, WrappedDEK: dek, Payload: payload,
			},
			Revision: *rev, CreatedAt: *created, SealedAt: *sealed,
		}
		return t, nil
	})
	if err != nil {
		return nil, fmt.Errorf("sending transports: %w", err)
	}
	return out, nil
}

// UsableTransport is a transport the executor can send through now, at
// the revisions it resolved: a claim through it matches both, so a row is
// never claimed through a transport or secret that changed since.
type UsableTransport struct {
	ID             string
	Revision       int64
	SecretRevision int64 // 0 for none
}

// ChainMember is a member of an identity's transport chain.
type ChainMember struct {
	ID   string
	Name string
}

// usableTransports is the id and name of each transport in the arrays $1
// (ids), $2 (revisions) and $3 (secret revisions) whose revisions still
// match.
const usableTransports = `
	SELECT t.id, t.name FROM transports t
	LEFT JOIN secrets s ON s.id = t.secret_id
	JOIN unnest($1::text[], $2::bigint[], $3::bigint[]) AS u (id, rev, srev)
	  ON u.id = t.id AND u.rev = t.revision AND u.srev = coalesce(s.revision, 0)`

func usableArrays(us []UsableTransport) (ids []string, revs, srevs []int64) {
	for _, u := range us {
		ids, revs, srevs = append(ids, u.ID), append(revs, u.Revision), append(srevs, u.SecretRevision)
	}
	return ids, revs, srevs
}

// RouteAttempt records that the open attempt attemptID goes through the
// transport called name rather than the one it was claimed for: the
// executor found, once it had read the stored bytes, that an earlier
// member of the chain does not allow them (§10.2.2). Only before the
// commit point, by its owner and within its lease, like CommitAttempt;
// otherwise ErrLeaseLost. It writes no change: the outbox row does not
// change state.
func (db *DB) RouteAttempt(ctx context.Context, attemptID, owner, name string) error {
	tag, err := db.pool.Exec(ctx, `
		UPDATE send_attempts SET transport = $3
		WHERE id = $1 AND owner = $2 AND finished_at IS NULL AND committed_at IS NULL AND lease_until > now()`,
		attemptID, owner, name)
	if err != nil {
		return fmt.Errorf("route attempt %s: %w", attemptID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("route attempt %s: %w", attemptID, ErrLeaseLost)
	}
	return nil
}
