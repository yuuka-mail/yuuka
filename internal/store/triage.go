package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/triage/score"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Triage with decision models (§15, migration 0013). A scorer asks a
// model closed questions about each message, and the answers are kept as
// observed facts in triage_scores: one row per scoring run, written by
// InsertScore and nothing else, never updated or deleted, never touched
// by reparse. v1 observes only (§15.4): scores are read and shown, and
// nothing in the store acts on them. The §4.3 downgrade, the push, the
// rules and the send policy do not read this table.

// ErrInvalidScore: a score the store will not keep, or a profile that
// cannot name one.
var ErrInvalidScore = errors.New("invalid score")

var (
	scorerName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,31}$`)
	answerKey  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	sha256Hex  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func validProfile(p score.Profile) error {
	if !scorerName.MatchString(p.Scorer) {
		return fmt.Errorf("%w: scorer %q: lowercase letters, digits, dots and hyphens, at most 32", ErrInvalidScore, p.Scorer)
	}
	if p.Questions <= 0 {
		return fmt.Errorf("%w: question set version %d", ErrInvalidScore, p.Questions)
	}
	return nil
}

func validScore(s score.Score) error {
	if !ulid.Valid(s.ID) || !ulid.Valid(s.MessageID) {
		return fmt.Errorf("%w: id %q, message %q: want ULIDs", ErrInvalidScore, s.ID, s.MessageID)
	}
	if err := validProfile(s.Profile); err != nil {
		return err
	}
	if s.Model == "" || utf8.RuneCountInString(s.Model) > 200 || !utf8.ValidString(s.Model) || strings.ContainsRune(s.Model, 0) {
		return fmt.Errorf("%w: model: 1 to 200 characters", ErrInvalidScore)
	}
	if s.ScoredAt.IsZero() {
		return fmt.Errorf("%w: no scored_at", ErrInvalidScore)
	}
	if len(s.Answers) == 0 {
		return fmt.Errorf("%w: no answers", ErrInvalidScore)
	}
	for q, a := range s.Answers {
		if !answerKey.MatchString(q) {
			return fmt.Errorf("%w: question %q", ErrInvalidScore, q)
		}
		if a.Abstain == (len(a.P) > 0) {
			return fmt.Errorf("%w: %s: probabilities or an abstention, one of them", ErrInvalidScore, q)
		}
		for o, p := range a.P {
			if !answerKey.MatchString(o) {
				return fmt.Errorf("%w: %s: option %q", ErrInvalidScore, q, o)
			}
			if math.IsNaN(p) || p < 0 || p > 1 {
				return fmt.Errorf("%w: %s.%s: probability %v", ErrInvalidScore, q, o, p)
			}
		}
	}
	in := s.Input
	switch {
	case in.Endpoint != score.EndpointLocal && in.Endpoint != score.EndpointHosted:
		return fmt.Errorf("%w: input endpoint %q: local or hosted", ErrInvalidScore, in.Endpoint)
	case in.View < 0 || in.Parser < 0 || in.Chars < 0:
		return fmt.Errorf("%w: input versions and length cannot be negative", ErrInvalidScore)
	case !sha256Hex.MatchString(in.SHA256):
		return fmt.Errorf("%w: input sha256: 64 lowercase hex digits", ErrInvalidScore)
	case len(in.Facts.Auth) > 300 || !utf8.ValidString(in.Facts.Auth) || strings.ContainsRune(in.Facts.Auth, 0):
		return fmt.Errorf("%w: input auth line: at most 300 bytes of text", ErrInvalidScore)
	case !slices.Contains([]string{"unknown", "machine", "human"}, in.Facts.Class):
		return fmt.Errorf("%w: input class %q", ErrInvalidScore, in.Facts.Class)
	}
	return nil
}

// InsertScore stores one scoring run (§15.2) and logs a change of kind
// triage for its message in the same transaction. It is the only writer
// of triage_scores (TestOneWayIntoScores): scores are appended, never
// updated or deleted, and reparse leaves them alone. Rescoring is
// another call with a new id. ErrNotFound if the message does not exist.
func (db *DB) InsertScore(ctx context.Context, s score.Score) error {
	if err := validScore(s); err != nil {
		return fmt.Errorf("insert score: %w", err)
	}
	answers, err := json.Marshal(s.Answers)
	if err != nil {
		return fmt.Errorf("insert score: %w", err)
	}
	input, err := json.Marshal(s.Input)
	if err != nil {
		return fmt.Errorf("insert score: %w", err)
	}
	err = pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO triage_scores (id, message_id, scorer, model, questions, answers, input, scored_at)
			SELECT $1, m.id, $3, $4, $5, $6, $7, $8 FROM messages m WHERE m.id = $2`,
			s.ID, s.MessageID, s.Scorer, s.Model, s.Questions, answers, input, s.ScoredAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		w := writer{tx: tx}
		w.change("triage", s.MessageID)
		return w.flush(ctx)
	})
	if err != nil {
		return fmt.Errorf("insert score %s of %s: %w", s.ID, s.MessageID, err)
	}
	return nil
}

