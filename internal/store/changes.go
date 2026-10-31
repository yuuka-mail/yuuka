package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/auth"
)

// The pull side of the change log (§8.6): what changed after a cursor,
// for clients that follow changes to old mail as well as new arrivals.
// The cursor is changes.seq, which is safe to resume from because every
// row is written by writer.flush under contentLock (see there).

// ChangeKinds are the kinds of the change log (changes_kind_check), in
// the order they were introduced.
var ChangeKinds = []string{"message", "delivery", "thread", "flag", "prefix", "proposal", "draft", "outbox", "rule"}

// ErrCursorExpired: the cursor is not one this log issued, because it is
// ahead of the log (the database was restored from a backup, or it
// belongs to another installation). The client starts again from now
// and rereads what it holds. The log is not pruned; a pruner would
// answer the same for a cursor below what it kept.
var ErrCursorExpired = errors.New("cursor expired")

// changeScan bounds the rows one call looks at. A viewer that may see
// little scans past the rest in steps of this size, each call moving the
// cursor, rather than in one long statement.
const changeScan = 1000

// ChangeQuery selects changes.
type ChangeQuery struct {
	// Since is the cursor: changes after it are returned. nil starts from
	// now: no changes, and the cursor of the latest one.
	Since *int64
	// Kinds keeps these kinds only (ChangeKinds); empty keeps all. The
	// cursor moves past the others all the same, so a client that later
	// wants more kinds starts again from now.
	Kinds []string
	Limit int // default 100, at most 500
}

// Change is one entry of the log: Kind/ID changed at At. What changed
// is not in it; the reader fetches the object through its own read,
// which may by then show a later state, or none if it is gone.
type Change struct {
	Seq  int64
	Kind string
	// ID is a ULID, or prefix@zone for kind prefix. For message and flag
	// it is the message id, for delivery the delivery id.
	ID string
	// MessageID is the message of a delivery, the object a client can
	// read; "" for other kinds.
	MessageID string
	At        time.Time
}

// ChangePage is one call's worth of changes.
type ChangePage struct {
	// Changes is in log order, each object once: an object that changed
	// several times within the page appears at its last change.
	Changes []Change
	// Cursor continues after this page. It moves past the changes v may
	// not see too, so they are never scanned again; the holes it leaves
	// in seq tell that something happened, which §13.8 allows.
	Cursor int64
	// More: the log goes on past Cursor. A page may then hold fewer
	// changes than the limit, or none.
	More bool
}

// Changes returns the changes after q.Since that v may see, in the order
// they committed (§8.6). Visibility is each object's own (§13.8): a
// message, flag or thread change goes to whom the message (some message
// of the thread) is visible, a delivery's to whom the delivery is, an
// outbox row's to whom the account it is sent from is readable, drafts
// and proposals by their own rules (visibleDraft, proposalVisible),
// rules to people only; the prefix registry is listed to everyone who
// may list prefixes, so its changes are too. Of an object that is gone
// only its id is left: a thread emptied by rethreading goes to a viewer
// that reads every account, a deleted draft to one that sees every
// draft. Which kinds a credential may follow at all is a matter of
// scopes, decided by the caller.
func (db *DB) Changes(ctx context.Context, v *auth.Viewer, q ChangeQuery) (ChangePage, error) {
	if err := needViewer(v, "changes"); err != nil {
		return ChangePage{}, err
	}
	for _, k := range q.Kinds {
		if !slices.Contains(ChangeKinds, k) {
			return ChangePage{}, fmt.Errorf("changes: kind %q", k)
		}
	}
	kinds := q.Kinds
	if len(kinds) == 0 {
		kinds = ChangeKinds
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 500)

	var head int64
	if err := db.pool.QueryRow(ctx, `SELECT coalesce(max(seq), 0) FROM changes`).Scan(&head); err != nil {
		return ChangePage{}, fmt.Errorf("changes: %w", err)
	}
	if q.Since == nil {
		return ChangePage{Changes: []Change{}, Cursor: head}, nil
	}
	since := *q.Since
	if since < 0 || since > head {
		return ChangePage{}, fmt.Errorf("changes after %d: %w", since, ErrCursorExpired)
	}

	// One statement, one snapshot: every seq below one it sees has
	// committed (flush), so nothing can appear behind the cursor later.
	rows, err := db.pool.Query(ctx, `
		SELECT c.seq, c.kind, c.target_id, c.at, c.kind = ANY($2) AND `+db.changeVisible(v)+`,
			CASE WHEN c.kind = 'delivery' THEN (SELECT coalesce(d.message_id, '') FROM deliveries d WHERE d.id = c.target_id) END
		FROM changes c
		WHERE c.seq > $3
		ORDER BY c.seq
		LIMIT $4`, v.PrincipalID, kinds, since, changeScan)
	if err != nil {
		return ChangePage{}, fmt.Errorf("changes: %w", err)
	}
	page := ChangePage{Cursor: since}
	var shown []Change
	var c Change
	var visible bool
	var message *string
	scanned := 0
	_, err = pgx.ForEachRow(rows, []any{&c.Seq, &c.Kind, &c.ID, &c.At, &visible, &message}, func() error {
		scanned++
		if page.More {
			return nil
		}
		if visible {
			if len(shown) == limit {
				page.More = true
				return nil
			}
			c.MessageID = ""
			if message != nil {
				c.MessageID = *message
			}
			shown = append(shown, c)
		}
		page.Cursor = c.Seq
		return nil
	})
	if err != nil {
		return ChangePage{}, fmt.Errorf("changes: %w", err)
	}
	page.More = page.More || scanned == changeScan
	page.Changes = lastOfEach(shown)
	return page, nil
}

