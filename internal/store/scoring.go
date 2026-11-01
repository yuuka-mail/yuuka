package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/triage/score"
)

// What the scorer (internal/triage) reads besides triage.go: one fact
// of its input, the messages to score again, and the counts of its
// report. None of them reads mail content.

// fromDomain is the domain of a message's From address, aliased m.
func fromDomain(m string) string {
	return `lower(substring(` + m + `.from_addr from '@([^@]+)$'))`
}

// KnownSender reports whether the From domain of message id wrote to
// the same prefix before and escaped the §4.3 downgrade there, as v
// sees both: the "first mail?" fact of the scorer's input (§15.2). The
// domain, not the address: a service writes from many addresses of
// one domain. Mail that was itself downgraded does not count, or a
// spammer would become known by writing twice; and only v's visible
// deliveries do, or v could learn of mail in accounts it cannot read.
// ErrNotFound if v cannot see id.
func (db *DB) KnownSender(ctx context.Context, v *auth.Viewer, id string) (bool, error) {
	if err := needViewer(v, "known sender"); err != nil {
		return false, err
	}
	var known bool
	err := db.pool.QueryRow(ctx, `
		SELECT `+fromDomain("m")+` IS NOT NULL AND EXISTS (
			SELECT 1 FROM deliveries md
			JOIN deliveries d ON d.zone = md.zone AND d.prefix = md.prefix AND d.message_id <> md.message_id
			JOIN messages o ON o.id = d.message_id
			JOIN prefixes p ON p.zone = d.zone AND p.prefix = d.prefix
			WHERE md.message_id = m.id AND d.received_at < md.received_at
			  AND `+fromDomain("o")+` = `+fromDomain("m")+`
			  AND `+db.visibleDelivery(v, "md")+` AND `+db.visibleDelivery(v, "d")+` AND `+notDowngraded+`)
		FROM messages m
		WHERE m.id = coalesce((SELECT new_id FROM message_aliases WHERE old_id = $1), $1)
		  AND `+db.visibleMessage(v, "m"), id).Scan(&known)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("message %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return false, fmt.Errorf("known sender %s: %w", id, err)
	}
	return known, nil
}

// RescoreTargets returns up to limit messages received since then that
// a scorer may score, scored already or not, with ids after after, in
// id order: what `yuuka triage rescore` pages through. The targets are
// ScoreTargets', for the same accounts.
func (db *DB) RescoreTargets(ctx context.Context, accounts []string, since time.Time, after string, limit int) ([]string, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT m.id FROM messages m
		WHERE m.received_at >= $1 AND m.id > $2
			AND `+scoreTarget("m", "$4", "")+`
		ORDER BY m.id
		LIMIT $3`, since, after, limit, accounts)
	if err != nil {
		return nil, fmt.Errorf("rescore targets: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("rescore targets: %w", err)
	}
	return ids, nil
}

// ScoreCounts is how the latest scores of a profile answer, over the
// messages a viewer sees.
type ScoreCounts struct {
	// Scored messages have a score of the profile; Unscored are targets
	// (ScoreTargets) without one.
	Scored, Unscored int
	// Answers count, by question, the option each message's latest
	// score gives the highest probability (ties to the first by name),
	// under "" when it abstained.
	Answers map[string]map[string]int
}

// ScoreCounts counts the latest scores of profile p on the messages v
// sees that were received since then: the report of `yuuka triage
// report`. Unscored counts the targets of accounts (ScoreTargets) whose
// qualifying delivery v sees. Counts only, never a message or its
// content; a hidden message counts nowhere.
func (db *DB) ScoreCounts(ctx context.Context, v *auth.Viewer, p score.Profile, accounts []string, since time.Time) (ScoreCounts, error) {
	if err := needViewer(v, "score counts"); err != nil {
		return ScoreCounts{}, err
	}
	if err := validProfile(p); err != nil {
		return ScoreCounts{}, fmt.Errorf("score counts: %w", err)
	}
	out := ScoreCounts{Answers: map[string]map[string]int{}}
	err := pgx.BeginTxFunc(ctx, db.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		latest := `
			SELECT DISTINCT ON (ts.message_id) ts.message_id, ts.answers
			FROM triage_scores ts JOIN messages m ON m.id = ts.message_id
			WHERE ts.scorer = $1 AND ts.questions = $2 AND m.received_at >= $3 AND ` + db.visibleMessage(v, "m") + `
			ORDER BY ts.message_id, ts.scored_at DESC, ts.id DESC`
		err := tx.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM (`+latest+`) l),
			       (SELECT count(*) FROM messages m
			        WHERE m.received_at >= $3
			          AND `+scoreTarget("m", "$4", db.visibleDelivery(v, "d"))+`
			          AND NOT EXISTS (SELECT 1 FROM triage_scores ts
			                          WHERE ts.message_id = m.id AND ts.scorer = $1 AND ts.questions = $2))`,
			p.Scorer, p.Questions, since, accounts).Scan(&out.Scored, &out.Unscored)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT q.key, `+topOption("q.value")+`, count(*)
			FROM (`+latest+`) l CROSS JOIN LATERAL jsonb_each(l.answers) q
			GROUP BY 1, 2`, p.Scorer, p.Questions, since)
		if err != nil {
			return err
		}
		var q string
		var top *string
		var n int
		_, err = pgx.ForEachRow(rows, []any{&q, &top, &n}, func() error {
			if out.Answers[q] == nil {
				out.Answers[q] = map[string]int{}
			}
			opt := ""
			if top != nil {
				opt = *top
			}
			out.Answers[q][opt] += n
			return nil
		})
		return err
	})
	if err != nil {
		return ScoreCounts{}, fmt.Errorf("score counts: %w", err)
	}
	return out, nil
}

