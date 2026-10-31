package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/yuuka-mail/yuuka/internal/secret"
)

// loadSealer reads the KEK files that seal third-party secrets (§17.3).
// Without YUUKA_KEK_FILE it returns a nil Sealer, the locked state: every
// Seal and Open fails with secret.ErrLocked, and the caller decides what
// waits (sending through a configured transport, a channel) while
// receiving and everything else run. A KEK file that is named but
// unreadable or malformed is an error instead: a configuration mistake,
// not a state to run degraded in.
func loadSealer(getenv func(string) string) (*secret.Sealer, error) {
	cur := getenv("YUUKA_KEK_FILE")
	var prev []string
	for p := range strings.SplitSeq(getenv("YUUKA_KEK_PREVIOUS_FILES"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			prev = append(prev, p)
		}
	}
	if cur == "" {
		if len(prev) > 0 {
			return nil, errors.New("YUUKA_KEK_PREVIOUS_FILES only unwraps, after a rotation: it needs YUUKA_KEK_FILE")
		}
		return nil, nil
	}
	ring, err := secret.LoadKeyring(cur, prev)
	if err != nil {
		return nil, fmt.Errorf("YUUKA_KEK_FILE: %w", err)
	}
	return secret.NewSealer(ring)
}
