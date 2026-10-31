package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Notes (§8.10, migration 0012): a signed piece of text a principal
// keeps on a message or its thread, one per author per target,
// overwritten in place. Deleting one is how it is taken back; there is
// no undo token, since a note changes nothing about the mail.
//
// A note is untrusted: it was written after reading mail and may carry
// what the mail injected. Nothing reads notes to decide anything. The
// rule facts, the scorer and the review never load this table, and the
// readers here are for showing a note to a person, or to its author.

// MaxNoteChars bounds a note's text, as a proposal's rationale.
const MaxNoteChars = 4000

// ErrInvalidNote: a note write that cannot be stored (no text, too long,
// no cites, a bad scope). The caller sent it, so it is theirs to fix.
var ErrInvalidNote = errors.New("invalid note")

// NewNote is a note to write.
type NewNote struct {
	// MessageID is the message the note is on, or for ScopeThread the
	// message whose thread it is about.
	MessageID string
	Scope     string // ScopeMessage or ScopeThread
	Text      string
	// Cites are required: at least one, as for a proposal (§8.5).
	Cites []Cite
}

func (n NewNote) validate() error {
	switch {
	case n.Scope != ScopeMessage && n.Scope != ScopeThread:
		return fmt.Errorf("%w: scope %q: want message or thread", ErrInvalidNote, n.Scope)
	case strings.TrimSpace(n.Text) == "":
		return fmt.Errorf("%w: text is empty; delete the note instead", ErrInvalidNote)
	case utf8.RuneCountInString(n.Text) > MaxNoteChars:
		return fmt.Errorf("%w: text is longer than %d characters", ErrInvalidNote, MaxNoteChars)
	case strings.ContainsRune(n.Text, 0):
		return fmt.Errorf("%w: text contains NUL", ErrInvalidNote)
	case len(n.Cites) == 0:
		return fmt.Errorf("%w: cites: at least one passage the note relies on", ErrInvalidNote)
	}
	for i, c := range n.Cites {
		if c.URI == "" || strings.ContainsRune(c.URI+c.Quote, 0) {
			return fmt.Errorf("%w: cites[%d]: want a uri, and no NUL", ErrInvalidNote, i)
		}
	}
	return nil
}