// scoreTarget is "message m is one a scorer may be given" (§15.6,
// §10.1.4): a delivery of it escapes the §4.3 downgrade (what §4.3
// hides already, and mail we only sent, are not worth asking a model
// about) in an account the host lets be scored. accounts is the SQL of
// a text[] of account names, YUUKA_TRIAGE_ACCOUNTS; NULL is its
// default, the Cloudflare accounts. Scoring hands the content to the
// scorer, which may be a third party's, so whose mail that may be is
// the host's decision (§17.1): a parameter, never a row a session could
// write. and narrows the deliveries further (a viewer's), "" for none.
func scoreTarget(m, accounts, and string) string {
	if and != "" {
		and = " AND " + and
	}
	return `EXISTS (SELECT 1 FROM deliveries d JOIN prefixes p ON p.zone = d.zone AND p.prefix = d.prefix
		JOIN accounts sa ON sa.id = d.account_id
		WHERE d.message_id = ` + m + `.id AND ` + notDowngraded + and + `
			AND CASE WHEN ` + accounts + `::text[] IS NULL THEN sa.kind = '` + AccountCloudflare + `'
				ELSE sa.name = ANY (` + accounts + `::text[]) END)`
}

// ScoreTargets returns up to limit messages received since then that p
// has not scored, newest first: those scoreTarget admits for accounts
// (nil: the default). The scorer works for the system, so visibility
// does not narrow it; the accounts do. A message whose scoring fails
// stays a target; backing off is the caller's.
func (db *DB) ScoreTargets(ctx context.Context, p score.Profile, accounts []string, since time.Time, limit int) ([]string, error) {
	if err := validProfile(p); err != nil {
		return nil, fmt.Errorf("score targets: %w", err)
	}
	rows, err := db.pool.Query(ctx, `
		SELECT m.id FROM messages m
		WHERE m.received_at >= $1
			AND `+scoreTarget("m", "$5", "")+`
			AND NOT EXISTS (SELECT 1 FROM triage_scores ts WHERE ts.message_id = m.id AND ts.scorer = $2 AND ts.questions = $3)
		ORDER BY m.received_at DESC, m.id DESC
		LIMIT $4`, since, p.Scorer, p.Questions, limit, accounts)
	if err != nil {
		return nil, fmt.Errorf("score targets: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("score targets: %w", err)
	}
	return ids, nil
}

// IsScoreTarget reports whether message id is one scoreTarget admits for
// accounts: what the scorer checks of each message right before it
// builds its input, however the message was queued.
func (db *DB) IsScoreTarget(ctx context.Context, id string, accounts []string) (bool, error) {
	var ok bool
	err := db.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM messages m WHERE m.id = $1 AND `+scoreTarget("m", "$2", "")+`)`,
		id, accounts).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("score target %s: %w", id, err)
	}
	return ok, nil
}

// ScoreRead is a score as one viewer reads it.
type ScoreRead struct {
	score.Score
	// InputWithheld: the message has deliveries v cannot see, whose
	// facts (authentication, prefix state) the input may carry, so Input
	// is left zero (§13.8).
	InputWithheld bool
}

// LatestScore returns the latest score of message id under profile p as
// v sees it, or nil if there is none. A score is information about the
// content: v reads it when v reads the message (§13.8; ErrNotFound
// otherwise), its input only when v sees every delivery of the message.
// An id merged away resolves to its survivor, as in MessageByID.
func (db *DB) LatestScore(ctx context.Context, v *auth.Viewer, id string, p score.Profile) (*ScoreRead, error) {
	if err := needViewer(v, "score"); err != nil {
		return nil, err
	}
	if err := validProfile(p); err != nil {
		return nil, fmt.Errorf("score of %s: %w", id, err)
	}
	var out *ScoreRead
	err := pgx.BeginTxFunc(ctx, db.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		mid, err := db.visibleMessageID(ctx, tx, v, id)
		if err != nil {
			return err
		}
		var r ScoreRead
		var answers, input []byte
		err = tx.QueryRow(ctx, `
			SELECT ts.id, ts.message_id, ts.scorer, ts.questions, ts.model, ts.answers, ts.input, ts.scored_at,
				EXISTS (SELECT 1 FROM deliveries d WHERE d.message_id = ts.message_id AND NOT `+db.visibleDelivery(v, "d")+`)
			FROM triage_scores ts
			WHERE ts.message_id = $1 AND ts.scorer = $2 AND ts.questions = $3
			ORDER BY ts.scored_at DESC, ts.id DESC LIMIT 1`, mid, p.Scorer, p.Questions).
			Scan(&r.ID, &r.MessageID, &r.Scorer, &r.Questions, &r.Model, &answers, &input, &r.ScoredAt, &r.InputWithheld)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := json.Unmarshal(answers, &r.Answers); err != nil {
			return err
		}
		if !r.InputWithheld {
			if err := json.Unmarshal(input, &r.Input); err != nil {
				return err
			}
		}
		out = &r
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("score of %s: %w", id, err)
	}
	return out, nil
}
