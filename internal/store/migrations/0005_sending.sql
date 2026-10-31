-- M2: accounts and identities (§9.3, §9.5), and the sending tables of the
-- outbox state machine (§4.5): drafts, outbox, send_attempts,
-- outbox_recipients. Also closes the M0/M1 deferrals: deliveries.account_key
-- becomes account_id, and identity_grants / account_access get the foreign
-- keys 0003 promised. Deviations from §4 are marked "M2:".
--
-- §4.2 categories of what this creates; reparse touches none of it:
--   configuration  accounts, identities
--   decisions      drafts, outbox
--   send facts     send_attempts, outbox_recipients: what a remote server
--                  answered, and when. Like delivery facts they record
--                  outside state at the time and cannot be recomputed.
--
-- M2: transports are not a table. They are named in process configuration
-- (host, port, caps, a credential file path), and identities store only
-- the name. A transport holds a secret and a host the process must reach;
-- both are deployment facts, like the R2 and database settings. A row would
-- only mirror the configuration and could disagree with it.
--
-- M2: no zones table. Its columns (account, catch-all, sending onboarded)
-- had no reader: an identity carries its zone and account, and whether a
-- zone is onboarded for sending is a property of the transport's
-- configuration. Add it when something needs to ask about a zone alone.

-- ---------------------------------------------------------------------------
-- accounts

