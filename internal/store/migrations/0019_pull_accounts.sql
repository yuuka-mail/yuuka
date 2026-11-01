-- M3.5: accounts that yuuka pulls over IMAP (§9.2, §9.5, §10.1.3), and
-- the default that lets account_access be enforced without changing who
-- reads what on an existing deployment (§13.8). Deviations from the
-- design block of §4 are marked "M3.5:".
--
-- §4.2 categories of what this creates or changes; reparse touches none
-- of it:
--   configuration   accounts' new columns, account_sources, and
--                   secrets.kind gaining imap_password
--   delivery facts  source_mailboxes and pull_failures (what the remote
--                   said and how far we read), deliveries' new columns
--
-- The writers are internal/store/pull.go and internal/store/accounts.go.
-- The IMAP password lives in secrets (0017) like a transport's; the
-- store only persists a secret.Record and never encrypts or decrypts.

-- ---------------------------------------------------------------------------
-- secrets

-- The IMAP password of a pull source, sealed for
-- imaps://<username>@<host>:<port> (§17.8). A Gmail app password used
-- for both IMAP and SMTP is stored twice, once per audience. The OAuth
-- kinds arrive with M3.5b.
ALTER TABLE secrets DROP CONSTRAINT secrets_kind_check;
ALTER TABLE secrets ADD CONSTRAINT secrets_kind_check
    CHECK (kind IN ('smtp_password', 'telegram_bot_token', 'imap_password'));

-- ---------------------------------------------------------------------------
-- accounts

-- imap joins the closed list. gmail_api and smtp_only join it when they
-- have a reader (§9.5).
ALTER TABLE accounts DROP CONSTRAINT accounts_kind_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_kind_check CHECK (kind IN ('cf_catchall', 'imap'));

ALTER TABLE accounts
    -- §9.2. M3.5: only full. metadata is the last step of M3.5 and adds
    -- its value with its reader (FetchBody, §4.4); notify comes with its
    -- notifications table. A value nothing reads would be a promise.
    ADD COLUMN sync_policy      text NOT NULL DEFAULT 'full' CHECK (sync_policy IN ('full')),
    -- Writeback is not done (§9.2). The column exists so that turning it
    -- on later widens a constraint instead of changing what a missing
    -- column meant.
    ADD COLUMN writeback_policy text NOT NULL DEFAULT 'off' CHECK (writeback_policy IN ('off')),
    -- Offload (§9.6). M3.6 decides its shape and replaces this
    -- constraint; until then NULL, which means off.
    ADD COLUMN offload_policy   jsonb CHECK (offload_policy IS NULL),
    -- Whether a principal without an account_access row may read this
    -- account (§13.8); a row overrides it either way. Every account that
    -- exists today is a Cloudflare account everyone reads, so they get
    -- true and no deployment sees anything different after the upgrade.
    -- The default is dropped below: a new account states its own
    -- (Cloudflare true, pull false).
    ADD COLUMN default_read     boolean NOT NULL DEFAULT true,
    -- A removed pull account keeps its row, deliveries and raw objects,
    -- and its name is never reused (like transports, §17.4).
    ADD COLUMN disabled_at      timestamptz;

ALTER TABLE accounts ALTER COLUMN default_read DROP DEFAULT;

-- ---------------------------------------------------------------------------
-- account_sources

