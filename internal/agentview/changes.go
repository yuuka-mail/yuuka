package agentview

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"

	"github.com/yuuka-mail/yuuka/internal/auth"
)

// ChangeKind is what one entry of the change log (§8.6) is about. REST
// and MCP share it, and the scopes a kind needs.
type ChangeKind string

func (ChangeKind) Enum() []string {
	return []string{"message", "delivery", "thread", "flag", "prefix", "proposal", "draft", "outbox", "rule"}
}

// Scopes are what a credential needs to follow changes of kind k, beyond
// message:read: the scopes of the reads a client follows the change
// with. Deliveries and the outbox are delivery data (§13.4).
func (k ChangeKind) Scopes() []auth.Scope {
	switch k {
	case "delivery", "outbox":
		return []auth.Scope{auth.DeliveryRead}
	case "prefix":
		return []auth.Scope{auth.PrefixRead}
	case "rule":
		return []auth.Scope{auth.RuleManage}
	}
	return nil
}

// FollowedKinds resolves the kinds v asked for: none asked is every kind
// v's scopes cover; a kind asked for without its scope is refused with
// the scopes it lacks, rather than quietly left out.
func FollowedKinds(v *auth.Viewer, asked []ChangeKind) ([]string, []auth.Scope) {
	var out []string
	if len(asked) == 0 {
		for _, k := range ChangeKind("").Enum() {
			if len(v.Missing(ChangeKind(k).Scopes()...)) == 0 {
				out = append(out, k)
			}
		}
		return out, nil
	}
	for _, k := range asked {
		if missing := v.Missing(k.Scopes()...); len(missing) > 0 {
			return nil, missing
		}
		out = append(out, string(k))
	}
	return out, nil
}

// The change cursor is changes.seq, opaque to clients (§5.1): base64url
// of "c1 <seq>". The tag leaves room to add to it, say an epoch that
// tells a restored database apart, without a client noticing.
const cursorTag = "c1 "

// ChangeCursor encodes seq.
func ChangeCursor(seq int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursorTag + strconv.FormatInt(seq, 10)))
}

// ParseChangeCursor decodes a cursor ChangeCursor made.
func ParseChangeCursor(s string) (int64, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		if n, ok := strings.CutPrefix(string(b), cursorTag); ok {
			if seq, err := strconv.ParseInt(n, 10, 64); err == nil && seq >= 0 {
				return seq, nil
			}
		}
	}
	return 0, errors.New("malformed; pass the cursor back as it came")
}