-- Configuration (§4.2). M2: only the columns M2 reads. sync_policy,
-- writeback_policy, offload_policy and auth_ref arrive with the pull
-- accounts (M3.5) that need them, together with their kinds.
CREATE TABLE accounts (
    id         text COLLATE "C" PRIMARY KEY
               CHECK (id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    -- The handle in configuration and on the command line. The Cloudflare
    -- account is the one YUUKA_ACCOUNT names; ingest creates its row on
    -- first use (store.EnsureCloudflareAccount), because the process
    -- configuration is what declares it. Same pattern as principals.name.
    name       text NOT NULL UNIQUE CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$'),
    -- Closed list, as credentials.scopes: a new kind needs a migration that
    -- replaces this constraint, and brings the columns it needs with it.
    kind       text NOT NULL CHECK (kind IN ('cf_catchall')),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Converting deliveries.account_key as 0001 prescribed. Every M0/M1 key
-- named the single Cloudflare account, so each distinct key becomes a
-- cf_catchall account. account_access.account_id held keys too (0003);
-- a key that only appears there names an account that has not received
-- mail yet, and is converted the same way.
--
-- The ids are ULIDs generated here, since the database has no ULID
-- function: 48 bits of milliseconds and 80 random bits, Crockford
-- base32. random() is not a CSPRNG; account ids are not secrets, only
-- unique, and a deployment has a handful.
DO $$
DECLARE
    alphabet constant text := '0123456789ABCDEFGHJKMNPQRSTVWXYZ';
    ms bigint := floor(extract(epoch FROM clock_timestamp()) * 1000)::bigint;
    k text;
    new_id text;
BEGIN
    FOR k IN
        SELECT account_key FROM deliveries
        UNION
        SELECT account_id FROM account_access
        ORDER BY 1
    LOOP
        IF k !~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$' THEN
            RAISE EXCEPTION 'account key % is not a valid account name (^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$); rename it in deliveries.account_key and account_access.account_id, and in YUUKA_ACCOUNT, then migrate again', quote_literal(k);
        END IF;
        new_id := '';
        FOR i IN REVERSE 9..0 LOOP
            new_id := new_id || substr(alphabet, ((ms >> (5 * i)) & 31)::int + 1, 1);
        END LOOP;
        FOR i IN 1..16 LOOP
            new_id := new_id || substr(alphabet, floor(random() * 32)::int + 1, 1);
        END LOOP;
        INSERT INTO accounts (id, name, kind) VALUES (new_id, k, 'cf_catchall');
    END LOOP;
END $$;

ALTER TABLE deliveries ADD COLUMN account_id text COLLATE "C" REFERENCES accounts (id);
UPDATE deliveries d SET account_id = a.id FROM accounts a WHERE a.name = d.account_key;
ALTER TABLE deliveries ALTER COLUMN account_id SET NOT NULL;
-- The idempotency key of §10.1 moves with it: unique per account.
ALTER TABLE deliveries DROP CONSTRAINT deliveries_account_key_source_key_key;
ALTER TABLE deliveries ADD CONSTRAINT deliveries_account_id_source_key_key UNIQUE (account_id, source_key);
ALTER TABLE deliveries DROP COLUMN account_key;

UPDATE account_access x SET account_id = a.id FROM accounts a WHERE a.name = x.account_id;
-- Collation "C" like every other id column, which the foreign key wants.
ALTER TABLE account_access
    ALTER COLUMN account_id TYPE text COLLATE "C",
    ADD CONSTRAINT account_access_account_id_fkey FOREIGN KEY (account_id) REFERENCES accounts (id);

-- ---------------------------------------------------------------------------
-- identities

-- Configuration (§4.2): an address yuuka may send as, the transport that
-- carries it (§9.3: routing follows the identity, callers never pick a
-- transport), and its owner (§8.9: the owner decides the hard cap).
CREATE TABLE identities (
    id                 text COLLATE "C" PRIMARY KEY
                       CHECK (id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    -- The account this address belongs to. Replies default to the
    -- identity of the account the mail came in through (§9.3), and sent
    -- mail is visible to whoever may read this account (§13.8).
    account_id         text COLLATE "C" NOT NULL REFERENCES accounts (id),
    -- The address as (zone, prefix), lowercased like deliveries. A NULL
    -- prefix is the catch-all pattern *@zone: any prefix of the zone may be
    -- sent from, which is what a domain-level onboarding allows (§9.3).
    -- An exact identity wins over the pattern of its zone, so a grant on
    -- *@zone never reaches an address someone owns explicitly; grants for
    -- services should be on exact identities (§13.5).
    zone               text NOT NULL CHECK (zone <> ''),
    prefix             text CHECK (prefix <> ''),
    -- Shown in From. It overrides what an SMTP client writes (§13.6), so
    -- a display name cannot impersonate someone else.
    display_name       text NOT NULL DEFAULT '',
    -- Markdown appended to composed mail.
    signature          text NOT NULL DEFAULT '',
    -- §10.2.1: render the text/plain part with Markdown markup stripped
    -- (**bold** -> bold) instead of the Markdown source.
    plain_strip_markup boolean NOT NULL DEFAULT false,
    -- Name of a transport in process configuration. Checked against it
    -- when sending, not here.
    transport          text NOT NULL CHECK (transport ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$'),
    owner_id           text COLLATE "C" NOT NULL REFERENCES principals (id),
    created_at         timestamptz NOT NULL DEFAULT now(),
    -- `yuuka identity rm` disables rather than deletes: drafts, outbox
    -- rows and grants keep pointing at the identity they were made for.
    disabled_at        timestamptz
);

-- One active identity per address, and one pattern per zone.
CREATE UNIQUE INDEX identities_address ON identities (zone, prefix) NULLS NOT DISTINCT
    WHERE disabled_at IS NULL;

-- The foreign key 0003 deferred. identity_grants cannot hold a row that
-- satisfies it (identities did not exist), and nothing has written it, so
-- any row is hand-made and is refused rather than dropped: grants are
-- authorization decisions.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM identity_grants) THEN
        RAISE EXCEPTION 'identity_grants has rows, but identities did not exist before this migration; remove them and grant again with yuuka grant set';
    END IF;
END $$;
ALTER TABLE identity_grants
    ADD CONSTRAINT identity_grants_identity_id_fkey FOREIGN KEY (identity_id) REFERENCES identities (id);

-- ---------------------------------------------------------------------------
-- drafts

-- Decisions (§4.2): mail someone wrote and nobody has sent. Agents write
-- them (create_draft), and every submission that stops short of the
-- outbox lands here with hold_reason set (§8.9: beyond the grant is a
-- downgrade to a draft, never a refusal).
CREATE TABLE drafts (
    id             text COLLATE "C" PRIMARY KEY
                   CHECK (id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    -- Who wrote it: the principal whose grant applies on submission.
    principal_id   text COLLATE "C" NOT NULL REFERENCES principals (id),
    identity_id    text COLLATE "C" NOT NULL REFERENCES identities (id),
    -- The concrete From address; for a *@zone identity it names the prefix.
    from_addr      text NOT NULL,
    -- The message replied to. M2: a message id, not a thread id as in the
    -- §4 sketch: message ids are stable (§4.2), thread ids are not, and the
    -- thread follows from the message.
    in_reply_to_id text COLLATE "C" REFERENCES messages (id),
    -- Envelope recipients by role; bcc is in no header.
    to_addrs       text[] NOT NULL DEFAULT '{}',
    cc_addrs       text[] NOT NULL DEFAULT '{}',
    bcc_addrs      text[] NOT NULL DEFAULT '{}',
    subject        text NOT NULL DEFAULT '',
    -- Exactly one body. Composed mail (WebUI, create_draft) is Markdown,
    -- assembled into MIME on submission (§10.2.1). M2: a message submitted
    -- over SMTP (§13.6) that stops at a draft is already MIME; its
    -- normalized bytes are stored like any outgoing bytes, under sent/
    -- (see outbox.raw_id), so confirming it later sends exactly what the
    -- service submitted. Such a raw becomes a message only once an outbox
    -- row points at it.
    body_md        text,
    raw_id         text COLLATE "C" REFERENCES raw_objects (id),
    -- [{uri, quote}]: inbound content this draft cites (§8.1 anchors),
    -- input to the exfiltration checks of the review tier (§8.9, M3).
    cites          jsonb NOT NULL DEFAULT '[]',
    -- Why a submission stopped here; NULL for a draft never submitted.
    --   policy      send_policy is none or draft
    --   owner       human-owned identity, submitter is not that human (§8.9 cap)
    --   limit       max_per_hour / max_per_day / max_new_recipients_per_day
    --   recipients  outside recipient_scope
    --   suspended   the grant is suspended (circuit breaker)
    --   loop        mail loop rules (§8.9, RFC 3834)
    --   review      the automatic review escalated (M3)
    --   withdrawn   a human recalled it from held (M3)
    -- Closed, so the UI can say it in its own words; hold_detail says the rest.
    hold_reason    text CHECK (hold_reason IN
                       ('policy', 'owner', 'limit', 'recipients', 'suspended', 'loop', 'review', 'withdrawn')),
    hold_detail    text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CHECK (num_nonnulls(body_md, raw_id) = 1)
    -- M2: no attachments and no proposal_id yet. Attachments need an
    -- upload store first; proposals arrive in M3.
);

CREATE INDEX drafts_principal ON drafts (principal_id, updated_at);

-- ---------------------------------------------------------------------------
-- outbox

-- Decisions (§4.2): one row per message committed to being sent, and its
-- place in the §4.5 state machine.
CREATE TABLE outbox (
    id            text COLLATE "C" PRIMARY KEY
                  CHECK (id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    -- §4.5. failed(n) is 'failed' with n = its send_attempts. M2 writes
    -- neither held nor withdrawn; they are admitted now so the review tier
    -- (M3) needs no constraint change. withdrawn is the end of a held
    -- message that went back to a draft (recalled, or escalated by
    -- review); the row stays, with its review and its sent copy.
    state         text NOT NULL CHECK (state IN
                      ('held', 'queued', 'sending', 'sent', 'partial', 'unknown', 'failed', 'dead', 'withdrawn')),
    -- When the executor may next claim it. Set exactly while it waits to be
    -- sent: queued (now, or a backoff), failed (backoff), and partial with
    -- recipients left to retry. Claiming clears it in the same transaction
    -- that inserts the send_attempts row.
    next_retry_at timestamptz,
    identity_id   text COLLATE "C" NOT NULL REFERENCES identities (id),
    -- Header From, as in raw; for a *@zone identity it names the prefix.
    from_addr     text NOT NULL,
    -- Who started it: an agent through submit_draft, a human, or a service
    -- through the submission port (§13.6). The grant checked and the rate
    -- limits counted are those of (identity_id, submitted_by).
    submitted_by  text COLLATE "C" NOT NULL REFERENCES principals (id),
    -- Who let it go: the human who pressed send, or system when a policy
    -- did (§8.8). NULL only while held, or when it never left held.
    approved_by   text COLLATE "C" REFERENCES principals (id),
    -- The review tier (M3): each check's verdict, and the end of the recall
    -- window.
    review        jsonb,
    hold_until    timestamptz,
    -- The draft it came from; NULL for an SMTP submission sent directly.
    draft_id      text COLLATE "C" REFERENCES drafts (id),
    -- The sent copy (§10.2, §4.6): the bytes handed to the transport,
    -- assembled (or normalized, for SMTP submissions) once before the row
    -- is created and stored write-once under sent/YYYY/MM/<id>.eml in the
    -- raw bucket. Every attempt submits these bytes; what recipients see
    -- may differ by headers the platform adds (§10.4.1), which
    -- send_attempts records. sent/ holds every byte string yuuka composed
    -- or normalized for sending, including mail that ends withdrawn, dead
    -- or never sent: whether it went out is this row's state, not where
    -- the bytes live. Not unique: an SMTP submission held as a draft and
    -- confirmed after a recall reuses its raw.
    --
    -- Every outbox raw is parsed into messages like inbound mail, whatever
    -- the state, so replies thread with it and it is searchable; the UI
    -- shows the state next to it. Reparse thus reads only raw_id, which
    -- never changes, and no decision. Message and outbox are linked by
    -- the fold key, not by a stored message id: the message is the one
    -- whose content_hash is raw_objects.content_hash of this raw (§4.1).
    -- So no derived column lives in this decision table, and the link is
    -- as stable as folding.
    raw_id        text COLLATE "C" NOT NULL REFERENCES raw_objects (id),
    -- Local part of Return-Path that DSNs come back to (§10.4.2); NULL
    -- where the transport sets Return-Path itself (Cloudflare).
    verp_tag      text UNIQUE,
    -- The last failure, for people; attempts keep each one.
    error         text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (next_retry_at IS NULL OR state IN ('queued', 'failed', 'partial')),
    CHECK (state NOT IN ('queued', 'failed') OR next_retry_at IS NOT NULL),
    CHECK (state <> 'held' OR hold_until IS NOT NULL),
    CHECK (approved_by IS NOT NULL OR state IN ('held', 'withdrawn'))
);

-- Claiming queued work: due rows, oldest first (FOR UPDATE SKIP LOCKED).
CREATE INDEX outbox_due ON outbox (next_retry_at) WHERE next_retry_at IS NOT NULL;
-- Rate limits of §8.9 count per grant, i.e. per (identity, principal),
-- over the last hour and day.
CREATE INDEX outbox_grant_created ON outbox (identity_id, submitted_by, created_at);
-- Whether a draft is in flight, and its history.
CREATE INDEX outbox_draft ON outbox (draft_id) WHERE draft_id IS NOT NULL;
CREATE INDEX outbox_raw ON outbox (raw_id);

-- ---------------------------------------------------------------------------
-- send_attempts

-- Send facts: one row per attempt to hand an outbox message to a
-- transport, with who runs it and how far it got. Crash recovery reads
-- only this table (§4.5).
CREATE TABLE send_attempts (
    id           text COLLATE "C" PRIMARY KEY
                 CHECK (id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    outbox_id    text COLLATE "C" NOT NULL REFERENCES outbox (id),
    -- The envelope recipients of this attempt, verbatim as in
    -- outbox_recipients.addr; after partial, only the deferred ones.
    recipients   text[] NOT NULL CHECK (cardinality(recipients) > 0),
    -- The configured transport actually used.
    transport    text NOT NULL,
    -- Executor and lease, renewed while it runs.
    owner        text NOT NULL,
    lease_until  timestamptz NOT NULL,
    started_at   timestamptz NOT NULL DEFAULT now(),
    -- Written and committed before the point of no return (§4.5). NULL
    -- proves the remote did not get the body.
    committed_at timestamptz,
    finished_at  timestamptz,
    outcome      text CHECK (outcome IN ('accepted', 'partial', 'rejected', 'failed', 'unknown', 'abandoned')),
    provider_id  text,
    -- The Message-ID recipients see for this attempt: ours when the
    -- transport keeps it, the platform's when it assigns one (§10.4.1);
    -- NULL when it could not be learned. Inbound In-Reply-To/References
    -- are matched against every attempt's id, since a platform assigns a
    -- new one per send.
    message_id   text,
    error        text,
    CHECK ((finished_at IS NULL) = (outcome IS NULL)),
    -- Accepting a body needs the body to have been sent, and unknown
    -- means the commit point was crossed; abandoned means it provably
    -- was not.
    CHECK (outcome NOT IN ('accepted', 'partial', 'unknown') OR committed_at IS NOT NULL),
    CHECK (outcome <> 'abandoned' OR committed_at IS NULL)
);

-- At most one attempt of a message runs at a time, whoever the executor:
-- a second claim of the same outbox row fails here even if the claim
-- logic is wrong.
CREATE UNIQUE INDEX send_attempts_one_open ON send_attempts (outbox_id) WHERE finished_at IS NULL;
-- Lease recovery: unfinished attempts by lease end.
CREATE INDEX send_attempts_lease ON send_attempts (lease_until) WHERE finished_at IS NULL;
CREATE INDEX send_attempts_outbox ON send_attempts (outbox_id, started_at);
-- Threading inbound replies to what we sent.
CREATE INDEX send_attempts_message_id ON send_attempts (message_id) WHERE message_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- outbox_recipients

-- Send facts, per recipient: partial rejections, bounces and complaints
-- land here, not in outbox.state (§4.5).
CREATE TABLE outbox_recipients (
    outbox_id       text COLLATE "C" NOT NULL REFERENCES outbox (id),
    -- Verbatim, as it goes into RCPT TO: a local part is case-sensitive
    -- (RFC 5321 §2.4), so yuuka does not change what the sender wrote.
    addr            text NOT NULL,
    -- addr lowercased by the writer (Go's strings.ToLower, the folding
    -- ingest applies to prefixes), for matching only: deduplication within
    -- a message and the first-contact / new-recipient lookups. Written by
    -- the writer rather than generated with lower(), whose result for
    -- non-ASCII depends on the database's locale and could change on a
    -- restore into another one.
    addr_fold       text NOT NULL,
    kind            text NOT NULL CHECK (kind IN ('to', 'cc', 'bcc')),
    state           text NOT NULL DEFAULT 'pending' CHECK (state IN
                        ('pending', 'accepted', 'rejected', 'deferred', 'unknown', 'bounced', 'complained')),
    last_attempt_id text COLLATE "C" REFERENCES send_attempts (id),
    smtp_code       smallint CHECK (smtp_code BETWEEN 200 AND 599),
    -- The server's or provider's words, enhanced status code included.
    detail          text,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    -- Keyed by the folded address: A@example.net and a@example.net almost
    -- always reach one mailbox, so one message sends them one copy. The
    -- first spelling given wins and is what RCPT TO uses.
    PRIMARY KEY (outbox_id, addr_fold)
);

-- First contact (§8.9) and max_new_recipients_per_day: has this address
-- been sent to before, in any spelling.
CREATE INDEX outbox_recipients_addr_fold ON outbox_recipients (addr_fold);

-- ---------------------------------------------------------------------------
-- changes

-- Drafts and outbox rows change while someone watches: the WebUI shows a
-- send progress, an agent waits for its submission's fate (§8.6). One
-- 'outbox' change covers the row, its attempts and its recipients;
-- target_id is the outbox id.
ALTER TABLE changes DROP CONSTRAINT changes_kind_check;
ALTER TABLE changes ADD CONSTRAINT changes_kind_check
    CHECK (kind IN ('message', 'delivery', 'thread', 'flag', 'prefix', 'proposal', 'draft', 'outbox'));
