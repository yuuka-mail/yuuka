package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/openapi"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// ChangesParams are the parameters of GET /api/v1/changes.
type ChangesParams struct {
	Since *ChangeCursor          `query:"since" jsonschema:"cursor of the previous call; absent starts from now, with no changes"`
	Kinds []agentview.ChangeKind `query:"kinds" jsonschema:"only these kinds; absent is every kind the credential's scopes cover"`
	Limit int                    `query:"limit" minimum:"1" maximum:"500" default:"100"`
}

// ChangeCursor is a position in the change log, opaque to clients
// (agentview.ChangeCursor).
type ChangeCursor int64

func (c *ChangeCursor) UnmarshalText(b []byte) error {
	seq, err := agentview.ParseChangeCursor(string(b))
	*c = ChangeCursor(seq)
	return err
}

// ChangePage is what changed after a cursor (§8.6).
type ChangePage struct {
	// Changes is in the order they committed, each object once. They
	// say what changed, not how: read the object through its own
	// endpoint, which may show a later state, or 404 if it is gone.
	Changes []Change `json:"changes"`
	// Cursor continues after this page: pass it as since. It is always
	// set, and moves past changes the caller may not see.
	Cursor string `json:"cursor"`
	// More: there is more after cursor; ask again right away. A page
	// can be short, even empty, while there is more.
	More bool `json:"more"`
}

// Change is one object that changed.
type Change struct {
	Kind agentview.ChangeKind `json:"kind"`
	// ID is the object's id: a ULID, or prefix@zone for a prefix. A
	// flag change is the message's labels, class or archived state.
	ID string `json:"id"`
	// MessageID is the message a delivery belongs to, or the message a
	// note is on (its notes are in GET /api/v1/messages/{id}); absent
	// for other kinds and for a deleted note.
	MessageID string    `json:"message_id,omitempty"`
	At        time.Time `json:"at"`
}

func (a *API) changes(ctx context.Context, v *auth.Viewer, p ChangesParams) (ChangePage, error) {
	kinds, missing := agentview.FollowedKinds(v, p.Kinds)
	if len(missing) > 0 {
		return ChangePage{}, openapi.Errorf(http.StatusForbidden, openapi.CodeScopeRequired,
			"these kinds need %s", strings.Join(auth.Strings(missing), ", "))
	}
	q := store.ChangeQuery{Kinds: kinds, Limit: p.Limit}
	if p.Since != nil {
		q.Since = (*int64)(p.Since)
	}
	page, err := a.db.Changes(ctx, v, q)
	if err != nil {
		return ChangePage{}, err
	}
	out := ChangePage{Changes: make([]Change, len(page.Changes)), Cursor: agentview.ChangeCursor(page.Cursor), More: page.More}
	for i, c := range page.Changes {
		out.Changes[i] = Change{Kind: agentview.ChangeKind(c.Kind), ID: c.ID, MessageID: c.MessageID, At: c.At.UTC()}
	}
	return out, nil
}
