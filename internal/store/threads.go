package store

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/classify"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Threads are the connected components of a graph over messages (§9.1,
// threads belong to no account). A message is known by its Message-ID
// and, for our own sent mail, by the ids its send attempts recorded
// (send_attempts.message_id and provider_id, §10.4.1: a platform that
// assigns Message-IDs writes none into the sent copy). Two messages are
// linked when
//
//   - one's In-Reply-To or References names an id the other is known by,
//     or both are known by the same id (content that did not fold, §4.1);
//   - or, only for class=human mail without any reply headers, they share
//     sender and normalized subject within fallbackWindow (§4.1: machine
//     mail would collapse every "Your verification code" into one thread).
//
// Ingest adds one message at a time and merges the components it links;
// reparse recomputes all components from scratch (Rethread). Both apply
// the same rules, so a reparse of unchanged content keeps every thread.

// fallbackWindow bounds the subject+sender fallback, so a "hello" from the
// same person a year later starts a new conversation.
const fallbackWindow = 30 * 24 * time.Hour

// replyPrefix matches one reply/forward marker, in the languages mail
// clients actually emit, optionally with a counter like "Re[2]:".
var replyPrefix = regexp.MustCompile(`(?i)^(re|fwd?|aw|wg|sv|vs|回复|答复|转发)\s*(\[\d+\])?\s*[:：]\s*`)

// normalizeSubject strips reply/forward markers, folds case and spacing.
func normalizeSubject(s string) string {
	s = strings.TrimSpace(s)
	for {
		loc := replyPrefix.FindStringIndex(s)
		if loc == nil {
			break
		}
		s = strings.TrimSpace(s[loc[1]:])
	}
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func fallbackEligible(msg mimeparse.Message, class classify.Class) bool {
	return class == classify.Human && msg.InReplyTo == "" && len(msg.References) == 0 &&
		msg.FromAddr != "" && normalizeSubject(msg.Subject) != ""
}

// placeThread picks the thread for a new message: the one it links to,
// merging several if it bridges them, or a fresh one. h is its content
// hash, by which its sent copies and their attempts are found.
func (w *writer) placeThread(ctx context.Context, id string, at time.Time, h []byte, msg mimeparse.Message, class classify.Class) (string, error) {
	own, err := w.sentIDs(ctx, h)
	if err != nil {
		return "", err
	}
	if msg.MessageID != "" {
		own = append(own, msg.MessageID)
	}
	names := slices.Clone(msg.References)
	if msg.InReplyTo != "" {
		names = append(names, msg.InReplyTo)
	}
	names = append(names, own...)
	// The other message m is known by m.message_id or by its sent copies'
	// attempt ids; the last clause finds m by the attempts that name it.
	rows, err := w.tx.Query(ctx, `
		SELECT DISTINCT thread_id FROM messages m
		WHERE m.id <> $1 AND (m.message_id = ANY($2::text[])
			OR m.in_reply_to = ANY($3::text[]) OR m.refs && $3::text[]
			OR m.content_hash IN (
				SELECT sr.content_hash FROM send_attempts a
				JOIN outbox o ON o.id = a.outbox_id JOIN raw_objects sr ON sr.id = o.raw_id
				WHERE a.message_id = ANY($2::text[]) OR a.provider_id = ANY($2::text[])))
		ORDER BY thread_id`, id, names, own)
	if err != nil {
		return "", fmt.Errorf("thread links: %w", err)
	}
	threads, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", fmt.Errorf("thread links: %w", err)
	}
	if len(threads) == 0 && fallbackEligible(msg, class) {
		t, err := w.fallbackThread(ctx, id, at, msg)
		if err != nil {
			return "", err
		}
		if t != "" {
			threads = []string{t}
		}
	}
	if len(threads) == 0 {
		t := ulid.New(at)
		if err := w.insertThread(ctx, t, at); err != nil {
			return "", err
		}
		return t, nil
	}
	// Merge into the oldest thread; the rest disappear.
	survivor, rest := threads[0], threads[1:]
	if len(rest) > 0 {
		if _, err := w.tx.Exec(ctx, `UPDATE messages SET thread_id = $1 WHERE thread_id = ANY($2)`, survivor, rest); err != nil {
			return "", fmt.Errorf("merge threads: %w", err)
		}
		for _, t := range rest {
			if err := w.refreshThread(ctx, t); err != nil {
				return "", err
			}
		}
	}
	return survivor, nil
}