-- Where and how a pull account is read (§9.5, §10.1.3): one source per
-- account. Configuration, written with a revision like transports.
CREATE TABLE account_sources (
    account_id          text COLLATE "C" PRIMARY KEY REFERENCES accounts (id),
    kind                text NOT NULL CHECK (kind IN ('imap')),
    -- The mailbox's own address, lowercase. Pulled deliveries have it as
    -- envelope_to and as their zone and prefix (§9.3), so replies resolve
    -- to this account's identity and the claimed prefix keeps the mail
    -- out of the §4.3 downgrade.
    address             text NOT NULL CHECK (address = lower(address) AND address ~ '^[^@[:space:]]+@[^@[:space:]]+$'
                            AND octet_length(address) <= 320),
    -- secret.CanonicalHost of the server. Implicit TLS only, like
    -- transports; the audience of the password is computed from these.
    host                text NOT NULL CHECK (host <> ''),
    port                integer NOT NULL CHECK (port BETWEEN 1 AND 65535),
    -- M3.5b adds xoauth2.
    auth                text NOT NULL CHECK (auth IN ('plain')),
    username            text NOT NULL CHECK (username <> ''),
    -- The imap_password. NULL only once removed: removing a source erases
    -- its secret.
    secret_id           text COLLATE "C" UNIQUE REFERENCES secrets (id),
    -- Server differences fixed in code (§10.1): gmail syncs \All only and
    -- requires X-GM-EXT-1.
    preset              text CHECK (preset IN ('gmail')),
    -- Remote folders to sync, by their UTF-8 names. NULL: the default
    -- (\All for gmail; otherwise every selectable folder except \Junk,
    -- \Trash and \Drafts).
    mailboxes           text[] CHECK (cardinality(mailboxes) > 0),
    -- Whose Authentication-Results this account's mail trusts (§10.1.1).
    -- NULL: none, so all of it is unverified. Read by ingest and reparse
    -- for this account's deliveries only.
    trusted_authserv_id text CHECK (trusted_authserv_id <> '' AND octet_length(trusted_authserv_id) <= 255),
    -- Larger mail is stored header only (§4.4, §10.1.2).
    max_message_bytes   bigint NOT NULL CHECK (max_message_bytes > 0),
    -- Backfill stops at mail older than this. NULL: all of it (§10.1.4).
    backfill_since      timestamptz,
    -- NULL: unlimited.
    daily_byte_budget   bigint CHECK (daily_byte_budget > 0),
    poll_seconds        integer NOT NULL CHECK (poll_seconds > 0),
    -- Paused by the operator, or by the worker when login was refused
    -- (§10.1.3): retrying a wrong password only gets the mailbox locked.
    paused_at           timestamptz,
    pause_reason        text CHECK (pause_reason IN ('operator', 'auth_failed')),
    disabled_at         timestamptz,
    -- Bumped on every configuration write (and pause, resume, removal);
    -- a write names the revision it read. The source manager restarts a
    -- worker when it, or its secret's, moves on (§10.1.3).
    revision            bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at          timestamptz NOT NULL DEFAULT now(),
    -- M3.5: not in the design block. `yuuka account sync` asks serve
    -- for a round by writing these (§10.1.3), retry_requested_at when it
    -- is to fetch the recorded failures again; the worker clears them
    -- when it takes the request. Requests, not configuration: they do not
    -- move the revision.
    sync_requested_at   timestamptz,
    retry_requested_at  timestamptz,
    -- M3.5: not in the design block. The bytes fetched on budget_day (a
    -- UTC date), so the daily budget survives a restart of serve; a
    -- counter, not configuration.
    budget_day          date,
    budget_used         bigint NOT NULL DEFAULT 0 CHECK (budget_used >= 0),
    CHECK ((paused_at IS NULL) = (pause_reason IS NULL)),
    -- An enabled source has its password; a removed one has none.
    CHECK ((disabled_at IS NULL) = (secret_id IS NOT NULL))
);

-- One enabled source per mailbox: two would pull the same mail into two
-- accounts and claim the same prefix twice.
CREATE UNIQUE INDEX account_sources_address ON account_sources (address) WHERE disabled_at IS NULL;

-- ---------------------------------------------------------------------------
-- source_mailboxes

-- How far each remote folder has been read (§10.1.2, §10.1.3). A delivery
-- fact: what the remote said and where we got to; reparse never touches
-- it.
CREATE TABLE source_mailboxes (
    account_id         text COLLATE "C" NOT NULL REFERENCES account_sources (account_id),
    -- Decoded to UTF-8 (the loc: source keys use this spelling).
    name               text NOT NULL CHECK (name <> '' AND octet_length(name) <= 1024),
    -- The folder's SPECIAL-USE attribute (RFC 6154) as the server
    -- reported it, e.g. \All. NULL: none.
    special_use        text CHECK (octet_length(special_use) <= 64),
    -- How this folder's source keys are made (§10.1), fixed on the first
    -- sync and never switched automatically:
    --   1  location   loc:<uidvalidity>:<uid>:<name>
    --   2  OBJECTID   oid:<EMAILID> (RFC 8474)
    --   3  Gmail      gm:<X-GM-MSGID as 16 lowercase hex digits>
    key_kind           smallint NOT NULL CHECK (key_kind IN (1, 2, 3)),
    -- IMAP UIDs and UIDVALIDITY are 32-bit and non-zero.
    uidvalidity        bigint NOT NULL CHECK (uidvalidity BETWEEN 1 AND 4294967295),
    -- The cursors (§10.1.2): every UID in [coalesce(low_uid, high_uid + 1),
    -- high_uid] is committed or recorded in pull_failures. Catch-up moves
    -- high_uid up; backfill moves low_uid down. Both move by
    -- compare-and-set after the mail below them is committed, never
    -- before (§3.5).
    --   high_uid  0: nothing below it yet
    --   low_uid   NULL: backfill has not started, everything up to
    --             high_uid is still the backfill's; 1: it reached the
    --             bottom
    high_uid           bigint NOT NULL CHECK (high_uid BETWEEN 0 AND 4294967295),
    low_uid            bigint CHECK (low_uid BETWEEN 1 AND 4294967295),
    -- What STATUS said at the end of the last round that finished this
    -- folder, to skip it while nothing changed (§10.1.2).
    uidnext_seen       bigint CHECK (uidnext_seen BETWEEN 1 AND 4294967295),
    messages_seen      bigint CHECK (messages_seen >= 0),
    -- A location-keyed folder whose UIDVALIDITY changed: the old value,
    -- while deliveries under it are still being matched to their new
    -- locations (§10.1.3). NULL: nothing pending.
    rescan_uidvalidity bigint CHECK (rescan_uidvalidity BETWEEN 1 AND 4294967295),
    -- M3.5: not in the design block. Mail with \Draft is seen but not
    -- stored, only counted (§10.1); the count is shown by `account ls`.
    -- Added with the cursor move that passed it, so it counts each once.
    drafts_skipped     bigint NOT NULL DEFAULT 0 CHECK (drafts_skipped >= 0),
    last_round_at      timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, name),
    CHECK (low_uid IS NULL OR low_uid <= high_uid + 1),
    CHECK (rescan_uidvalidity IS NULL OR (key_kind = 1 AND rescan_uidvalidity <> uidvalidity))
);

