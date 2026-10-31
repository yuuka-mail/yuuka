// Package contract implements the Go side of the Worker <-> Go contract v1
// (docs/design/architecture.md §2.2): the delivery marker, the R2 key layout, the
// yuuka-marker object metadata and the webhook signature.
//
// The shared vectors under testdata/contract/v1 are the authority; this
// package must keep passing them byte for byte.
package contract

import (
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Version is the only marker version this package understands.
const Version = 1

// MetadataKey is the R2 custom metadata key on raw objects that carries the
// marker, base64url-encoded without padding.
const MetadataKey = "yuuka-marker"

// timeLayout is the one accepted time form: UTC, millisecond precision, "Z".
// Anything looser would break byte-exact round trips with the Worker.
const timeLayout = "2006-01-02T15:04:05.000Z"

// Marker is a delivery marker: one Worker invocation, i.e. one RCPT TO.
type Marker struct {
	ID           string
	ReceivedAt   time.Time
	EnvelopeFrom string // "" for the null sender
	EnvelopeTo   string
	RawKey       string
	RawSize      int64
}

// ErrInvalid is wrapped by every Parse error caused by the input itself.
var ErrInvalid = errors.New("invalid marker")

// wire mirrors the JSON with pointers so a missing field can be told apart
// from a zero value (envelope_from may legitimately be "").
type wire struct {
	V            *int64  `json:"v"`
	ID           *string `json:"id"`
	ReceivedAt   *string `json:"received_at"`
	EnvelopeFrom *string `json:"envelope_from"`
	EnvelopeTo   *string `json:"envelope_to"`
	RawKey       *string `json:"raw_key"`
	RawSize      *int64  `json:"raw_size"`
}

// Parse decodes and validates a v1 marker. Unknown fields are ignored;
// an unknown version is rejected rather than guessed at.
//
// encoding/json/v2 is used because it matches member names exactly and
// rejects duplicate names, where v1 would let "ID" or a second "id" through.
func Parse(data []byte) (Marker, error) {
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return Marker{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if w.V == nil {
		return Marker{}, fmt.Errorf("%w: v is required", ErrInvalid)
	}
	if *w.V != Version {
		return Marker{}, fmt.Errorf("%w: unsupported version %d", ErrInvalid, *w.V)
	}
	for name, p := range map[string]*string{
		"id": w.ID, "received_at": w.ReceivedAt, "envelope_from": w.EnvelopeFrom,
		"envelope_to": w.EnvelopeTo, "raw_key": w.RawKey,
	} {
		if p == nil {
			return Marker{}, fmt.Errorf("%w: %s is required", ErrInvalid, name)
		}
	}
	if w.RawSize == nil {
		return Marker{}, fmt.Errorf("%w: raw_size is required", ErrInvalid)
	}
	m := Marker{
		ID:           *w.ID,
		EnvelopeFrom: *w.EnvelopeFrom,
		EnvelopeTo:   *w.EnvelopeTo,
		RawKey:       *w.RawKey,
		RawSize:      *w.RawSize,
	}
	if !ulid.Valid(m.ID) {
		return Marker{}, fmt.Errorf("%w: id %q is not an uppercase ULID", ErrInvalid, m.ID)
	}
	t, err := ParseTime(*w.ReceivedAt)
	if err != nil {
		return Marker{}, fmt.Errorf("%w: received_at: %w", ErrInvalid, err)
	}
	m.ReceivedAt = t
	if m.EnvelopeTo == "" {
		return Marker{}, fmt.Errorf("%w: envelope_to is empty", ErrInvalid)
	}
	if want := RawKey(m.ReceivedAt, m.ID); m.RawKey != want {
		return Marker{}, fmt.Errorf("%w: raw_key %q does not match received_at and id (want %q)", ErrInvalid, m.RawKey, want)
	}
	if m.RawSize < 0 {
		return Marker{}, fmt.Errorf("%w: raw_size %d is negative", ErrInvalid, m.RawSize)
	}
	return m, nil
}

// ParseTime parses a contract timestamp, accepting only the exact
// "2006-01-02T15:04:05.000Z" form.
func ParseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("want UTC RFC 3339 with milliseconds and Z: %w", err)
	}
	if FormatTime(t) != s {
		return time.Time{}, fmt.Errorf("%q is not in canonical form", s)
	}
	return t, nil
}

// FormatTime renders t in the contract's time form.
func FormatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

// Marshal serializes m exactly as the Worker does: fields in contract order,
// no whitespace, strings escaped the way JavaScript's JSON.stringify escapes
// them. encoding/json is not used because it escapes <, >, & and U+2028/9,
// which JSON.stringify leaves alone.
func Marshal(m Marker) []byte {
	var b strings.Builder
	b.WriteString(`{"v":`)
	b.WriteString(strconv.Itoa(Version))
	b.WriteString(`,"id":`)
	writeString(&b, m.ID)
	b.WriteString(`,"received_at":`)
	writeString(&b, FormatTime(m.ReceivedAt))
	b.WriteString(`,"envelope_from":`)
	writeString(&b, m.EnvelopeFrom)
	b.WriteString(`,"envelope_to":`)
	writeString(&b, m.EnvelopeTo)
	b.WriteString(`,"raw_key":`)
	writeString(&b, m.RawKey)
	b.WriteString(`,"raw_size":`)
	b.WriteString(strconv.FormatInt(m.RawSize, 10))
	b.WriteByte('}')
	return []byte(b.String())
}

func writeString(b *strings.Builder, s string) {
	const hex = "0123456789abcdef"
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte(hex[r>>4])
				b.WriteByte(hex[r&0xf])
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

// RawKey is the raw bucket key for an invocation: raw/YYYY/MM/<id>.eml,
// with year and month taken from receivedAt in UTC.
func RawKey(receivedAt time.Time, id string) string {
	t := receivedAt.UTC()
	return fmt.Sprintf("raw/%04d/%02d/%s.eml", t.Year(), int(t.Month()), id)
}

// PendingKey is the pending bucket key of the marker for id.
func PendingKey(id string) string {
	return "pending/" + id
}

// IDFromPendingKey returns the id part of a pending key.
func IDFromPendingKey(key string) (string, bool) {
	id, ok := strings.CutPrefix(key, "pending/")
	return id, ok && ulid.Valid(id)
}

// EncodeMetadata encodes marker JSON for the yuuka-marker metadata value.
func EncodeMetadata(markerJSON []byte) string {
	return base64.RawURLEncoding.EncodeToString(markerJSON)
}

// DecodeMetadata reverses EncodeMetadata. It does not parse the marker.
func DecodeMetadata(value string) ([]byte, error) {
	b, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("decode %s metadata: %w", MetadataKey, err)
	}
	return b, nil
}
