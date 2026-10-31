-- Search over the display names of To and Cc, and the one definition of
-- the text that search reads (§16.3, §16.4). Step 2 of §16.7: JMAP's
-- to, cc and text conditions match names as well as addresses
-- (RFC 8621 §4.4.1); 0007 stored the addresses only.
--
-- Derived (§4.2): mimeparse reads the names from the canonical raw
-- (Message.ToNames, CcNames) and the content writer stores them next to
-- to_addrs and cc_addrs (internal/store/content.go); reparse rewrites
-- them like every other content column. Nothing here is a decision.
--
-- `yuuka migrate` is all an upgrade takes: the new columns start NULL,
-- "not read yet", and the backlog pass fills them from the raw, as it
-- did for 0007 and 0010. Until a row is filled, to:, cc: and text find
-- it by its addresses only, as before this migration.

-- The decoded display names of To and of Cc, in order of first
-- appearance; empty names and repeats are dropped, so these do not line
-- up with to_addrs and cc_addrs and say nothing about whose name is
-- whose. Only search reads them: a view that shows recipients parses
-- the raw (mimeparse.ParseRecipients), as before. Names are kept as
-- written, not folded: matching is ILIKE, and lower() on non-ASCII
-- depends on the database locale (§4.6).
--
-- NULL means "not read yet": rows written before this migration, which
-- cannot be filled here since that takes parsing the raw. Every write
-- after it stores arrays, '{}' when the field has no names.
ALTER TABLE messages
    ADD COLUMN to_names text[],
    ADD COLUMN cc_names text[],
    -- Written together, so one column's NULL is the pending marker.
    ADD CONSTRAINT messages_names_together CHECK ((to_names IS NULL) = (cc_names IS NULL));

-- What the backlog pass looks up on every reconcile tick
-- (store.UnreadNames). Empty after the first pass, so the lookup costs
-- nothing on a large table.
CREATE INDEX messages_names_pending ON messages (id) WHERE to_names IS NULL;

-- The text that the JMAP text condition searches, and that every
-- string field condition also asks so the trigram index below finds
-- its candidates: subject, the sender's name and address, the To and
-- Cc addresses and names, and the body. The store's expression
-- (searchText in internal/store/filter.go) calls this function with
-- the same arguments in the same order, or the index is not used.
--
-- A function rather than the expression itself because joining an
-- array takes array_to_string, which Postgres marks STABLE (it calls
-- the element type's output function); an index expression must be
-- IMMUTABLE. For text[] the output is the text itself, so the result
-- depends on the arguments alone and IMMUTABLE is true here. If a
-- column of another type ever joins, check that before adding it.
--
-- A NULL array (a row not read yet) counts as empty; the text columns
-- are NOT NULL (0002). Elements are joined with spaces and fields with
-- a space, like the 0002 expression: a phrase may match across a field
-- boundary, as it could before.
--
-- The body is a standard SQL body (RETURN), not a string: it is parsed
-- here, its functions are bound now and recorded as dependencies, so
-- index maintenance does not depend on the search_path of whoever
-- writes the row. PARALLEL SAFE, as every builtin it calls is.
CREATE FUNCTION message_search_text(
    subject text, from_name text, from_addr text,
    to_addrs text[], to_names text[], cc_addrs text[], cc_names text[],
    body_text text)
RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE
RETURN subject || ' ' || from_name || ' ' || from_addr
    || ' ' || coalesce(array_to_string(to_addrs, ' '), '')
    || ' ' || coalesce(array_to_string(to_names, ' '), '')
    || ' ' || coalesce(array_to_string(cc_addrs, ' '), '')
    || ' ' || coalesce(array_to_string(cc_names, ' '), '')
    || ' ' || body_text;

-- The trigram index over it (pg_trgm, 0002), in place of 0002's
-- messages_search, whose expression lacked the recipients. Built here
-- over the rows as they are (names still NULL); the backlog pass's
-- updates keep it current like any other write.
CREATE INDEX messages_search_text ON messages USING gin
    (message_search_text(subject, from_name, from_addr, to_addrs, to_names, cc_addrs, cc_names, body_text) gin_trgm_ops);

-- No query reads the old expression any more; keeping its index would
-- only cost every write to messages.
DROP INDEX messages_search;