// topOption is the option the answer a (a jsonb score.Answer) gives the
// highest probability, ties to the first by name, as score.Answer.Top
// picks it; NULL when it abstained or there is no answer. No threshold:
// it is how the report counts, what lists show and what triage= finds.
func topOption(a string) string {
	return `(CASE WHEN coalesce(((` + a + `)->>'abstain')::boolean, false) THEN NULL
		ELSE (SELECT o.key FROM jsonb_each((` + a + `)->'p') o ORDER BY (o.value)::float8 DESC, o.key LIMIT 1) END)`
}

// AnswerFilter keeps the messages whose latest score of Profile gives
// Option the highest probability for Question (topOption): what the
// model leaned to, with no threshold. A message without a score of the
// profile, or whose model abstained, matches no filter.
type AnswerFilter struct {
	score.Profile
	Question, Option string
}

func (f *AnswerFilter) valid() error {
	if err := validProfile(f.Profile); err != nil {
		return err
	}
	if !answerKey.MatchString(f.Question) || !answerKey.MatchString(f.Option) {
		return fmt.Errorf("%w: filter %s:%s", ErrInvalidScore, f.Question, f.Option)
	}
	return nil
}

// condition is f on the message aliased m. The latest score is picked
// by the triage_scores_latest index.
func (f *AnswerFilter) condition(arg func(any) string) string {
	q := arg(f.Question) + `::text`
	return `(SELECT ` + topOption("ts.answers -> "+q) + ` FROM triage_scores ts
		WHERE ts.message_id = m.id AND ts.scorer = ` + arg(f.Scorer) + ` AND ts.questions = ` + arg(f.Questions) + `
		ORDER BY ts.scored_at DESC, ts.id DESC LIMIT 1) = ` + arg(f.Option)
}

// TopAnswers returns, for each of ids v sees that profile p scored,
// what its latest score leans to: by question, the option with the
// highest probability (topOption), abstentions left out. It is the
// compact form a list shows beside each message, one query for a page.
// Ids v cannot see, or that p never scored, are absent.
func (db *DB) TopAnswers(ctx context.Context, v *auth.Viewer, p score.Profile, ids []string) (map[string]map[string]string, error) {
	if err := needViewer(v, "top answers"); err != nil {
		return nil, err
	}
	if err := validProfile(p); err != nil {
		return nil, fmt.Errorf("top answers: %w", err)
	}
	out := map[string]map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := db.pool.Query(ctx, `
		SELECT l.message_id, q.key, t.top
		FROM (SELECT DISTINCT ON (ts.message_id) ts.message_id, ts.answers
		      FROM triage_scores ts JOIN messages m ON m.id = ts.message_id
		      WHERE ts.message_id = ANY($1) AND ts.scorer = $2 AND ts.questions = $3 AND `+db.visibleMessage(v, "m")+`
		      ORDER BY ts.message_id, ts.scored_at DESC, ts.id DESC) l
		CROSS JOIN LATERAL jsonb_each(l.answers) q
		CROSS JOIN LATERAL (SELECT `+topOption("q.value")+` AS top) t
		WHERE t.top IS NOT NULL`, ids, p.Scorer, p.Questions)
	if err != nil {
		return nil, fmt.Errorf("top answers: %w", err)
	}
	var id, q, top string
	_, err = pgx.ForEachRow(rows, []any{&id, &q, &top}, func() error {
		if out[id] == nil {
			out[id] = map[string]string{}
		}
		out[id][q] = top
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("top answers: %w", err)
	}
	return out, nil
}
