package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// ChangesInput is the input of changes_since.
type ChangesInput struct {
	Cursor string                 `json:"cursor,omitempty" jsonschema:"the cursor the previous call returned; absent returns the current cursor and no changes"`
	Kinds  []agentview.ChangeKind `json:"kinds,omitempty" jsonschema:"only these kinds; absent is every kind your scopes cover"`
	Limit  int                    `json:"limit,omitempty" jsonschema:"at most this many changes; default 100, at most 500"`
}

// ChangesOutput is the structured result of changes_since.
type ChangesOutput struct {
	Changes []Change `json:"changes"`
	// Cursor is what to pass next time; it is always set.
	Cursor string `json:"cursor"`
	// More: call again right away with cursor.
	More bool `json:"more"`
}

// Change is one object that changed. No content: the caller reads it.
type Change struct {
	Kind agentview.ChangeKind `json:"kind"`
	ID   string               `json:"id"`
	// URI is the message to read with get_message: of a message or flag
	// change, the message a delivery belongs to, or the one a note is
	// on.
	URI string    `json:"uri,omitempty"`
	At  time.Time `json:"at"`
}

func (s *Server) changesSince(ctx context.Context, v *auth.Viewer, in ChangesInput) (result, error) {
	kinds, missing := agentview.FollowedKinds(v, in.Kinds)
	if len(missing) > 0 {
		return result{}, refuse("these kinds need %s", strings.Join(auth.Strings(missing), ", "))
	}
	q := store.ChangeQuery{Kinds: kinds, Limit: in.Limit}
	if in.Cursor != "" {
		seq, err := agentview.ParseChangeCursor(in.Cursor)
		if err != nil {
			return result{}, refuse("cursor: %v", err)
		}
		q.Since = &seq
	}
	page, err := s.db.Changes(ctx, v, q)
	if errors.Is(err, store.ErrCursorExpired) {
		return result{}, refuse("cursor_expired: start again without a cursor and reread what you hold")
	}
	if err != nil {
		return result{}, err
	}
	out := ChangesOutput{Changes: make([]Change, len(page.Changes)), Cursor: agentview.ChangeCursor(page.Cursor), More: page.More}
	var text strings.Builder
	switch {
	case in.Cursor == "":
		text.WriteString("Following from now. Call again with this cursor to see what changes.\n")
	case len(page.Changes) == 0:
		text.WriteString("Nothing changed.\n")
	default:
		fmt.Fprintf(&text, "%d changed, oldest first. Read them to see how.\n", len(page.Changes))
	}
	for i, c := range page.Changes {
		ch := Change{Kind: agentview.ChangeKind(c.Kind), ID: c.ID, At: c.At.UTC()}
		switch {
		case c.Kind == "message" || c.Kind == "flag":
			ch.URI = agentview.MessageURI(c.ID)
		case c.MessageID != "":
			ch.URI = agentview.MessageURI(c.MessageID)
		}
		out.Changes[i] = ch
		// Ids and kinds only: nothing a sender wrote.
		fmt.Fprintf(&text, "%d. %s %s", i+1, ch.Kind, ch.ID)
		if ch.URI != "" && ch.URI != agentview.MessageURI(ch.ID) {
			fmt.Fprintf(&text, " of %s", ch.URI)
		}
		text.WriteString("\n")
	}
	if page.More {
		text.WriteString("There is more: call again with the cursor.\n")
	}
	fmt.Fprintf(&text, "cursor: %s\n", out.Cursor)
	// An empty poll disclosed nothing, and agents poll: it is not
	// recorded, so the audit log keeps what an agent learned (§8.8).
	return result{
		structured: out, text: text.String(), audit: map[string]any{"returned": len(page.Changes)},
		recorded: len(page.Changes) == 0,
	}, nil
}
