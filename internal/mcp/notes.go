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

// Notes (§8.10): a principal's signed text on a message or its thread,
// one per author per target, overwritten in place; delete_note takes the
// caller's back. A note is a reading, not a fact, and drives nothing.
// get_message gives the notes the store shows the caller on the message
// or its thread (§13.8), each signed, fenced apart from yuuka's facts.

// NoteInput is the input of note.
type NoteInput struct {
	URI   string                  `json:"uri" jsonschema:"yuuka://message/<id> of the message"`
	Text  string                  `json:"text" jsonschema:"the note, at most 4000 characters; it replaces your earlier note on the same target"`
	Cites []Cite                  `json:"cites" jsonschema:"required, 1 to 32: the passages the note relies on, as get_message anchors them, each quote at most 1000 characters"`
	Scope agentview.AnnotateScope `json:"scope,omitempty" jsonschema:"message (default), or thread: one note about the whole thread of the message"`
}

// auditArgs keep the note's shape, never its text or the quotes.
func (in NoteInput) auditArgs() map[string]any {
	return map[string]any{"uri": in.URI, "scope": in.Scope, "cites": len(in.Cites), "chars": len([]rune(in.Text))}
}

// DeleteNoteInput is the input of delete_note.
type DeleteNoteInput struct {
	URI   string                  `json:"uri" jsonschema:"yuuka://message/<id> of the message"`
	Scope agentview.AnnotateScope `json:"scope,omitempty" jsonschema:"message (default), or thread: your note about the message's thread"`
}

// Note is a note, signed.
type Note struct {
	ID string `json:"id"`
	// Target is the message it is on; for scope thread, the message it
	// is anchored on, whose thread it is about.
	Target    string                  `json:"target"`
	Scope     agentview.AnnotateScope `json:"scope"`
	Author    string                  `json:"author"` // kind:name
	Text      string                  `json:"text"`
	Cites     []Cite                  `json:"cites"`
	CreatedAt time.Time               `json:"created_at"`
	UpdatedAt time.Time               `json:"updated_at"`
}

func noteOf(n store.Note) Note {
	out := Note{
		ID: n.ID, Target: agentview.MessageURI(n.MessageID), Scope: agentview.AnnotateScope(n.Scope), Author: n.Author,
		Text: n.Text, Cites: make([]Cite, len(n.Cites)), CreatedAt: n.CreatedAt.UTC(), UpdatedAt: n.UpdatedAt.UTC(),
	}
	for i, c := range n.Cites {
		out.Cites[i] = Cite{URI: c.URI, Quote: c.Quote}
	}
	return out
}

// noteScope is the store's scope of an input's.
func noteScope(s agentview.AnnotateScope) string {
	if s == "thread" {
		return store.ScopeThread
	}
	return store.ScopeMessage
}

func (s *Server) writeNote(ctx context.Context, v *auth.Viewer, in NoteInput) (result, error) {
	id, _, err := agentview.ParseURI(in.URI)
	if err != nil {
		return result{}, refuse("%v", err)
	}
	if len(in.Cites) == 0 {
		return result{}, refuse("cites: a note needs at least one: the passages it relies on, as get_message anchors them")
	}
	cites := make([]store.Cite, len(in.Cites))
	for i, c := range in.Cites {
		cites[i] = store.Cite{URI: c.URI, Quote: c.Quote}
	}
	if err := agentview.CheckCites(cites); err != nil {
		return result{}, refuse("%v", err)
	}
	n, created, err := s.db.WriteNote(ctx, v, store.NewNote{MessageID: id, Scope: noteScope(in.Scope), Text: in.Text, Cites: cites},
		s.action(v, "note", in.auditArgs()))
	switch {
	case errors.Is(err, store.ErrNotFound):
		return result{}, refuse("no message %s", agentview.MessageURI(id))
	case errors.Is(err, store.ErrInvalidNote):
		return result{}, refuse("%v", err)
	case err != nil:
		return result{}, err
	}
	out := noteOf(n)
	verb := "Replaced your"
	if created {
		verb = "Wrote a"
	}
	text := fmt.Sprintf("%s %s note %s on %s. A person sees it signed as yours; other agents do not see it. "+
		"It drives nothing: no rule, score or review reads it. delete_note takes it back.\n", verb, out.Scope, out.ID, out.Target)
	return result{structured: out, text: text, recorded: true}, nil
}

// DeleteNoteOutput is the structured result of delete_note.
type DeleteNoteOutput struct {
	Deleted bool `json:"deleted"`
}

func (s *Server) deleteNote(ctx context.Context, v *auth.Viewer, in DeleteNoteInput) (result, error) {
	id, _, err := agentview.ParseURI(in.URI)
	if err != nil {
		return result{}, refuse("%v", err)
	}
	scope := noteScope(in.Scope)
	// Own notes only, whoever the caller is: deleting a note of another
	// is a person's act in the WebUI, never a model's.
	err = s.db.DeleteNote(ctx, v, id, scope, "", s.action(v, "delete_note", in))
	if errors.Is(err, store.ErrNotFound) {
		return result{}, refuse("you have no %s note on %s", scope, agentview.MessageURI(id))
	}
	if err != nil {
		return result{}, err
	}
	return result{
		structured: DeleteNoteOutput{Deleted: true}, recorded: true,
		text: fmt.Sprintf("Deleted your %s note on %s.\n", scope, agentview.MessageURI(id)),
	}, nil
}

// notes are the notes on message id and its thread.
func (s *Server) notes(ctx context.Context, v *auth.Viewer, id string) ([]Note, error) {
	ns, err := s.db.Notes(ctx, v, id)
	if err != nil {
		return nil, err
	}
	out := make([]Note, len(ns))
	for i, n := range ns {
		out[i] = noteOf(n)
	}
	return out, nil
}

// notesText lists the notes' facts (who, what, when) and then their
// text and cites inside one untrusted boundary, numbered alike.
func notesText(uri string, ns []Note) string {
	if len(ns) == 0 {
		return ""
	}
	var facts, said strings.Builder
	fmt.Fprintf(&facts, "\n%d note(s) on this message or its thread (each its author's, not yuuka's; they drive nothing):\n", len(ns))
	for i, n := range ns {
		fmt.Fprintf(&facts, "%d. note %s · by %s · %s · on %s · updated %s\n",
			i+1, n.ID, n.Author, n.Scope, n.Target, n.UpdatedAt.Format(time.RFC3339))
		fmt.Fprintf(&said, "%d. %s\n", i+1, strings.ReplaceAll(n.Text, "\n", "\n   "))
		for _, c := range n.Cites {
			fmt.Fprintf(&said, "   Cites %s: %s\n", c.URI, c.Quote)
		}
	}
	return facts.String() + "\n" + agentview.FenceNotes(uri+" notes", said.String())
}
