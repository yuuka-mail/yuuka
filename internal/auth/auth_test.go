package auth

import (
	"strings"
	"testing"
)

func TestSecretRoundTrip(t *testing.T) {
	t.Parallel()
	id := "01M67SX66B38NKRKAYDXR834N3"
	s := NewSecret(PrefixToken, id)
	if !strings.HasPrefix(s.Text, "ykt_"+id+"_") {
		t.Fatalf("format %q", s.Text)
	}
	gotID, h, ok := ParseSecret(PrefixToken, s.Text)
	if !ok || gotID != id || !HashEqual(s.Hash[:], h) {
		t.Fatal("round trip failed")
	}
	if _, _, ok := ParseSecret(PrefixSession, s.Text); ok {
		t.Fatal("token parsed as a session")
	}
	for _, bad := range []string{"", "ykt_", "ykt_" + id, "ykt_" + id + "_short", "ykt_bad_" + strings.Repeat("A", 43), s.Text + "A"} {
		if _, _, ok := ParseSecret(PrefixToken, bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
	other := NewSecret(PrefixToken, id)
	if HashEqual(s.Hash[:], other.Hash) {
		t.Fatal("two secrets collide")
	}
}

func TestRecoveryCode(t *testing.T) {
	t.Parallel()
	c := NewRecoveryCode()
	if len(c) != 19 || strings.Count(c, "-") != 3 {
		t.Fatalf("format %q", c)
	}
	want, ok := HashRecoveryCode(c)
	if !ok {
		t.Fatal("own code rejected")
	}
	for _, typed := range []string{strings.ToLower(c), strings.ReplaceAll(c, "-", " "), strings.ReplaceAll(c, "-", "")} {
		if got, ok := HashRecoveryCode(typed); !ok || got != want {
			t.Errorf("%q does not match %q", typed, c)
		}
	}
	// Crockford look-alikes fold to digits.
	a, _ := HashRecoveryCode("O0IL-0000-0000-0000")
	b, _ := HashRecoveryCode("0011-0000-0000-0000")
	if a != b {
		t.Error("O/I/L not folded")
	}
	for _, bad := range []string{"", "0000-0000-0000", "0000-0000-0000-000U", "0000-0000-0000-00000"} {
		if _, ok := HashRecoveryCode(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestViewerScopes(t *testing.T) {
	t.Parallel()
	v := &Viewer{Scopes: []Scope{MessageRead, Admin}}
	if !v.Has(Admin) || v.Has(SubmissionSend) {
		t.Fatal("Has")
	}
	if m := v.Missing(MessageRead, DraftCreate, SubmissionSend); len(m) != 2 || m[0] != DraftCreate {
		t.Fatalf("Missing %v", m)
	}
	// Flat: admin implies nothing else.
	if (&Viewer{Scopes: []Scope{Admin}}).Has(MessageRead) {
		t.Fatal("admin implied message:read")
	}
	var none *Viewer
	if none.Has(MessageRead) {
		t.Fatal("nil viewer has a scope")
	}
	if _, err := ParseScopes([]string{"message:read", "root"}); err == nil {
		t.Fatal("unknown scope parsed")
	}
}
