package r2

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

// PullKey is the raw bucket key of a pulled message: pull/YYYY/MM/<id>.eml,
// with year and month taken from at, the time of the write, in UTC, and id
// a ULID. Like sent/ it is not part of the Worker contract: the Worker
// never reads or writes pull/, and Go is its only writer (§2.2). The month
// is never the remote INTERNALDATE, which is whatever the remote server
// says and so cannot be trusted to place a key.
func PullKey(at time.Time, id string) string {
	t := at.UTC()
	return fmt.Sprintf("pull/%04d/%02d/%s.eml", t.Year(), int(t.Month()), id)
}

// CheckPullKey accepts exactly the keys PullKey produces for a valid ULID.
// With CheckSentKey it is the only thing between Go's writable raw bucket
// token and raw/, so it checks the whole shape rather than the prefix.
func CheckPullKey(key string) error {
	if !keyShape(key, "pull") {
		return fmt.Errorf("r2: %q is not a pull key", key)
	}
	return nil
}

// PullMetadataKey is the custom metadata key on pull/ objects. Like
// yuuka-marker on raw/ (§2.2) it makes the object describe itself: once
// the remote copy is offloaded (§9.6), the R2 object may be the only copy
// left, and the delivery facts must be recoverable from it alone.
const PullMetadataKey = "yuuka-pull"

// MaxPullMetadata bounds len(PullMetadataKey) plus the encoded value. R2
// allows 8 KiB of metadata per object; 2 KiB is the S3 limit, measured the
// same way (key plus value bytes), so the objects stay portable to any S3
// store. A value over it is refused, never truncated: a truncated
// description is a wrong one.
const MaxPullMetadata = 2048

// PullMeta describes where a pulled message came from. It is encoded as
// base64url (no padding) of a JSON object, as yuuka-marker is and for the
// same reason: R2 hands non-ASCII metadata back RFC 2047 encoded through
// the S3 API, so the value is kept ASCII.
//
// The JSON object is
//
//	{"v":1,"account":…,"source_key":…,"mailbox":…,"uidvalidity":…,"uid":…,"internal_date":…}
//
// with internal_date in RFC 3339 keeping the remote offset. A decoder
// refuses an unknown v and ignores unknown fields, so adding a field is
// not a breaking change and changing one's meaning is.
type PullMeta struct {
	Account      string // the account's name
	SourceKey    string // the delivery's idempotency key within the account
	Mailbox      string // the remote mailbox name, UTF-8
	UIDValidity  uint32
	UID          uint32
	InternalDate time.Time // the remote INTERNALDATE, as the server said it
}

// pullMetaV is the only version of PullMeta's encoding.
const pullMetaV = 1

type pullMetaJSON struct {
	V            int       `json:"v"`
	Account      string    `json:"account"`
	SourceKey    string    `json:"source_key"`
	Mailbox      string    `json:"mailbox"`
	UIDValidity  uint32    `json:"uidvalidity"`
	UID          uint32    `json:"uid"`
	InternalDate time.Time `json:"internal_date"`
}

func (m PullMeta) check() error {
	for _, f := range []struct{ name, v string }{
		{"account", m.Account}, {"source_key", m.SourceKey}, {"mailbox", m.Mailbox},
	} {
		switch {
		case f.v == "":
			return fmt.Errorf("%s: empty", f.name)
		case !utf8.ValidString(f.v):
			// encoding/json would quietly replace the bad bytes with
			// U+FFFD, and the stored description would not be this one.
			return fmt.Errorf("%s: not UTF-8", f.name)
		}
	}
	switch {
	case m.UIDValidity == 0:
		return errors.New("uidvalidity: zero")
	case m.UID == 0:
		return errors.New("uid: zero")
	case m.InternalDate.IsZero():
		return errors.New("internal_date: zero")
	}
	return nil
}

// Encode returns the yuuka-pull metadata value for m. It fails on a
// missing field or when the value would exceed MaxPullMetadata.
func (m PullMeta) Encode() (string, error) {
	if err := m.check(); err != nil {
		return "", fmt.Errorf("encode %s metadata: %w", PullMetadataKey, err)
	}
	b, err := json.Marshal(pullMetaJSON{
		V: pullMetaV, Account: m.Account, SourceKey: m.SourceKey, Mailbox: m.Mailbox,
		UIDValidity: m.UIDValidity, UID: m.UID, InternalDate: m.InternalDate,
	})
	if err != nil {
		return "", fmt.Errorf("encode %s metadata: %w", PullMetadataKey, err)
	}
	v := base64.RawURLEncoding.EncodeToString(b)
	if n := len(PullMetadataKey) + len(v); n > MaxPullMetadata {
		return "", fmt.Errorf("encode %s metadata: %d bytes, over %d", PullMetadataKey, n, MaxPullMetadata)
	}
	return v, nil
}

// DecodePullMeta reverses Encode.
func DecodePullMeta(value string) (PullMeta, error) {
	if n := len(PullMetadataKey) + len(value); n > MaxPullMetadata {
		return PullMeta{}, fmt.Errorf("decode %s metadata: %d bytes, over %d", PullMetadataKey, n, MaxPullMetadata)
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil {
		return PullMeta{}, fmt.Errorf("decode %s metadata: %w", PullMetadataKey, err)
	}
	var j pullMetaJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return PullMeta{}, fmt.Errorf("decode %s metadata: %w", PullMetadataKey, err)
	}
	if j.V != pullMetaV {
		return PullMeta{}, fmt.Errorf("decode %s metadata: version %d", PullMetadataKey, j.V)
	}
	m := PullMeta{
		Account: j.Account, SourceKey: j.SourceKey, Mailbox: j.Mailbox,
		UIDValidity: j.UIDValidity, UID: j.UID, InternalDate: j.InternalDate,
	}
	if err := m.check(); err != nil {
		return PullMeta{}, fmt.Errorf("decode %s metadata: %w", PullMetadataKey, err)
	}
	return m, nil
}

// PutPull stores body under key, which must pass CheckPullKey, with meta
// as its yuuka-pull metadata, only if nothing is stored there yet
// (If-None-Match: *, as PutSent).
//
// Unlike PutSent it settles a taken key itself, since the comparison
// covers the metadata, whose encoding is this package's: a 412 is a retry
// whose first try landed, or a bug, so the stored object is read back and
// PutPull succeeds only if it holds exactly body and meta. Any other
// stored object is an error, and it is left as it was.
func (r *RawBucket) PutPull(ctx context.Context, key string, body []byte, meta PullMeta) error {
	if err := CheckPullKey(key); err != nil {
		return err
	}
	v, err := meta.Encode()
	if err != nil {
		return err
	}
	err = r.create(ctx, key, body, map[string]string{PullMetadataKey: v})
	if !errors.Is(err, ErrExists) {
		return err
	}
	// Not wrapping ErrExists from here on: to a PutSent caller it means
	// "compare and maybe succeed", and PutPull has already compared.
	obj, err := r.Get(ctx, key)
	switch {
	case err != nil:
		return fmt.Errorf("put %s/%s: key taken; reading it back: %w", r.b.name, key, err)
	case !bytes.Equal(obj.Body, body):
		return fmt.Errorf("put %s/%s: already exists with different content", r.b.name, key)
	case obj.Metadata[PullMetadataKey] != v:
		return fmt.Errorf("put %s/%s: already exists with different %s metadata", r.b.name, key, PullMetadataKey)
	}
	return nil
}