func (w *writer) fallbackThread(ctx context.Context, id string, at time.Time, msg mimeparse.Message) (string, error) {
	rows, err := w.tx.Query(ctx, `
		SELECT thread_id, subject FROM messages
		WHERE id <> $1 AND from_addr = $2 AND class <> $3
			AND received_at <= $4 AND received_at >= $5
		ORDER BY received_at DESC, id DESC
		LIMIT 200`, id, msg.FromAddr, int16(classify.Machine), at, at.Add(-fallbackWindow))
	if err != nil {
		return "", fmt.Errorf("thread fallback: %w", err)
	}
	want := normalizeSubject(msg.Subject)
	var thread, subject string
	found := ""
	_, err = pgx.ForEachRow(rows, []any{&thread, &subject}, func() error {
		if found == "" && normalizeSubject(subject) == want {
			found = thread
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("thread fallback: %w", err)
	}
	return found, nil
}

// insertThread creates a placeholder row; refreshThread fills it once the
// message referencing it exists.
func (w *writer) insertThread(ctx context.Context, id string, at time.Time) error {
	_, err := w.tx.Exec(ctx, `
		INSERT INTO threads (id, subject_norm, participants, first_at, last_at, n, class)
		VALUES ($1, '', '{}', $2, $2, 1, 0)`, id, at)
	if err != nil {
		return fmt.Errorf("insert thread: %w", err)
	}
	w.change("thread", id)
	return nil
}

func (w *writer) refreshThreadOf(ctx context.Context, messageID string) error {
	var t string
	if err := w.tx.QueryRow(ctx, `SELECT thread_id FROM messages WHERE id = $1`, messageID).Scan(&t); err != nil {
		return fmt.Errorf("thread of %s: %w", messageID, err)
	}
	return w.refreshThread(ctx, t)
}

// refreshThread recomputes the stored aggregates of thread id, deleting
// it when no message is left in it.
func (w *writer) refreshThread(ctx context.Context, id string) error {
	rows, err := w.tx.Query(ctx, `
		SELECT subject, from_addr, class, received_at FROM messages
		WHERE thread_id = $1 ORDER BY received_at, id`, id)
	if err != nil {
		return fmt.Errorf("thread %s: %w", id, err)
	}
	ms, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (threadMsg, error) {
		var m threadMsg
		err := r.Scan(&m.subject, &m.from, &m.class, &m.at)
		return m, err
	})
	if err != nil {
		return fmt.Errorf("thread %s: %w", id, err)
	}
	if len(ms) == 0 {
		if _, err := w.tx.Exec(ctx, `DELETE FROM threads WHERE id = $1`, id); err != nil {
			return fmt.Errorf("delete thread %s: %w", id, err)
		}
		w.change("thread", id)
		return nil
	}
	return w.writeThread(ctx, id, aggregate(ms))
}

func (w *writer) writeThread(ctx context.Context, id string, a threadAgg) error {
	tag, err := w.tx.Exec(ctx, `
		UPDATE threads SET subject_norm = $2, participants = $3, first_at = $4, last_at = $5, n = $6, class = $7
		WHERE id = $1 AND (subject_norm, participants, first_at, last_at, n, class)
			IS DISTINCT FROM ($2, $3::text[], $4::timestamptz, $5::timestamptz, $6::integer, $7::smallint)`,
		id, a.subjectNorm, a.participants, a.first, a.last, a.n, a.class)
	if err != nil {
		return fmt.Errorf("update thread %s: %w", id, err)
	}
	if tag.RowsAffected() > 0 {
		w.change("thread", id)
	}
	return nil
}

// threadMsg is the slice of a message that threading needs.
type threadMsg struct {
	id, messageID, inReplyTo string
	refs                     []string
	sentIDs                  []string // its send attempts' ids, as sentIDs
	subject, from, threadID  string
	class                    int16
	at                       time.Time
}

type threadAgg struct {
	subjectNorm  string
	participants []string
	first, last  time.Time
	n            int
	class        int16
}

// aggregate computes the stored thread columns from its messages, which
// must be in (received_at, id) order.
func aggregate(ms []threadMsg) threadAgg {
	a := threadAgg{
		subjectNorm:  normalizeSubject(ms[0].subject),
		participants: []string{},
		first:        ms[0].at,
		last:         ms[len(ms)-1].at,
		n:            len(ms),
		class:        int16(classify.Unknown),
	}
	for _, m := range ms {
		if m.from != "" && !slices.Contains(a.participants, m.from) {
			a.participants = append(a.participants, m.from)
		}
		switch {
		case m.class == int16(classify.Human):
			a.class = m.class
		case m.class == int16(classify.Machine) && a.class == int16(classify.Unknown):
			a.class = m.class
		}
	}
	return a
}

// RethreadStats counts what Rethread did.
type RethreadStats struct {
	Threads int // threads after the rebuild
	Moved   int // messages whose thread changed
}

// Rethread rebuilds all threads from the messages table: the threads part
// of reparse (§4.2). Existing thread ids are reused where a component
// still contains messages of that thread, so an unchanged mailbox keeps
// its thread ids.
func (db *DB) Rethread(ctx context.Context) (RethreadStats, error) {
	var st RethreadStats
	err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, contentLock); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id, coalesce(message_id, ''), coalesce(in_reply_to, ''), refs, subject, from_addr, class, received_at, thread_id
			FROM messages ORDER BY received_at, id`)
		if err != nil {
			return fmt.Errorf("load messages: %w", err)
		}
		ms, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (threadMsg, error) {
			var m threadMsg
			err := r.Scan(&m.id, &m.messageID, &m.inReplyTo, &m.refs, &m.subject, &m.from, &m.class, &m.at, &m.threadID)
			return m, err
		})
		if err != nil {
			return fmt.Errorf("load messages: %w", err)
		}
		// The attempt ids of every message with sent copies, the same
		// ids placeThread reads through sentIDs.
		rows, err = tx.Query(ctx, `
			SELECT DISTINCT m.id, x
			FROM send_attempts a JOIN outbox o ON o.id = a.outbox_id
			JOIN raw_objects sr ON sr.id = o.raw_id
			JOIN messages m ON m.content_hash = sr.content_hash,
			unnest(ARRAY[a.message_id, a.provider_id]) x
			WHERE x IS NOT NULL AND x <> ''
			ORDER BY m.id, x`)
		if err != nil {
			return fmt.Errorf("load sent ids: %w", err)
		}
		index := make(map[string]int, len(ms))
		for i, m := range ms {
			index[m.id] = i
		}
		var mid, x string
		_, err = pgx.ForEachRow(rows, []any{&mid, &x}, func() error {
			i := index[mid]
			ms[i].sentIDs = append(ms[i].sentIDs, x)
			return nil
		})
		if err != nil {
			return fmt.Errorf("load sent ids: %w", err)
		}
		comps := components(ms)
		w := writer{tx: tx}
		used := map[string]bool{}
		var batch pgx.Batch
		for _, c := range comps {
			id := pickThreadID(ms, c, used)
			if id == "" {
				id = ulid.New(ms[c[0]].at)
				if err := w.insertThread(ctx, id, ms[c[0]].at); err != nil {
					return err
				}
			}
			used[id] = true
			for _, i := range c {
				if ms[i].threadID != id {
					batch.Queue(`UPDATE messages SET thread_id = $2 WHERE id = $1`, ms[i].id, id)
					ms[i].threadID = id
					st.Moved++
				}
			}
		}
		if batch.Len() > 0 {
			if err := tx.SendBatch(ctx, &batch).Close(); err != nil {
				return fmt.Errorf("move messages: %w", err)
			}
		}
		for _, c := range comps {
			group := make([]threadMsg, len(c))
			for j, i := range c {
				group[j] = ms[i]
			}
			if err := w.writeThread(ctx, group[0].threadID, aggregate(group)); err != nil {
				return err
			}
		}
		ids := make([]string, 0, len(used))
		for id := range used {
			ids = append(ids, id)
		}
		rows, err = tx.Query(ctx, `DELETE FROM threads WHERE id <> ALL($1) RETURNING id`, ids)
		if err != nil {
			return fmt.Errorf("delete empty threads: %w", err)
		}
		gone, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("delete empty threads: %w", err)
		}
		slices.Sort(gone)
		for _, id := range gone {
			w.change("thread", id)
		}
		st.Threads = len(comps)
		return w.flush(ctx)
	})
	if err != nil {
		return RethreadStats{}, fmt.Errorf("rethread: %w", err)
	}
	return st, nil
}

// components groups ms (in received order) into threads. Each component
// lists message indexes in received order; components are ordered by
// their first message.
func components(ms []threadMsg) [][]int {
	parent := make([]int, len(ms))
	for i := range parent {
		parent[i] = i
	}
	find := func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra < rb {
			parent[rb] = ra
		} else if rb < ra {
			parent[ra] = rb
		}
	}

	byMsgID := map[string]int{}
	for i, m := range ms {
		for _, id := range append([]string{m.messageID}, m.sentIDs...) {
			if id == "" {
				continue
			}
			if j, ok := byMsgID[id]; ok {
				union(i, j)
			} else {
				byMsgID[id] = i
			}
		}
	}
	type key struct{ from, subject string }
	lastBy := map[key]int{}
	for i, m := range ms {
		for _, r := range append(slices.Clone(m.refs), m.inReplyTo) {
			if j, ok := byMsgID[r]; ok && r != "" {
				union(i, j)
			}
		}
		k := key{m.from, normalizeSubject(m.subject)}
		eligible := fallbackEligible(mimeparse.Message{
			InReplyTo: m.inReplyTo, References: m.refs, FromAddr: m.from, Subject: m.subject,
		}, classify.Class(m.class))
		if j, ok := lastBy[k]; ok && eligible && m.at.Sub(ms[j].at) <= fallbackWindow {
			union(i, j)
		}
		if m.class != int16(classify.Machine) && k.from != "" && k.subject != "" {
			lastBy[k] = i
		}
	}

	groups := map[int][]int{}
	var roots []int
	for i := range ms {
		r := find(i)
		if _, ok := groups[r]; !ok {
			roots = append(roots, r)
		}
		groups[r] = append(groups[r], i)
	}
	out := make([][]int, len(roots))
	for i, r := range roots {
		out[i] = groups[r]
	}
	return out
}

// pickThreadID reuses the smallest current thread id among the members of
// component c that no earlier component took.
func pickThreadID(ms []threadMsg, c []int, used map[string]bool) string {
	best := ""
	for _, i := range c {
		t := ms[i].threadID
		if !used[t] && (best == "" || t < best) {
			best = t
		}
	}
	return best
}
