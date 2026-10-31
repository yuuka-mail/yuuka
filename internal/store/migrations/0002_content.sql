-- M1 content layer (§4): messages and threads derived from raw, the
-- decision tables reparse never touches, the prefix registry and the
-- change log. Deviations from §4 are marked "M1:".

-- pg_trgm backs substring search over subject/sender/body (§4). It is a
-- trusted extension, so a database owner can create it. The test harness
-- creates it in public beforehand; then this is a no-op.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- Derived (§4.2): rebuilt by reparse. Ids are kept where possible, but
-- unlike message ids they carry no promise.
CREATE TABLE threads (
    id           text COLLATE "C" PRIMARY KEY
                 CHECK (id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    -- Subject of the earliest message without reply/forward prefixes,
    -- lowercased; the key of the human-only subject+sender fallback.
    subject_norm text NOT NULL,
    -- Distinct from_addr of the messages, in order of first appearance.
    participants text[] NOT NULL,
    first_at     timestamptz NOT NULL,
    last_at      timestamptz NOT NULL,
    n            integer NOT NULL CHECK (n > 0),
    -- 0 unknown, 1 machine, 2 human: human if any message is, else
    -- machine if any is, else unknown.
    class        smallint NOT NULL CHECK (class IN (0, 1, 2))
);

-- Derived (§4.2) except id, which is stable across reparse.
CREATE TABLE messages (
    id               text COLLATE "C" PRIMARY KEY
                     CHECK (id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    -- The raw of the earliest delivery; each delivery keeps its own raw.
    canonical_raw_id text COLLATE "C" NOT NULL REFERENCES raw_objects (id),
    -- Fold key (§4.1), copied from raw_objects.content_hash. NULL for
    -- header_only raw, which never folds (§4.4); NULLs do not collide.
    content_hash     bytea UNIQUE CHECK (octet_length(content_hash) = 32),
    -- Message-ID, In-Reply-To and References without angle brackets;
    -- used for threading only, never for folding.
    message_id       text,
    in_reply_to      text,
    refs             text[] NOT NULL DEFAULT '{}',
    thread_id        text COLLATE "C" NOT NULL REFERENCES threads (id),
    date_header      timestamptz,
    -- Earliest delivery; lists sort by it.
    received_at      timestamptz NOT NULL,
    subject          text NOT NULL,
    snippet          text NOT NULL,
    -- Lowercased address and decoded display name of the first From.
    from_addr        text NOT NULL,
    from_name        text NOT NULL,
    -- 0 unknown, 1 machine, 2 human (internal/classify). Human overrides
    -- live in message_state.class_override, never here.
    class            smallint NOT NULL CHECK (class IN (0, 1, 2)),
    -- M1: always NULL. The §4.3 minimal downgrade is computed live from
    -- deliveries.auth and prefixes.state instead (a claim takes effect
    -- without a reparse); X-CF-SpamH-Score stays in deliveries.auth until
    -- its scale is known.
    spam_score       real,
    has_attach       boolean NOT NULL,
    -- Capped at 1 MiB (mimeparse.MaxBodyText); the raw is the source.
    body_text        text NOT NULL,
    -- M1: always NULL; HTML rendering arrives with the WebUI.
    body_html_key    text,
    parser_version   integer NOT NULL,
    -- M1: the quarantine state of §3 principle 5. NULL = parsed cleanly;
    -- otherwise what went wrong, with whatever could be read kept in the
    -- other columns. It lives here and not as raw_objects.ingest_state
    -- because it is derived: a better parser clears it on reparse, and
    -- raw_objects holds facts reparse must not touch (§4.2).
    parse_error      text
);

CREATE INDEX messages_thread_id ON messages (thread_id);
CREATE INDEX messages_received_at ON messages (received_at DESC, id DESC);
CREATE INDEX messages_message_id ON messages (message_id) WHERE message_id IS NOT NULL;
CREATE INDEX messages_in_reply_to ON messages (in_reply_to) WHERE in_reply_to IS NOT NULL;
CREATE INDEX messages_refs ON messages USING gin (refs);
CREATE INDEX messages_quarantine ON messages (id) WHERE parse_error IS NOT NULL;
-- One trigram index over the concatenation the search predicate uses
-- (store.Search); the expression must stay identical to it.
CREATE INDEX messages_search ON messages USING gin
    ((subject || ' ' || from_name || ' ' || from_addr || ' ' || body_text) gin_trgm_ops);

-- Decisions (§4.2): written by people and rules, never by reparse.
CREATE TABLE message_state (
    message_id     text COLLATE "C" PRIMARY KEY REFERENCES messages (id),
    flags          bigint NOT NULL DEFAULT 0,
    labels         text[] NOT NULL DEFAULT '{}',
    class_override smallint CHECK (class_override IN (0, 1, 2)),
    pinned         boolean NOT NULL DEFAULT false
);

-- A message merged away by a content_hash algorithm change points at the
-- survivor, so yuuka:// anchors and references keep resolving (§4.2).
CREATE TABLE message_aliases (
    old_id text COLLATE "C" PRIMARY KEY
           CHECK (old_id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    new_id text COLLATE "C" NOT NULL REFERENCES messages (id)
);

-- The (zone, prefix) registry (§3.3). Rows appear on first sight as
-- discovered; claiming is an explicit act. label, note, expected_domains
-- and state are decisions. first_seen/last_seen are observations that
-- ingest and reparse only ever widen.
CREATE TABLE prefixes (
    zone             text NOT NULL,
    prefix           text NOT NULL,
    first_seen       timestamptz NOT NULL,
    last_seen        timestamptz NOT NULL,
    label            text,
    note             text,
    expected_domains text[] NOT NULL DEFAULT '{}',
    -- M1: not filled yet. It needs the DMARC-aligned organizational
    -- domain (§3.3), which arrives with leak detection (M3).
    observed_domains text[] NOT NULL DEFAULT '{}',
    -- 1 claimed, 2 discovered.
    state            smallint NOT NULL DEFAULT 2 CHECK (state IN (1, 2)),
    leak_suspect     boolean NOT NULL DEFAULT false,
    active           boolean NOT NULL DEFAULT true,
    PRIMARY KEY (zone, prefix)
);

-- Global change log (§4, §8.6): one row per changed object, in commit
-- order of the writers (they serialize on one advisory lock).
-- target_id is a ULID, or "prefix@zone" for kind = 'prefix'.
CREATE TABLE changes (
    seq       bigserial PRIMARY KEY,
    kind      text NOT NULL
              CHECK (kind IN ('message', 'delivery', 'thread', 'flag', 'prefix', 'proposal')),
    target_id text NOT NULL,
    at        timestamptz NOT NULL DEFAULT now()
);

-- The foreign key M0 deferred until messages existed.
ALTER TABLE deliveries
    ADD CONSTRAINT deliveries_message_id_fkey FOREIGN KEY (message_id) REFERENCES messages (id);
CREATE INDEX deliveries_message_id ON deliveries (message_id);
-- Deliveries M0 committed before parsing existed, or whose parse step
-- failed after the commit point, are found by this.
CREATE INDEX deliveries_unprocessed ON deliveries (received_at) WHERE message_id IS NULL;

-- M1: still no accounts table. deliveries.account_key stays as in M0 and
-- the trusted authserv-id of §10.1.1 is process configuration
-- (YUUKA_TRUSTED_AUTHSERV_ID) until accounts lands with multi-account
-- support; the M0 plan for account_id is unchanged.
-- M1: attachments and extractions are not created; M1 only records
-- has_attach, and extraction is M3.