-- ---------------------------------------------------------------------------
-- pull_failures

-- Mail that could not be fetched (§10.1.3): the server answered NO, the
-- literal was cut short, or its yuuka-pull metadata could not be encoded.
-- A cursor passes such a UID only once it has a row here, so nothing is
-- skipped silently. A delivery fact.
CREATE TABLE pull_failures (
    account_id  text COLLATE "C" NOT NULL,
    mailbox     text NOT NULL,
    uidvalidity bigint NOT NULL CHECK (uidvalidity BETWEEN 1 AND 4294967295),
    uid         bigint NOT NULL CHECK (uid BETWEEN 1 AND 4294967295),
    -- RFC822.SIZE as the server said it, when it said.
    size        bigint CHECK (size >= 0),
    -- What went wrong. Partly the server's words, which are untrusted
    -- input: shown as data, never put verbatim into alerts or logs.
    detail      text NOT NULL CHECK (octet_length(detail) <= 1024),
    attempts    integer NOT NULL CHECK (attempts > 0),
    first_at    timestamptz NOT NULL,
    last_at     timestamptz NOT NULL,
    -- Fetched after all (a retry), so no longer counted.
    resolved_at timestamptz,
    PRIMARY KEY (account_id, mailbox, uidvalidity, uid),
    FOREIGN KEY (account_id, mailbox) REFERENCES source_mailboxes (account_id, name),
    CHECK (first_at <= last_at)
);

-- The open failures of an account, for `account ls` and retries.
CREATE INDEX pull_failures_open ON pull_failures (account_id) WHERE resolved_at IS NULL;

-- ---------------------------------------------------------------------------
-- deliveries

-- Pulled mail has no envelope sender. NULL rather than '': in 0001 ''
-- is the null sender of a bounce, and saying that of pulled mail would
-- mislead. Only a delivery with a remote location may lack one.
-- envelope_to stays NOT NULL: a pulled delivery has the source's address,
-- "delivered to this mailbox".
ALTER TABLE deliveries ALTER COLUMN envelope_from DROP NOT NULL;

ALTER TABLE deliveries
    -- Whether the remote still has it (§9.6): 1 present, 2 gone,
    -- 3 offloaded. M3.5 only writes present; gone (M3.6) and offloaded
    -- (§9.6) replace this constraint with their writers.
    ADD COLUMN remote_state   smallint CHECK (remote_state IN (1)),
    -- When it was offloaded; only for remote_state 3 (§9.6).
    ADD COLUMN offloaded_at   timestamptz,
    -- Gmail's labels when it was pulled (X-GM-LABELS). A snapshot and a
    -- delivery fact: it does not follow later changes on the remote.
    ADD COLUMN remote_labels  text[],
    -- Why only the header was stored (§4.4): 1 larger than the source's
    -- max_message_bytes, 2 the body could not be fetched.
    ADD COLUMN remote_omitted smallint CHECK (remote_omitted IN (1, 2)),
    -- The remote location is all or nothing, and remote_state goes with
    -- it: push deliveries have none of them.
    ADD CONSTRAINT deliveries_remote_location CHECK (
        (remote_mailbox IS NULL) = (remote_uidvalidity IS NULL)
        AND (remote_mailbox IS NULL) = (remote_uid IS NULL)
        AND (remote_mailbox IS NULL) = (remote_state IS NULL)),
    ADD CONSTRAINT deliveries_pulled_facts CHECK (
        remote_mailbox IS NOT NULL
        OR (envelope_from IS NOT NULL AND remote_labels IS NULL AND remote_omitted IS NULL)),
    ADD CONSTRAINT deliveries_offloaded_at CHECK (offloaded_at IS NULL OR remote_state = 3);

-- The remote location, for the UIDVALIDITY remap and the weekly
-- reconciliation (§10.1.3).
CREATE INDEX deliveries_remote ON deliveries (account_id, remote_mailbox, remote_uidvalidity, remote_uid)
    WHERE remote_mailbox IS NOT NULL;
