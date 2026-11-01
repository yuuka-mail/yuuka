// Package manage holds the management actions on transports, channels,
// bindings and their sealed secrets (§17.6) that both the host CLI and
// the REST API perform. The checks, the sealing, what an action records
// and when a change counts as a widening are written once here; the
// callers decide who is acting (Act), how to ask for what is missing and
// how to say what happened.
package manage

import (
	"errors"

	"github.com/yuuka-mail/yuuka/internal/store"
)

// Act makes the audit entry of a change: the host CLI records the system
// principal and the host user, the REST API the person and their session
// (§13.7, §13.10).
type Act func(tool string, args map[string]any) store.Action

// ErrLocked: the change seals or opens a secret, and there is no key to
// do it with (YUUKA_KEK_FILE is not set, §17.3).
var ErrLocked = errors.New("no key to seal or open third-party secrets with: YUUKA_KEK_FILE is not set")

// Invalid is a change that cannot be made as given, for Reason: a host
// that is not one, a port out of range, an unknown cap. Nothing was
// written.
type Invalid struct{ Reason string }

func (e *Invalid) Error() string { return e.Reason }
