-- M3 triage with decision models (§15): the scores a scorer gives each
-- message, and the change kind that tells clients a score arrived. v1
-- observes only (§15.4): a score is shown, to people and in the agent
-- view, and acts on nothing, so nothing here touches message_state,
-- annotation_log or the §4.3 downgrade. The store writes and reads
-- scores in internal/store/triage.go; the shared types are
-- internal/triage/score.
--
-- §4.2 category: an observed fact, what one scorer said about one
-- message at one moment, like deliveries.spf_recheck. Not derived (the
-- same mail scored by another model, or later, scores differently) and
-- not anyone's decision. It cannot be rebuilt from raw mail and reparse
-- never touches it. messages.spam_score (0002) stays unwritten:
-- messages is derived, and a model's score is not.
--
-- The new table is empty and the rewritten constraint admits every row
-- the old one did: `yuuka migrate` is all it takes.

-- One row per scoring run of one message: every question of one
-- question set, answered at once and versioned as a whole, so one run
-- is one row rather than one row per question. Append-only: rescoring
-- (a new model, a reworded question) writes new rows and leaves the old
-- ones, so "why was this marked as phishing last month" is answered by
-- the score of last month. Exactly one statement inserts into this
-- table and none updates or deletes it (store.InsertScore,
-- TestOneWayIntoScores).
CREATE TABLE triage_scores (
    -- A ULID of scored_at.
    id         text COLLATE "C" PRIMARY KEY CHECK (id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    -- Keyed by message id, which is stable across reparse (§4.2). No ON
    -- DELETE: like the decisions on a message (message_state, notes), a
    -- fact about it is not dropped with it. Whatever merges messages
    -- away (message_aliases) carries these rows to the survivor first.
    message_id text COLLATE "C" NOT NULL REFERENCES messages (id),
    -- The scorer as configured (score.Profile.Scorer): a name the
    -- operator gives and changes with the model. Closed alphabet, so it
    -- is safe in any query and any display.
    scorer     text NOT NULL CHECK (scorer ~ '^[a-z0-9][a-z0-9.-]{0,31}$'),
    -- The backend's model and version, as it reported them.
    model      text NOT NULL CHECK (length(model) BETWEEN 1 AND 200),
    -- The version of the question set. A reworded question is a new
    -- version; scores of different versions are not compared.
    questions  smallint NOT NULL CHECK (questions > 0),
    -- By question key: {"p": {option: probability}} or {"abstain": true}.
    -- The store validates keys and probabilities before writing; the
    -- size cap is a backstop.
    answers    jsonb NOT NULL CHECK (jsonb_typeof(answers) = 'object' AND octet_length(answers::text) <= 16384),
    -- What the scorer was shown, never the content: the facts it was
    -- given, the endpoint (local or hosted: whether the content left the
    -- deployment), view and parser versions, characters sent, whether
    -- they were truncated, and the SHA-256 of the text (score.Input).
    -- Facts change later (a prefix claimed after the mail arrived), so
    -- the row keeps the ones it was computed from.
    input      jsonb NOT NULL CHECK (jsonb_typeof(input) = 'object' AND octet_length(input::text) <= 4096),
    -- When the scorer answered. No default: the writer gives it, and
    -- the id is a ULID of it.
    scored_at  timestamptz NOT NULL
);
-- The latest score of a message under a (scorer, questions) profile:
-- the read path's lookup and the scorer's "not scored yet"
-- (store.ScoreTargets).
CREATE INDEX triage_scores_latest ON triage_scores (message_id, scorer, questions, scored_at DESC, id DESC);

-- A score arriving is logged as a change of kind triage, its target the
-- message id, in the score's transaction: a long-running agent follows
-- it to mail a model was unsure of (§8.6, §15.7). Its visibility is the
-- message's.
ALTER TABLE changes DROP CONSTRAINT changes_kind_check;
ALTER TABLE changes ADD CONSTRAINT changes_kind_check CHECK (kind IN
    ('message', 'delivery', 'thread', 'flag', 'prefix', 'proposal', 'draft', 'outbox', 'rule', 'note', 'triage'));