// Note is a row of notes as a viewer sees it.
type Note struct {
	ID       string
	AuthorID string
	Author   string // kind:name
	// MessageID is the message the note is on; for ScopeThread, the
	// message it is anchored on, whose thread it is about.
	MessageID string
	Scope     string
	Text      string
	Cites     []Cite
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Who sees which notes (§13.8): a person sees every note on mail they
// can see; everyone else only the notes it wrote, as visibleDraft. One
// agent reading another's note is a second-order injection channel, and
// several agents stay out of each other's way. Widening it later is
// easy, narrowing it is not. Every note query passes the viewer's
// principal id as $1.
func (db *DB) noteVisible(v *auth.Viewer) string {
	own := "n.author_id = $1"
	if seesEveryNote(v) {
		own = "$1::text IS NOT NULL" // $1 must appear for its type
	}
	return `(` + own + ` AND EXISTS (SELECT 1 FROM messages nm WHERE nm.id = n.message_id AND ` +
		db.visibleMessage(v, "nm") + `))`
}

// seesEveryNote: v sees the notes of others. It alone sees what is left
// of a deleted note (its change, §8.6).
func seesEveryNote(v *auth.Viewer) bool { return v.PrincipalKind == sendpolicy.KindHuman }

const noteCols = `n.id, n.author_id, p.kind || ':' || p.name, n.message_id, n.scope, n.body, n.cites, n.created_at, n.updated_at`

const noteFrom = ` FROM notes n JOIN principals p ON p.id = n.author_id`

func scanNote(r pgx.Row) (Note, error) {
	var n Note
	err := r.Scan(&n.ID, &n.AuthorID, &n.Author, &n.MessageID, &n.Scope, &n.Text, &n.Cites, &n.CreatedAt, &n.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return n, ErrNotFound
	}
	return n, err
}

// sameThread is the condition on a note's anchor n.message_id that it
// is in the thread of message $2.
const sameThread = `n.message_id IN (SELECT tm.id FROM messages tm
	WHERE tm.thread_id = (SELECT thread_id FROM messages WHERE id = $2))`

// WriteNote writes v's note on n.MessageID, or on its thread, replacing
// the text and cites of v's note there if there is one, and records a
// with the note's shape, never its text: the audit log is not a copy of
// the note. v must see the message (ErrNotFound otherwise). It reports
// whether the note is new.
//
// A thread note is anchored on the message named. v's existing note of
// that thread keeps its id and moves onto it; any other v has in the
// thread (two merged threads leave two) is deleted.
func (db *DB) WriteNote(ctx context.Context, v *auth.Viewer, n NewNote, a Action) (Note, bool, error) {
	if err := needViewer(v, "write note"); err != nil {
		return Note{}, false, err
	}
	if err := n.validate(); err != nil {
		return Note{}, false, err
	}
	var out Note
	var created bool
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		// Writers of changes serialize on contentLock (§8.6); it also
		// keeps two writes of one author's thread note apart.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, contentLock); err != nil {
			return err
		}
		message, err := db.visibleMessageID(ctx, tx, v, n.MessageID)
		if err != nil {
			return err
		}
		target := `n.message_id = $2`
		if n.Scope == ScopeThread {
			target = sameThread
		}
		// The note already on this message first, so it keeps its id.
		rows, err := tx.Query(ctx, `SELECT n.id FROM notes n WHERE n.author_id = $1 AND n.scope = $3 AND `+target+`
			ORDER BY n.message_id = $2 DESC, n.id FOR UPDATE OF n`, v.PrincipalID, message, n.Scope)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		w := writer{tx: tx}
		var id string
		if len(ids) > 0 {
			id = ids[0]
			if len(ids) > 1 {
				if _, err := tx.Exec(ctx, `DELETE FROM notes WHERE id = ANY($1)`, ids[1:]); err != nil {
					return fmt.Errorf("delete merged notes: %w", err)
				}
				for _, gone := range ids[1:] {
					w.change("note", gone)
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE notes SET message_id = $2, body = $3, cites = $4, updated_at = greatest(created_at, $5)
				WHERE id = $1`, id, message, n.Text, n.Cites, a.At); err != nil {
				return fmt.Errorf("update note: %w", err)
			}
		} else {
			id, created = ulid.New(a.At), true
			if _, err := tx.Exec(ctx, `INSERT INTO notes (id, author_id, message_id, scope, body, cites, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`, id, v.PrincipalID, message, n.Scope, n.Text, n.Cites, a.At); err != nil {
				return fmt.Errorf("insert note: %w", err)
			}
		}
		a.Args = withArg(withArg(withArg(withArg(a.Args, "note", id), "message", message), "scope", n.Scope), "cites", len(n.Cites))
		a.Result = map[string]any{"created": created, "chars": utf8.RuneCountInString(n.Text), "merged": max(len(ids)-1, 0)}
		if err := insertAction(ctx, tx, a); err != nil {
			return err
		}
		w.change("note", id)
		if err := w.flush(ctx); err != nil {
			return err
		}
		out, err = scanNote(tx.QueryRow(ctx, `SELECT `+noteCols+noteFrom+` WHERE n.id = $1`, id))
		return err
	})
	if err != nil {
		return Note{}, false, fmt.Errorf("write note on %s: %w", n.MessageID, err)
	}
	return out, created, nil
}

// DeleteNote deletes v's note on message, or for ScopeThread v's note
// of its thread, and records a. Only the author deletes a note. v must
// see the message; ErrNotFound if it cannot, or has no such note.
func (db *DB) DeleteNote(ctx context.Context, v *auth.Viewer, message, scope string, a Action) error {
	if err := needViewer(v, "delete note"); err != nil {
		return err
	}
	if scope != ScopeMessage && scope != ScopeThread {
		return fmt.Errorf("delete note: %w: scope %q: want message or thread", ErrInvalidNote, scope)
	}
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, contentLock); err != nil {
			return err
		}
		id, err := db.visibleMessageID(ctx, tx, v, message)
		if err != nil {
			return err
		}
		target := `n.message_id = $2`
		if scope == ScopeThread {
			target = sameThread
		}
		rows, err := tx.Query(ctx, `DELETE FROM notes n WHERE n.author_id = $1 AND n.scope = $3 AND `+target+` RETURNING n.id`,
			v.PrincipalID, id, scope)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return ErrNotFound
		}
		a.Args = withArg(withArg(a.Args, "message", id), "scope", scope)
		a.Result = map[string]any{"deleted": ids}
		if err := insertAction(ctx, tx, a); err != nil {
			return err
		}
		w := writer{tx: tx}
		for _, gone := range ids {
			w.change("note", gone)
		}
		return w.flush(ctx)
	})
	if err != nil {
		return fmt.Errorf("delete note on %s: %w", message, err)
	}
	return nil
}

// Notes returns the notes v may see on message id and on its thread
// (thread notes anchored on any message of it), message notes first,
// then oldest first. ErrNotFound if v cannot see the message.
func (db *DB) Notes(ctx context.Context, v *auth.Viewer, id string) ([]Note, error) {
	if err := needViewer(v, "notes"); err != nil {
		return nil, err
	}
	message, err := db.visibleMessageID(ctx, db.pool, v, id)
	if err != nil {
		return nil, fmt.Errorf("notes of %s: %w", id, err)
	}
	rows, err := db.pool.Query(ctx, `SELECT `+noteCols+noteFrom+`
		WHERE `+db.noteVisible(v)+` AND (n.scope = 'message' AND n.message_id = $2 OR n.scope = 'thread' AND `+sameThread+`)
		ORDER BY n.scope = 'thread', n.id`, v.PrincipalID, message)
	if err != nil {
		return nil, fmt.Errorf("notes of %s: %w", id, err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Note, error) { return scanNote(r) })
	if err != nil {
		return nil, fmt.Errorf("notes of %s: %w", id, err)
	}
	return out, nil
}

// NoteByID returns note id if v may see it, ErrNotFound otherwise.
func (db *DB) NoteByID(ctx context.Context, v *auth.Viewer, id string) (Note, error) {
	if err := needViewer(v, "note"); err != nil {
		return Note{}, err
	}
	n, err := scanNote(db.pool.QueryRow(ctx, `SELECT `+noteCols+noteFrom+` WHERE n.id = $2 AND `+db.noteVisible(v), v.PrincipalID, id))
	if err != nil {
		return n, fmt.Errorf("note %s: %w", id, err)
	}
	return n, nil
}
