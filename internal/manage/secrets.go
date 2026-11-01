package manage

import (
	"context"
	"errors"
	"fmt"

	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// Owner is the row that uses a secret: what the secret is for, and what
// its audience is recomputed from. A secret is never opened for the
// audience stored beside it, which is exactly what a database writer
// controls (§17.2).
type Owner struct {
	// What names it for people: "transport cloudflare", "account gmail".
	What string
	// Kind is the secrets.kind it must have.
	Kind string
	// Transport, Channel or Account is the owning row's name; one of them
	// is set.
	Transport, Channel, Account string
	// Audience is recomputed from the owning row.
	Audience func() (string, error)
}

// OwnerStore lists the rows that own secrets.
type OwnerStore interface {
	Transports(ctx context.Context, all bool) ([]store.Transport, error)
	Channels(ctx context.Context, all bool) ([]store.Channel, error)
	Sources(ctx context.Context, all bool) ([]store.Source, error)
}

// Owners maps secret ids to their owners. Every table that references
// secrets contributes its rows here, and only here: a new owner kind
// (M3.5b's OAuth clients) is one more block below, and listing,
// checking and rewrapping follow.
func Owners(ctx context.Context, st OwnerStore) (map[string]Owner, error) {
	out := map[string]Owner{}
	trs, err := st.Transports(ctx, true)
	if err != nil {
		return nil, err
	}
	for _, t := range trs {
		if t.SecretID == "" {
			continue
		}
		out[t.SecretID] = Owner{
			What: "transport " + t.Name, Kind: store.SecretSMTPPassword, Transport: t.Name,
			Audience: func() (string, error) { return secret.SMTPS(t.Username, t.Host, t.Port) },
		}
	}
	chs, err := st.Channels(ctx, true)
	if err != nil {
		return nil, err
	}
	for _, c := range chs {
		if c.SecretID == "" {
			continue
		}
		out[c.SecretID] = Owner{
			What: "channel " + c.Name, Kind: store.SecretTelegramBotToken, Channel: c.Name,
			Audience: c.Audience, // telegram:bot<id>, from the row
		}
	}
	srcs, err := st.Sources(ctx, true)
	if err != nil {
		return nil, err
	}
	for _, s := range srcs {
		if s.SecretID == "" {
			continue
		}
		out[s.SecretID] = Owner{
			What: "account " + s.AccountName, Kind: store.SecretIMAPPassword, Account: s.AccountName,
			Audience: s.Audience, // imaps://<username>@<host>:<port>, from the row
		}
	}
	return out, nil
}

// ErrWrongKind: a secret is of another kind than its owner needs, as if
// one row's secret had been pointed at another's.
var ErrWrongKind = errors.New("wrong kind")

// Open opens r for the audience its owner o gives, as using it would,
// and wipes the plaintext at once: nil if it opens, else why not
// (ErrWrongKind, secret.ErrLocked, secret.ErrAudienceMismatch,
// secret.ErrTampered, ...). A nil sealer is locked.
func Open(ctx context.Context, s *secret.Sealer, r secret.Record, o Owner) error {
	if r.Kind != o.Kind {
		return fmt.Errorf("%w: %s, but its owner needs %s", ErrWrongKind, r.Kind, o.Kind)
	}
	aud, err := o.Audience()
	if err != nil {
		return fmt.Errorf("%w: its owner names no valid destination (%w)", secret.ErrAudienceMismatch, err)
	}
	v, err := s.Open(ctx, r, aud)
	if err != nil {
		return err
	}
	v.Close()
	return nil
}

// CheckOpen is Open said for a person: "ok", or why not (OpenFailure).
func CheckOpen(ctx context.Context, s *secret.Sealer, r secret.Record, o Owner) string {
	switch err := Open(ctx, s, r, o); {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrWrongKind):
		return err.Error()
	default:
		return OpenFailure(err)
	}
}
