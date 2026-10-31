package ulid

import (
	"testing"
	"time"
)

func TestNewIsValidAndCarriesTime(t *testing.T) {
	now := time.Date(2026, 10, 31, 1, 41, 38, 123e6, time.UTC)
	id := New(now)
	if !Valid(id) {
		t.Fatalf("New produced invalid ULID %q", id)
	}
	// The first 10 digits encode the millisecond timestamp, so two ids
	// from different milliseconds must sort by time.
	later := New(now.Add(time.Millisecond))
	if id[:10] >= later[:10] {
		t.Fatalf("timestamp prefix not increasing: %s then %s", id, later)
	}
}

func TestEncodeKnownValue(t *testing.T) {
	var max [16]byte
	for i := range max {
		max[i] = 0xff
	}
	if got := encode(max); got != "7ZZZZZZZZZZZZZZZZZZZZZZZZZ" {
		t.Fatalf("encode(max) = %s", got)
	}
	if got := encode([16]byte{}); got != "00000000000000000000000000" {
		t.Fatalf("encode(zero) = %s", got)
	}
}

func TestValid(t *testing.T) {
	for _, tc := range []struct {
		s  string
		ok bool
	}{
		{"01M67SX66B38NKRKAYDXR834N3", true},
		{"01m67sx66b38nkrkaydxr834n3", false},
		{"81M67SX66B38NKRKAYDXR834N3", false},
		{"01M67SX66B38NKRKAYDXR834NU", false},
		{"01M67SX66B38NKRKAYDXR834N", false},
	} {
		if got := Valid(tc.s); got != tc.ok {
			t.Errorf("Valid(%q) = %v, want %v", tc.s, got, tc.ok)
		}
	}
}
