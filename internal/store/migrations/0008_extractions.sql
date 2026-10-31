-- What the extractor (internal/extract) found in each message: one-time
-- codes, sign-in and verification links, how long they last (§3
-- principle 4). The notification zone shows them at a glance (§12.9);
-- the exfiltration check of the review tier (§8.9) will look for them in
-- outgoing mail.
--
-- Derived (§4.2), like the messages columns: a pure function of the
-- canonical raw (through mimeparse) and messages.received_at. Fold and
-- reparse rewrite a message's rows in the same transaction as its other
-- content columns; nothing else writes here, and no decision is kept
-- here. "This code was used", "dismissed", "this link is fine" are a
-- person's and belong with message_state, which reparse never touches.
--
-- The values were written by the sender: they are data, never
-- instructions, and a link is never opened or fetched by yuuka.
CREATE TABLE extractions (
    -- A merged-away message (message_aliases) takes its derived rows
    -- with it; the survivor gets its own from its next rebuild.
    message_id  text COLLATE "C" NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    -- The extractor's order within the message: codes, then links, best
    -- first, then the expiry. Readers that want "the code" take the
    -- lowest ord of kind otp.
    ord         smallint NOT NULL CHECK (ord >= 0),
    -- Closed list. amount is reserved: nothing extracts it yet, and
    -- naming it now spares a migration when something does.
    kind        text NOT NULL CHECK (kind IN ('otp', 'link', 'expiry', 'amount')),
    -- otp: the code as typed, grouping spaces and hyphens removed;
    -- link: the URL as written; expiry: an ISO 8601 duration (PT10M).
    -- Bounded so a hostile href cannot grow a row without limit
    -- (mimeparse keeps hrefs to 4096 bytes).
    value       text NOT NULL CHECK (value <> '' AND octet_length(value) <= 8192),
    -- 1..100, how sure the matcher is. The extractor drops anything
    -- under its own threshold; the column lets readers ask for more.
    confidence  smallint NOT NULL CHECK (confidence BETWEEN 1 AND 100),
    -- Where it was found: subject, body text, or the target of an
    -- <a href> in an HTML part. No offsets: the reading pane finds a
    -- value in the text it renders, and offsets into body_text would
    -- not match the structured view's blocks anyway. Adding them later
    -- is a column and an extract.Version bump, not a reparse.
    source      text NOT NULL CHECK (source IN ('subject', 'body', 'html')),
    -- kind expiry only: when the codes and links of the message stop
    -- working, counted from the earlier of the Date header and
    -- received_at (a forged Date can shorten, never stretch, it).
    -- Whether that moment has passed is the reader's question, asked
    -- against now(); it is never stored.
    expires_at  timestamptz,
    PRIMARY KEY (message_id, ord),
    CHECK ((kind = 'expiry') = (expires_at IS NOT NULL))
);

-- The exfiltration check asks "does this token of an outgoing body
-- appear as an extracted value": equality on value, never range or
-- prefix. A hash index answers that and, unlike a btree, takes values
-- of any length (a btree entry is limited to about a third of a page,
-- and URLs with tracking parameters get long). The check is expected to
-- tokenize the outgoing body the way the extractor does (digit runs
-- with grouping removed, URLs) and to join messages for recency and
-- deliveries for "came in from outside", so that values only ever seen
-- in our own sent copies do not count.
CREATE INDEX extractions_value ON extractions USING hash (value);

-- "The latest codes" (an agent asking for the newest code to a prefix, a
-- feed of codes) walks one kind newest first. Message ids are ULIDs of
-- their arrival time, so id order is arrival order without a join; the
-- notification list itself reads extractions by message_id through the
-- primary key.
CREATE INDEX extractions_kind_message ON extractions (kind, message_id DESC);

-- Which extractor version wrote a message's rows; 0 for messages stored
-- before this migration, which therefore have none yet. The backlog pass
-- (ingest.ProcessBacklog: serve runs it at start and on every reconcile
-- tick; reconcile and reparse run it too) re-extracts every message
-- below extract.Version from its raw, so this migration needs nothing
-- beyond `yuuka migrate`, and a better extractor reaches old mail the
-- same way without a reparse. It rewrites only extractions and this
-- column, never the other content columns, and logs a message change
-- only when the rows differ.
--
-- On the messages row, not on the extraction rows: a message with
-- nothing to extract has no row to carry it. NOT NULL DEFAULT 0 rather
-- than NULL for "never": the pending lookup is then one range scan
-- (extract_version < current), and adding a column with a constant
-- default rewrites no table.
ALTER TABLE messages ADD COLUMN extract_version smallint NOT NULL DEFAULT 0 CHECK (extract_version >= 0);

-- After the first pass every message is current and the scan for older
-- ones finds nothing at the start of the index.
CREATE INDEX messages_extract_version ON messages (extract_version);
