-- Notification channels, their bindings to people, the codes that make a
-- binding, and what each binding is subscribed to (§17, §8.4, §13.7).
-- Telegram was process configuration in M3 (YUUKA_TELEGRAM_*: one bot,
-- one chat, one operator); more than one person and more than one place
-- made it runtime data, and where a summary or a recall button goes is
-- authority, not preference (§13.10).
--
-- §4.2 categories: all four tables are configuration. reparse touches
-- none of them.
--
-- The writers are internal/store/channels.go. The bot token lives in
-- secrets (0017) like a transport's password; the store only persists a
-- secret.Record and never encrypts or decrypts anything.

-- ---------------------------------------------------------------------------
-- notification_channels

-- A channel is one connection: today one Telegram bot (§17.4).
CREATE TABLE notification_channels (
    id          text COLLATE "C" PRIMARY KEY
                CHECK (id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    -- Never changes, like a transport's: actions name channels by it.
    name        text NOT NULL UNIQUE CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$'),
    -- Other kinds (webhook, ntfy) arrive with their own migration (§17.10).
    kind        text NOT NULL CHECK (kind IN ('telegram')),
    -- Who the channel is on the other side. Telegram: the bot's numeric
    -- id, as getMe confirmed it. The bot token's audience is recomputed
    -- from it (telegram:bot<id>, §17.2), so pointing a channel at another
    -- bot needs a token sealed for that bot.
    external_id text NOT NULL CHECK (external_id <> '' AND octet_length(external_id) <= 256),
    -- Not secret, checked per kind in Go. Telegram: the bot's username,
    -- for the t.me link of a bind code.
    settings    jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(settings) = 'object'),
    secret_id   text COLLATE "C" UNIQUE REFERENCES secrets (id),
    -- Removing a channel disables it, erases its secret and revokes its
    -- bindings; the row stays so the audit log keeps resolving.
    disabled_at timestamptz,
    -- Bumped on every write. The channel manager restarts a channel's
    -- poller when it (or its secret's) moves on (§17.5).
    revision    bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at  timestamptz NOT NULL DEFAULT now(),
    -- An enabled channel has its secret; a disabled one has none.
    CHECK ((disabled_at IS NULL) = (secret_id IS NOT NULL))
);

-- getUpdates admits one poller per bot (409 otherwise), so one enabled
-- channel per bot.
CREATE UNIQUE INDEX notification_channels_one_per_bot
    ON notification_channels (kind, external_id) WHERE disabled_at IS NULL;

-- ---------------------------------------------------------------------------
-- channel_bindings

-- One destination on a channel that belongs to a person: a chat, and the
-- user whose button presses count there. Pushes go to people only
-- (§13.8), so principal_id is a human; the writer checks that, a foreign
-- key cannot.
CREATE TABLE channel_bindings (
    id                text COLLATE "C" PRIMARY KEY
                      CHECK (id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    channel_id        text COLLATE "C" NOT NULL REFERENCES notification_channels (id),
    principal_id      text COLLATE "C" NOT NULL REFERENCES principals (id),
    -- Telegram: the chat id (a private chat or a group).
    address           text NOT NULL CHECK (address <> '' AND octet_length(address) <= 256),
    -- Telegram: the user id whose presses count. NULL: no buttons on
    -- this binding.
    actor_external_id text CHECK (actor_external_id <> '' AND octet_length(actor_external_id) <= 256),
    label             text NOT NULL DEFAULT '' CHECK (octet_length(label) <= 256),
    created_at        timestamptz NOT NULL DEFAULT now(),
    -- The last button press that counted.
    last_used_at      timestamptz,
    revoked_at        timestamptz
);

-- A chat belongs to one person on a channel at a time.
CREATE UNIQUE INDEX channel_bindings_one_per_chat
    ON channel_bindings (channel_id, address) WHERE revoked_at IS NULL;
-- A person's bindings, for security alerts (§8.4).
CREATE INDEX channel_bindings_principal ON channel_bindings (principal_id) WHERE revoked_at IS NULL;

-- ---------------------------------------------------------------------------
-- channel_bind_codes

-- The two-sided confirmation of a binding (§13.7): yuuka makes a code
-- for a person, the bot receives /start <code> and records who sent it,
-- and the person confirms that Telegram identity in yuuka. Only then is
-- the binding made. The code is 16 random bytes; only its SHA-256 is
-- stored. It is good for 10 minutes, the /start and the confirmation
-- both within them, and is used once.
CREATE TABLE channel_bind_codes (
    id             text COLLATE "C" PRIMARY KEY
                   CHECK (id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    channel_id     text COLLATE "C" NOT NULL REFERENCES notification_channels (id),
    -- Who the binding will be for: a human, checked by the writer.
    principal_id   text COLLATE "C" NOT NULL REFERENCES principals (id),
    code_hash      bytea NOT NULL UNIQUE CHECK (octet_length(code_hash) = 32),
    -- The re-verified session that asked for it; NULL: the host's CLI.
    created_with   text COLLATE "C" REFERENCES credentials (id),
    created_at     timestamptz NOT NULL DEFAULT now(),
    expires_at     timestamptz NOT NULL,
    -- What Telegram reported with /start <code>, shown at confirmation:
    -- the chat, the user who sent it, their name and the kind of chat
    -- (private, group, supergroup). Recording them grants nothing.
    redeemed_at    timestamptz,
    seen_address   text CHECK (octet_length(seen_address) <= 256),
    seen_actor_id  text CHECK (octet_length(seen_actor_id) <= 256),
    seen_label     text CHECK (octet_length(seen_label) <= 512),
    seen_chat_kind text CHECK (octet_length(seen_chat_kind) <= 32),
    -- Set when the person confirmed.
    binding_id     text COLLATE "C" UNIQUE REFERENCES channel_bindings (id),
    CHECK (expires_at > created_at),
    CHECK ((redeemed_at IS NULL) = (seen_address IS NULL)),
    CHECK ((redeemed_at IS NULL) = (seen_actor_id IS NULL)),
    CHECK (binding_id IS NULL OR redeemed_at IS NOT NULL)
);

-- ---------------------------------------------------------------------------
-- notification_subscriptions

-- Which events a binding receives (§8.4). Only events that have a sender
-- today are listed; send.trouble, proposal.pending and M3.5's
-- account.trouble join the list with the work that sends them. security
-- is not here on purpose: it cannot be subscribed to or away from, and
-- goes to every administrator and the person it is about (§8.4, §13.10).
CREATE TABLE notification_subscriptions (
    binding_id text COLLATE "C" NOT NULL REFERENCES channel_bindings (id),
    event      text NOT NULL CHECK (event IN ('mail.new', 'send.held', 'send.escalated', 'grant.suspended')),
    PRIMARY KEY (binding_id, event)
);
