-- Third-party secrets, transports and each identity's transport chain
-- (§17). Transports were process configuration in M2 (0005: a name in
-- identities.transport, the rest in YUUKA_TRANSPORT_<NAME>_*); several
-- zones and outside mailboxes, each with its own exit and credentials,
-- made them runtime data (§17, §9.3).
--
-- §4.2 categories: all three tables are configuration. reparse touches
-- none of them.
--
-- The writers are internal/store/transports.go. What the encrypted
-- columns hold is internal/secret's (§17.2): the store only persists a
-- secret.Record and never encrypts or decrypts anything.

-- ---------------------------------------------------------------------------
-- secrets

-- One generic table that transports (and later notification channels and
-- pull account sources) each reference through a unique foreign key, so
-- sealing, opening, rewrapping, `secret ls` / `check` / `rewrap` have one
-- path, and the wrapper and key reference live in one place (§17.4).
CREATE TABLE secrets (
    id          text COLLATE "C" PRIMARY KEY
                CHECK (id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    -- Closed list: a new kind arrives with the migration of its first
    -- user. telegram_bot_token's user is 0018 (§8.4), already designed.
    kind        text NOT NULL CHECK (kind IN ('smtp_password', 'telegram_bot_token')),
    -- Who the plaintext may be handed to (§17.2), e.g. smtps://user@host:465.
    -- Sealed into the payload's AAD; whoever opens it recomputes it from
    -- the owning row, so changing a transport's destination needs a
    -- fresh seal. A row's audience never changes: a new destination is
    -- a new secret.
    audience    text NOT NULL CHECK (audience <> '' AND octet_length(audience) <= 512),
    -- Envelope encryption (§17.2): the payload is sealed under this row's
    -- own data key (DEK), and a wrapper wraps the DEK. Vault Transit or a
    -- cloud KMS can be added to this list without a data migration.
    wrapper     text NOT NULL CHECK (wrapper IN ('local')),
    -- Which of the wrapper's keys: for local the KEK's fingerprint.
    key_ref     text NOT NULL CHECK (key_ref <> '' AND octet_length(key_ref) <= 512),
    -- The wrapped DEK, in the wrapper's own format.
    wrapped_dek bytea NOT NULL CHECK (octet_length(wrapped_dek) BETWEEN 1 AND 4096),
    -- The sealed value, starting with its format version. The byte layout
    -- belongs to internal/secret and testdata/secret/ (secret.Record.Payload).
    payload     bytea NOT NULL CHECK (octet_length(payload) BETWEEN 1 AND 65536),
    -- Bumped on every write (a new payload, a rewrapped DEK); a write names
    -- the revision it read. Clients built from a secret are cached by it
    -- (§17.5).
    revision    bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at  timestamptz NOT NULL DEFAULT now(),
    -- When the payload was last sealed; a rewrap leaves it alone.
    sealed_at   timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- transports

-- An exit: where mail goes and how yuuka logs in there (§10.2).
CREATE TABLE transports (
    id            text COLLATE "C" PRIMARY KEY
                  CHECK (id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    -- Never changes: send_attempts.transport records the name, a send fact
    -- (§4.2), and a rename would point history elsewhere. Removing a
    -- transport disables it and erases its secret; the row stays.
    name          text NOT NULL UNIQUE CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$'),
    kind          text NOT NULL DEFAULT 'smtp' CHECK (kind IN ('smtp')),
    -- secret.CanonicalHost of the server: the spelling the audience uses
    -- and the one the transport dials.
    host          text CHECK (host <> ''),
    port          integer CHECK (port BETWEEN 1 AND 65535),
    -- M3.5 adds xoauth2 and an account_id (§17.8).
    auth          text CHECK (auth IN ('none', 'plain')),
    username      text NOT NULL DEFAULT '',
    secret_id     text COLLATE "C" UNIQUE REFERENCES secrets (id),
    -- What the exit lets the caller set (§10.2). Required once configured,
    -- with no default: it decides which header fields are stripped from
    -- every message, and a wrong default would silently change mail.
    caps          text[] CHECK (caps <@ ARRAY['message-id', 'return-path', 'dkim', 'status-query']),
    -- NULL: a placeholder for a name identities used before this table
    -- existed (below), or one the environment names (§17.7). It cannot
    -- send until someone configures it.
    configured_at timestamptz,
    disabled_at   timestamptz,
    revision      bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at    timestamptz NOT NULL DEFAULT now(),
    CHECK ((configured_at IS NULL) = (host IS NULL)),
    CHECK (configured_at IS NULL OR (port IS NOT NULL AND auth IS NOT NULL AND caps IS NOT NULL)),
    CHECK (configured_at IS NOT NULL OR secret_id IS NULL),
    -- A disabled transport has had its secret erased.
    CHECK (disabled_at IS NOT NULL OR auth IS DISTINCT FROM 'plain' OR (secret_id IS NOT NULL AND username <> ''))
);

-- A placeholder for every transport name an identity uses. The migration
-- cannot read the environment that defines them, so they are filled in by
-- the import at startup (§17.7) or by a person. Their ids are ULIDs made
-- here as in 0005: random() is not a CSPRNG, but transport ids are not
-- secrets, only unique, and a deployment has a handful.
DO $$
DECLARE
    alphabet constant text := '0123456789ABCDEFGHJKMNPQRSTVWXYZ';
    ms bigint := floor(extract(epoch FROM clock_timestamp()) * 1000)::bigint;
    n text;
    new_id text;
BEGIN
    FOR n IN SELECT DISTINCT transport FROM identities ORDER BY 1 LOOP
        new_id := '';
        FOR i IN REVERSE 9..0 LOOP
            new_id := new_id || substr(alphabet, ((ms >> (5 * i)) & 31)::int + 1, 1);
        END LOOP;
        FOR i IN 1..16 LOOP
            new_id := new_id || substr(alphabet, floor(random() * 32)::int + 1, 1);
        END LOOP;
        INSERT INTO transports (id, name) VALUES (new_id, n);
    END LOOP;
END $$;

-- ---------------------------------------------------------------------------
-- identity_transports

-- Each identity's transport chain (§9.3, §10.2.2): position 0 is the
-- primary, the rest are fallbacks in order. Positions run 0..n-1 without
-- gaps; the writer checks that, a constraint cannot.
CREATE TABLE identity_transports (
    identity_id  text COLLATE "C" NOT NULL REFERENCES identities (id),
    position     smallint NOT NULL CHECK (position BETWEEN 0 AND 7),
    transport_id text COLLATE "C" NOT NULL REFERENCES transports (id),
    PRIMARY KEY (identity_id, position),
    UNIQUE (identity_id, transport_id)
);

-- Which identities send through a transport.
CREATE INDEX identity_transports_transport ON identity_transports (transport_id);

-- Every identity keeps its transport, as a chain of one.
INSERT INTO identity_transports (identity_id, position, transport_id)
SELECT i.id, 0, t.id FROM identities i JOIN transports t ON t.name = i.transport;

ALTER TABLE identities DROP COLUMN transport;
