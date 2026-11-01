package r2

import "testing"

// TestCreateGate checks the second line of §2.2's code gate: the raw
// bucket's one PutObject refuses a key outside sent/ and pull/ even when
// a write method forgets its own check. The endpoint is unreachable, so
// any key that got past the gate would fail differently and slowly.
func TestCreateGate(t *testing.T) {
	r := NewRawBucket(Config{Endpoint: "http://127.0.0.1:0", Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s"})
	for _, k := range []string{
		"raw/2026/10/01M67SX66B38NKRKAYDXR834N3.eml",
		"pending/01M67SX66B38NKRKAYDXR834N3",
		"pull/../raw/10/01M67SX66B38NKRKAYDXR834N3.eml",
		"sent/2026/10/x.eml",
	} {
		err := r.create(t.Context(), k, []byte("x"), nil)
		if err == nil || err.Error() != `r2: refusing to write "`+k+`" to the raw bucket` {
			t.Errorf("create(%q) = %v", k, err)
		}
	}
}
