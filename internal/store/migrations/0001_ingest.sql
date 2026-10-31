-- M0: the two tables of the Go-side ingest commit point (§2.1) and of
-- redelivery dedupe (§4.1). Follows §4 as far as M0 allows; deviations are
-- marked "M0:" so the M1 migration knows what to change.
--
-- ULIDs are stored as text in their canonical 26-character uppercase form:
-- the same string appears in R2 keys, logs and yuuka:// anchors, and
-- COLLATE "C" makes text order equal ULID (time) order.

CREATE TABLE raw_objects (
    id           text COLLATE "C" PRIMARY KEY
                 CHECK (id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    -- Key in the raw bucket; for Cloudflare it is raw/YYYY/MM/<id>.eml.
    r2_key       text NOT NULL UNIQUE,
    -- Relative to the data directory; M0 uses the same string as r2_key.
    local_path   text NOT NULL,
    size         bigint NOT NULL CHECK (size >= 0),
    -- Not unique: every object in the raw bucket gets its own row, even if
    -- another Worker invocation stored the same bytes. Re-ingesting the same
    -- object collides on id; whether it is a new delivery is decided by
    -- deliveries.source_key. A unique sha256 would silently drop the
    -- delivery of a second recipient whose raw happened to be identical.
    sha256       bytea NOT NULL CHECK (octet_length(sha256) = 32),
    received_at  timestamptz NOT NULL,
    -- 1 = full, 2 = header_only (§4.4). M0 only ever writes 1.
    completeness smallint NOT NULL DEFAULT 1 CHECK (completeness IN (1, 2)),
    -- §4.1 content hash, algorithm v1 (internal/contenthash): SHA-256 of
    -- the raw bytes with transport header fields removed. A delivery fact
    -- (§4.2), not derived: never recomputed in place. NULL for header_only.
    content_hash bytea CHECK (octet_length(content_hash) = 32),
    -- Set when this raw is a sender redelivery of an already delivered
    -- message: the raw is kept, but no delivery row points at it.
    duplicate_of text COLLATE "C" REFERENCES raw_objects (id)
    -- M0: no ingest_state column; its states only exist once M1 parses.
);

CREATE TABLE deliveries (
    id                 text COLLATE "C" PRIMARY KEY
                       CHECK (id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    -- M0: there is no accounts table yet. account_key is the configured
    -- name of the single Cloudflare account (YUUKA_ACCOUNT). M1 creates
    -- accounts, inserts one row per distinct account_key, adds account_id,
    -- backfills it, moves the unique constraint to (account_id, source_key)
    -- and drops account_key.
    account_key        text NOT NULL,
    -- Stable identity of this delivery at its source; the idempotency key.
    -- Push sources (the Cloudflare Worker) use
    --     lower-hex(content_hash) || '/' || envelope_to
    -- with envelope_to verbatim (no case or IDN folding), so a sender
    -- redelivery to the same address collides here instead of being found
    -- by check-then-insert (§4.1, §10.1).
    source_key         text NOT NULL,
    raw_id             text COLLATE "C" NOT NULL REFERENCES raw_objects (id),
    -- Lowercased domain and local part of envelope_to.
    zone               text NOT NULL,
    prefix             text NOT NULL,
    -- Verbatim; '' is the null sender.
    envelope_from      text NOT NULL,
    envelope_to        text NOT NULL,
    received_at        timestamptz NOT NULL,
    -- Remote location for pull sources (§10.1); NULL for push sources.
    remote_mailbox     text,
    remote_uidvalidity bigint,
    remote_uid         bigint,
    -- Secondary SPF check (§10.1.1), a fact computed once at ingest.
    -- M0: not computed yet, always NULL.
    spf_recheck        jsonb,
    -- Derived columns, the only ones reparse rewrites (§4.2).
    -- M0: message_id stays NULL and has no foreign key until M1 creates
    -- messages; auth is not extracted yet (§10.1.1) and stays NULL.
    message_id         text COLLATE "C",
    auth               jsonb,
    -- M0: delivered_to, remote_labels, remote_flags, remote_state and
    -- offloaded_at are pull/offload facts; their migrations add them.
    UNIQUE (account_key, source_key)
);

CREATE INDEX deliveries_raw_id ON deliveries (raw_id);
CREATE INDEX raw_objects_sha256 ON raw_objects (sha256);
