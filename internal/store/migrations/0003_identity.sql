-- M1 track B: principals, credentials and authorization (§13), the actions
-- audit log (§8.8), and what the built-in WebUI login needs (§13.7):
-- passkeys, one-time enrollment tokens and offline recovery codes.
--
-- Every table here is a decision or configuration (§4.2): none of it can be
-- rebuilt from raw mail, and reparse never touches it. Deviations from the
-- design are marked "M1:".
--
-- Secrets: only server-generated, high-entropy secrets are stored, and only
-- as SHA-256 (secret_hash, code_hash). A slow password hash buys nothing
-- for 256-bit tokens or 80-bit recovery codes, and nothing here ever
-- accepts a human-chosen secret (§13.7: no passwords).

CREATE TABLE principals (
    id           text COLLATE "C" PRIMARY KEY
                 CHECK (id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    kind         text NOT NULL
                 CHECK (kind IN ('human', 'agent', 'service', 'rule', 'system')),
    -- Shown as kind:name (human:<operator>, agent:arisu, rule:<id>).
    name         text NOT NULL CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$'),
    -- The signature layer of §11.1 reads this; the UI does not hard-code names.
    display_name text NOT NULL,
    -- Service credentials belong to a human, forming a revocation chain.
    owner_id     text COLLATE "C" REFERENCES principals (id) CHECK (owner_id <> id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    disabled_at  timestamptz,
    UNIQUE (kind, name)
);

-- There is exactly one system principal; code refers to it by this id.
CREATE UNIQUE INDEX principals_one_system ON principals ((true)) WHERE kind = 'system';
INSERT INTO principals (id, kind, name, display_name)
VALUES ('00000000000000000000000000', 'system', 'system', 'yuuka');

CREATE TABLE credentials (
    id           text COLLATE "C" PRIMARY KEY
                 CHECK (id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    principal_id text COLLATE "C" NOT NULL REFERENCES principals (id),
    kind         text NOT NULL
                 CHECK (kind IN ('session', 'token', 'smtp_password', 'oauth')),
    label        text NOT NULL DEFAULT '',
    -- SHA-256 of the secret part of the presented string; the credential id
    -- travels in clear next to it, so lookup is by id and the hash is
    -- compared in constant time.
    secret_hash  bytea CHECK (octet_length(secret_hash) = 32),
    -- oauth refresh tokens must be recoverable, so they are encrypted with
    -- the local KEK instead of hashed (§13.3). M1 writes no oauth rows.
    secret_enc   bytea,
    -- Flat scopes (§13.4). The list is closed: a new scope needs a migration
    -- that replaces this constraint, which is the point.
    scopes       text[] NOT NULL DEFAULT '{}'
                 CONSTRAINT credentials_scopes_known CHECK (scopes <@ ARRAY[
                     'message:read', 'delivery:read', 'prefix:read',
                     'message:annotate', 'draft:create', 'submission:send',
                     'prefix:manage', 'account:manage', 'rule:manage', 'admin']::text[]),
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz,
    last_used_at timestamptz,
    revoked_at   timestamptz,
    CHECK ((kind = 'oauth') = (secret_hash IS NULL)),
    CHECK ((kind = 'oauth') = (secret_enc IS NOT NULL)),
    -- A session that never expires is a bug, not a policy.
    CHECK (kind <> 'session' OR expires_at IS NOT NULL)
);

CREATE INDEX credentials_principal ON credentials (principal_id);

-- WebAuthn credentials of a principal (§13.7). M1: not rows of credentials:
-- a passkey is a login method that yields a session credential; it carries
-- no scopes and its secret never leaves the authenticator. Columns follow
-- the WebAuthn credential record so that no library's serialization format
-- is frozen into the database.
CREATE TABLE passkeys (
    id                 text COLLATE "C" PRIMARY KEY
                       CHECK (id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    principal_id       text COLLATE "C" NOT NULL REFERENCES principals (id),
    credential_id      bytea NOT NULL UNIQUE
                       CHECK (octet_length(credential_id) BETWEEN 1 AND 1023),
    -- COSE_Key as returned at registration.
    public_key         bytea NOT NULL,
    sign_count         bigint NOT NULL DEFAULT 0 CHECK (sign_count BETWEEN 0 AND 4294967295),
    aaguid             bytea,
    transports         text[] NOT NULL DEFAULT '{}',
    user_verified      boolean NOT NULL,
    backup_eligible    boolean NOT NULL,
    backup_state       boolean NOT NULL,
    attestation_type   text NOT NULL,
    attestation_format text NOT NULL,
    label              text NOT NULL DEFAULT '',
    created_at         timestamptz NOT NULL DEFAULT now(),
    last_used_at       timestamptz,
    revoked_at         timestamptz
);

CREATE INDEX passkeys_principal ON passkeys (principal_id);

-- One-time links that may start exactly one passkey registration: issued
-- by `yuuka admin enroll` (reason enroll) or by spending a recovery code
-- (reason recovery). They never create a session.
CREATE TABLE enrollment_tokens (
    id           text COLLATE "C" PRIMARY KEY
                 CHECK (id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    principal_id text COLLATE "C" NOT NULL REFERENCES principals (id),
    secret_hash  bytea NOT NULL CHECK (octet_length(secret_hash) = 32),
    reason       text NOT NULL CHECK (reason IN ('enroll', 'recovery')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    used_at      timestamptz,
    passkey_id   text COLLATE "C" REFERENCES passkeys (id),
    CHECK ((used_at IS NULL) = (passkey_id IS NULL))
);

-- Offline recovery codes (§13.7). Unlike tokens they carry no id, so they
-- are found by hash; see internal/authn for why that is not a timing leak.
-- Re-running enroll deletes the old set and inserts a new one.
CREATE TABLE recovery_codes (
    id           text COLLATE "C" PRIMARY KEY
                 CHECK (id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    principal_id text COLLATE "C" NOT NULL REFERENCES principals (id),
    code_hash    bytea NOT NULL UNIQUE CHECK (octet_length(code_hash) = 32),
    created_at   timestamptz NOT NULL DEFAULT now(),
    used_at      timestamptz
);

CREATE INDEX recovery_codes_principal ON recovery_codes (principal_id);

-- Who may send as which identity, and how far (§13.5, §8.9). M1: schema
-- only; nothing reads it until sending exists.
CREATE TABLE identity_grants (
    -- M1: no identities table yet; it arrives with sending (M2), whose
    -- migration adds the foreign key.
    identity_id                text COLLATE "C" NOT NULL,
    principal_id               text COLLATE "C" NOT NULL REFERENCES principals (id),
    -- The default is the safe end; the human-owned-identity cap of §8.9
    -- (draft at most for anyone but the owner) is enforced in the API
    -- layer, not here, because it depends on identities.owner_id.
    send_policy                text NOT NULL DEFAULT 'none'
                               CHECK (send_policy IN ('none', 'draft', 'review', 'auto')),
    recipient_scope            text NOT NULL DEFAULT 'thread'
                               CHECK (recipient_scope IN ('thread', 'allowlist', 'any')),
    -- Addresses or domains.
    recipient_allowlist        text[] NOT NULL DEFAULT '{}',
    -- NULL means no per-grant limit; the global circuit breaker still
    -- applies. M2 decides the defaults when it enforces them.
    max_per_hour               integer CHECK (max_per_hour >= 0),
    max_per_day                integer CHECK (max_per_day >= 0),
    max_new_recipients_per_day integer CHECK (max_new_recipients_per_day >= 0),
    -- Recall window of the review tier.
    hold_seconds               integer NOT NULL DEFAULT 0 CHECK (hold_seconds >= 0),
    -- Circuit breaker or manual pause.
    suspended_at               timestamptz,
    created_at                 timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (identity_id, principal_id)
);

-- Account-level read ACL (§13.8). M1: schema only; visibility filtering of
-- reads arrives in phase 2.
CREATE TABLE account_access (
    -- M1: no accounts table yet (deliveries still carries account_key, see
    -- 0001); the migration that creates accounts adds the foreign key and
    -- converts these values.
    account_id   text NOT NULL,
    principal_id text COLLATE "C" NOT NULL REFERENCES principals (id),
    can_read     boolean NOT NULL,
    PRIMARY KEY (account_id, principal_id)
);

-- Audit log (§8.8): every action, by whom, through which credential.
CREATE TABLE actions (
    id            text COLLATE "C" PRIMARY KEY
                  CHECK (id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    -- M1: NULL for events whose caller is not authenticated, such as a
    -- failed login or a rejected recovery code; §8.8 only had actors.
    actor         text COLLATE "C" REFERENCES principals (id),
    -- M1: which credential acted, so "which key did that" has an answer
    -- after a principal holds several.
    credential_id text COLLATE "C" REFERENCES credentials (id),
    -- Dotted verb, e.g. auth.login, auth.recovery.use.
    tool          text NOT NULL,
    -- Never holds a secret.
    args          jsonb NOT NULL DEFAULT '{}',
    result        jsonb,
    undo_token    text,
    at            timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX actions_at ON actions (at);
CREATE INDEX actions_actor_at ON actions (actor, at);
