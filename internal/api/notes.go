package api

import (
	"context"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/openapi"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// Notes (§8.10): a principal's signed text on a message or its thread,
// one per author per target, overwritten in place and taken back by
// deleting it. A person sees every note on mail they can see, anyone
// else only its own (§13.8), which the store decides. Anyone deletes
// its own notes; a person also anyone else's they can see. Notes are the
// author's words, written after reading mail: untrusted, and they drive
// nothing.

// Note is one principal's note on a message or its thread. It is the
// author's, in the agent's voice when an agent wrote it (§12.5): never
// yuuka's facts, and it may repeat whatever the mail it read injected.
type Note struct {
	ID     string `json:"id"`
	Author Actor  `json:"author"`
	// MessageID is the message the note is on; for scope thread, the
	// message it is anchored on, whose thread it is about.
	MessageID string                  `json:"message_id"`
	Scope     agentview.AnnotateScope `json:"scope"`
	Text      string                  `json:"text"`
	Cites     []Cite                  `json:"cites"`
	CreatedAt time.Time               `json:"created_at"`
	UpdatedAt time.Time               `json:"updated_at"`
}

// NoteBody is a note as written. It replaces the writer's note on the
// same target, if any.
type NoteBody struct {
	Scope agentview.AnnotateScope `json:"scope,omitempty" jsonschema:"message (default), or thread: one note about the message's whole thread"`
	Text  string                  `json:"text" jsonschema:"the note; at most 4000 characters, not empty"`
	Cites []Cite                  `json:"cites" jsonschema:"required, 1 to 32: the passages the note relies on; each quote at most 1000 characters"`
}

// NoteParams are the parameters of PUT /api/v1/messages/{id}/note.
type NoteParams struct {
	ID   string   `path:"id"`
	Body NoteBody `body:"json"`
}

// DeleteNoteParams are the parameters of DELETE /api/v1/messages/{id}/note.
type DeleteNoteParams struct {
	ID    string                  `path:"id"`
	Scope agentview.AnnotateScope `query:"scope" default:"message" jsonschema:"message, or thread: the note about the message's thread"`
	// Author is whose note: absent for one's own. Only a person may name
	// someone else, to take back a wrong or injected agent note.
	Author string `query:"author" jsonschema:"the principal id of the note's author; absent is your own; only a person may delete another's note"`
}

func noteOut(n store.Note) Note {
	kind, name, _ := strings.Cut(n.Author, ":")
	out := Note{
		ID: n.ID, Author: Actor{ID: n.AuthorID, Kind: kind, Name: name}, MessageID: n.MessageID,
		Scope: agentview.AnnotateScope(n.Scope), Text: n.Text, Cites: make([]Cite, len(n.Cites)),
		CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt,
	}
	for i, c := range n.Cites {
		out.Cites[i] = Cite{URI: c.URI, Quote: c.Quote}
	}
	return out
}

func notesOut(ns []store.Note) []Note {
	out := make([]Note, len(ns))
	for i, n := range ns {
		out[i] = noteOut(n)
	}
	return out
}

// citesIn are the cites of a request body as the store holds them, for
// agentview.CheckCites, which proposals and notes share.
func citesIn(cs []Cite) []store.Cite {
	out := make([]store.Cite, len(cs))
	for i, c := range cs {
		out[i] = store.Cite{URI: c.URI, Quote: c.Quote}
	}
	return out
}

func noteScope(s agentview.AnnotateScope) string {
	if s == "thread" {
		return store.ScopeThread
	}
	return store.ScopeMessage
}

func (a *API) writeNote(ctx context.Context, v *auth.Viewer, p NoteParams) (Note, error) {
	b := p.Body
	cites := citesIn(b.Cites)
	if err := agentview.CheckCites(cites); err != nil {
		return Note{}, openapi.Errorf(http.StatusBadRequest, openapi.CodeInvalidBody, "%v", err)
	}
	scope := noteScope(b.Scope)
	// The shape of the note, never its text or quotes: the audit log is
	// not a copy of it.
	n, _, err := a.db.WriteNote(ctx, v, store.NewNote{MessageID: p.ID, Scope: scope, Text: b.Text, Cites: cites},
		action(v, "note.write", map[string]any{"chars": utf8.RuneCountInString(b.Text)}))
	if err != nil {
		return Note{}, err
	}
	return noteOut(n), nil
}

func (a *API) deleteNote(ctx context.Context, v *auth.Viewer, p DeleteNoteParams) (struct{}, error) {
	if p.Author != "" && p.Author != v.PrincipalID {
		if err := peopleOnly(v, "deletes another's note"); err != nil {
			return struct{}{}, err
		}
	}
	return struct{}{}, a.db.DeleteNote(ctx, v, p.ID, noteScope(p.Scope), p.Author, action(v, "note.delete", nil))
}