// changeVisible is the condition on a change c that v may see it, by
// its kind; see Changes. It composes the visibility of each object,
// decided where that object is read, and adds none of its own. $1 is
// v's principal id (visibleDraft).
func (db *DB) changeVisible(v *auth.Viewer) string {
	message := `EXISTS (SELECT 1 FROM messages m WHERE m.id = c.target_id AND ` + db.visibleMessage(v, "m") + `)`
	draftGone := "false"
	if seesEveryDraft(v) {
		draftGone = `NOT EXISTS (SELECT 1 FROM drafts dr WHERE dr.id = c.target_id)`
	}
	person := "false"
	if isPerson(v) {
		person = "true"
	}
	return `CASE c.kind
		WHEN 'message' THEN ` + message + `
		WHEN 'flag' THEN ` + message + `
		WHEN 'delivery' THEN EXISTS (SELECT 1 FROM deliveries d WHERE d.id = c.target_id AND ` + db.visibleDelivery(v, "d") + `)
		WHEN 'thread' THEN EXISTS (SELECT 1 FROM messages m WHERE m.thread_id = c.target_id AND ` + db.visibleMessage(v, "m") + `)
			OR NOT EXISTS (SELECT 1 FROM threads t WHERE t.id = c.target_id) AND ` + db.readsEveryAccount(v) + `
		WHEN 'prefix' THEN true
		WHEN 'proposal' THEN EXISTS (SELECT 1 FROM proposals pr WHERE pr.id = c.target_id AND ` + db.proposalVisible(v) + `)
		WHEN 'draft' THEN EXISTS (SELECT 1 FROM drafts dr WHERE dr.id = c.target_id AND ` + visibleDraft(v) + `)
			OR ` + draftGone + `
		WHEN 'outbox' THEN EXISTS (SELECT 1 FROM outbox o JOIN identities i ON i.id = o.identity_id
			WHERE o.id = c.target_id AND ` + db.readableAccount(v, "i.account_id") + `)
		WHEN 'rule' THEN ` + person + `
		ELSE false END`
}

// lastOfEach keeps each (kind, id) once, at its last position: a reader
// fetches the object once and sees its latest state either way.
func lastOfEach(cs []Change) []Change {
	type key struct{ kind, id string }
	seen := make(map[key]bool, len(cs))
	out := make([]Change, 0, len(cs))
	for i := len(cs) - 1; i >= 0; i-- {
		k := key{cs[i].Kind, cs[i].ID}
		if !seen[k] {
			seen[k] = true
			out = append(out, cs[i])
		}
	}
	slices.Reverse(out)
	return out
}
