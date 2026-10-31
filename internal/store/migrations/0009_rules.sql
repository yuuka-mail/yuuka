-- M3 rules and annotations (§8.2, §8.5, §8.8): the rules the operator
-- keeps, the proposals agents make, and one history of every change to a
-- message's class, archived state and labels, whoever made it. Every
-- table here is a decision (§4.2): none of it can be rebuilt from raw
-- mail, and reparse never touches it. The semantics of a stored rule
-- live in internal/rules; the writers are in internal/store (rules.go,
-- annotate.go, proposals.go).

-- What an agent (or a person) suggests and only a person decides.
-- Expiry is computed on read (state pending and expires_at passed), so
-- nothing has to sweep the table.
CREATE TABLE proposals (
    id          text COLLATE "C" PRIMARY KEY CHECK (id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    proposed_by text COLLATE "C" NOT NULL REFERENCES principals (id),
    kind        text NOT NULL CHECK (kind IN ('annotate', 'rule')),
    message_id  text COLLATE "C" REFERENCES messages (id),
    -- annotate: {"v":1,"scope":"message"|"thread","actions":[...]}
    -- rule:     {"v":1,"match":{...},"actions":[...]} (rules.Spec)
    -- The store writes the canonical form of what it parsed, never the
    -- caller's bytes.
    payload     jsonb NOT NULL,
    rationale   text NOT NULL DEFAULT '' CHECK (length(rationale) <= 4000),
    -- [{uri, quote}]: yuuka:// anchors and the text they point at (§8.1).
    cites       jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(cites) = 'array'),
    state       text NOT NULL DEFAULT 'pending'
                CHECK (state IN ('pending', 'accepted', 'rejected', 'withdrawn')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    decided_by  text COLLATE "C" REFERENCES principals (id),
    decided_at  timestamptz,
    -- The action that recorded the decision; for an accepted annotate
    -- proposal it is the one to undo.
    action_id   text COLLATE "C" REFERENCES actions (id),
    CHECK ((state = 'pending') = (decided_at IS NULL)),
    CHECK ((decided_at IS NULL) = (decided_by IS NULL)),
    CHECK (kind <> 'annotate' OR message_id IS NOT NULL),
    -- §8.5 cap, in the schema too: no outbound action can be proposed.
    -- Every action must name a kind (the count), and every kind must be
    -- an annotation. (A deviation from the reviewed draft, which checked
    -- the kinds only: an action without a kind passed it.)
    CONSTRAINT proposals_actions_annotation_only CHECK (
        jsonb_typeof(payload->'actions') = 'array'
        AND jsonb_array_length(jsonb_path_query_array(payload, '$.actions[*].kind')) = jsonb_array_length(payload->'actions')
        AND jsonb_path_query_array(payload, '$.actions[*].kind')
            <@ '["label", "classify", "archive", "notify"]'::jsonb)
);
CREATE INDEX proposals_pending ON proposals (created_at) WHERE state = 'pending';
CREATE INDEX proposals_message ON proposals (message_id) WHERE message_id IS NOT NULL;
-- The same pending proposal twice is one proposal.
CREATE UNIQUE INDEX proposals_once ON proposals
    (proposed_by, kind, coalesce(message_id, ''), md5(payload::text)) WHERE state = 'pending';

CREATE TABLE rules (
    id           text COLLATE "C" PRIMARY KEY CHECK (id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    -- rule:<id>, the actor of everything the rule does. It holds no
    -- credential and no identity grant; the store refuses both (§8.5).
    principal_id text COLLATE "C" NOT NULL UNIQUE REFERENCES principals (id),
    name         text NOT NULL DEFAULT '' CHECK (length(name) <= 200),
    priority     integer NOT NULL DEFAULT 0,          -- ascending; ties by id
    version      smallint NOT NULL CHECK (version IN (1)),
    -- The canonical JSON of the parsed match tree (rules.Match). The size
    -- cap is a backstop for rules.MaxMatchBytes, which the store checks
    -- on the compact form before saving; jsonb's text output adds a space
    -- after every ':' and ',', so the backstop is twice that. (The
    -- reviewed draft had 16384, which a valid rule near the limit would
    -- fail.)
    match        jsonb NOT NULL CHECK (jsonb_typeof(match) = 'object' AND octet_length(match::text) <= 32768),
    actions      jsonb NOT NULL CONSTRAINT rules_actions_annotation_only CHECK (
                     jsonb_typeof(actions) = 'array'
                     AND jsonb_array_length(actions) BETWEEN 1 AND 16
                     AND jsonb_array_length(jsonb_path_query_array(actions, '$[*].kind')) = jsonb_array_length(actions)
                     AND jsonb_path_query_array(actions, '$[*].kind')
                         <@ '["label", "classify", "archive", "notify"]'::jsonb),
    enabled      boolean NOT NULL DEFAULT true,
    -- Bumped by every edit; evaluators cache compiled rules by (id,
    -- revision).
    revision     integer NOT NULL DEFAULT 1 CHECK (revision > 0),
    proposal_id  text COLLATE "C" REFERENCES proposals (id),
    created_by   text COLLATE "C" NOT NULL REFERENCES principals (id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    -- Soft delete: the actions and annotation_log rows of a deleted rule
    -- still name it.
    deleted_at   timestamptz,
    CHECK (NOT (enabled AND deleted_at IS NOT NULL))
);
CREATE INDEX rules_active ON rules (priority, id) WHERE enabled;

-- One row per change of one attribute of one message, by anyone: people,
-- agents, services, rules. It answers "who set this", backs undo, and
-- decides precedence: a writer of rank w may change an attribute only
-- while the latest row for it has rank <= w (store.Annotate). A person
-- removing what a rule added leaves a rank 3 row, so later rule runs
-- leave it removed.
CREATE TABLE annotation_log (
    seq        bigserial PRIMARY KEY,
    action_id  text COLLATE "C" NOT NULL REFERENCES actions (id),
    message_id text COLLATE "C" NOT NULL REFERENCES messages (id),
    attr       text NOT NULL CHECK (attr IN ('class', 'archived', 'label')),
    label      text NOT NULL DEFAULT '' CHECK ((attr = 'label') = (label <> '')),
    -- class: message_state.class_override, NULL (none) or 0..2;
    -- archived, label: 0 off, 1 on.
    before     smallint,
    after      smallint,
    rank       smallint NOT NULL CHECK (rank IN (1, 2, 3)),  -- rule, agent/service, person/system
    -- The row this one reverts; each row is reverted at most once.
    undoes     bigint UNIQUE REFERENCES annotation_log (seq),
    CHECK (before IS DISTINCT FROM after),
    -- Value domains, which the reviewed draft left to the writer.
    CHECK (CASE attr WHEN 'class' THEN coalesce(before, 0) IN (0, 1, 2) AND coalesce(after, 0) IN (0, 1, 2)
                     ELSE before IN (0, 1) AND after IN (0, 1) END)
);
CREATE INDEX annotation_log_key ON annotation_log (message_id, attr, label, seq DESC);
CREATE INDEX annotation_log_action ON annotation_log (action_id);

-- Archived is its own column: it is logged and undoable like labels,
-- while the read state stays in flags, unlogged. Labels stay a text[]
-- on message_state (exact, case-sensitive strings, rules.ValidLabel);
-- who added one is in annotation_log. The label filter adds
-- labels <> '{}' to its @> so the partial index applies.
ALTER TABLE message_state
    ADD COLUMN archived boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT message_state_labels_bounded CHECK (cardinality(labels) <= 64);
CREATE INDEX message_state_labels ON message_state USING gin (labels) WHERE labels <> '{}';

ALTER TABLE changes DROP CONSTRAINT changes_kind_check;
ALTER TABLE changes ADD CONSTRAINT changes_kind_check CHECK (kind IN
    ('message', 'delivery', 'thread', 'flag', 'prefix', 'proposal', 'draft', 'outbox', 'rule'));
