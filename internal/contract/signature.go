package contract

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Webhook header names (§2.2).
const (
	HeaderTimestamp = "Yuuka-Timestamp"
	HeaderSignature = "Yuuka-Signature"
)

// SignatureWindow is how far the webhook timestamp may be from the local
// clock, in either direction, inclusive.
const SignatureWindow = 300 * time.Second

// ErrBadSignature is wrapped by every Verify failure.
var ErrBadSignature = errors.New("bad webhook signature")

// Sign returns the Yuuka-Signature value for body sent at timestamp.
func Sign(secret []byte, timestamp int64, body []byte) string {
	return "v1=" + hex.EncodeToString(mac(secret, strconv.FormatInt(timestamp, 10), body))
}

func mac(secret []byte, timestamp string, body []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte("v1:" + timestamp + ":"))
	h.Write(body)
	return h.Sum(nil)
}

// Verify checks a webhook against any of secrets (several are accepted so a
// key can be rotated without downtime). timestamp and signature are the raw
// header values; body is the request body exactly as received.
func Verify(secrets [][]byte, now time.Time, timestamp string, body []byte, signature string) error {
	ts, err := parseUnix(timestamp)
	if err != nil {
		return fmt.Errorf("%w: timestamp: %w", ErrBadSignature, err)
	}
	if d := now.Unix() - ts; d > int64(SignatureWindow/time.Second) || -d > int64(SignatureWindow/time.Second) {
		return fmt.Errorf("%w: timestamp %d is %ds away from local clock", ErrBadSignature, ts, d)
	}
	// Only the exact lowercase-hex v1 form is accepted; comparing the hex
	// text (rather than decoding it) rejects uppercase for free.
	got := []byte(signature)
	for _, s := range secrets {
		want := []byte("v1=" + hex.EncodeToString(mac(s, timestamp, body)))
		if hmac.Equal(got, want) {
			return nil
		}
	}
	return fmt.Errorf("%w: no secret matches", ErrBadSignature)
}

// parseUnix accepts only plain decimal digits, so "+1" or " 1" cannot sign
// as a different string than the one the Worker used.
func parseUnix(s string) (int64, error) {
	if s == "" || len(s) > 18 {
		return 0, fmt.Errorf("%q is not a unix time", s)
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("%q is not a unix time", s)
		}
	}
	return strconv.ParseInt(s, 10, 64)
}
