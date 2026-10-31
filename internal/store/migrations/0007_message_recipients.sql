-- The addresses in To, Cc and Reply-To of each message (§4, §8.9), so
-- a thread's correspondents (whom recipient_scope thread admits) can be
-- everyone it was addressed to, not only those who wrote into it; which
-- of these count, by how far the message's headers can be trusted, is
-- the reader's rule (store.ReplyFacts). Derived (§4.2): mimeparse reads
-- them from the canonical raw, and reparse rewrites them like every
-- other content column.
--
-- Only the folded form is stored (store.FoldAddr: Go's lowercasing,
-- computed by the writer, never lower() in SQL, whose result for
-- non-ASCII depends on the database locale; §4.6). The one reader is
-- matching against outbox_recipients.addr_fold, the same folding. The
-- address as written, and the display names, stay in the raw: a view
-- that shows them parses the raw, as the agent view already does, and
-- verbatim addresses without names would serve no display anyway.
-- from_addr is stored folded only too.
--
-- One column per field, not one merged list: what the policy reads is
-- the union, but which field an address came from is what reply-all,
-- "sent to me or copied" and a future recipients view need, and
-- splitting a merged column later would take a reparse.
--
-- Order of first appearance within the field, duplicates (after
-- folding) removed; group syntax contributes its members, and
-- elements that do not parse are skipped (mimeparse.Parse).
--
-- NULL means "not read yet": rows written before this migration. They
-- cannot be filled here, since that takes parsing the raw. The backlog
-- pass (ingest.ProcessBacklog: serve runs it right at start and then on
-- every reconcile tick, `yuuka reconcile` and `yuuka reparse` run it
-- too) re-parses them from their raw, so an upgrade needs nothing beyond
-- `yuuka migrate`. Until a row is filled it adds only its sender to a
-- thread's correspondents, the rule before this migration: the safe side.
-- Every write after this migration stores arrays, '{}' when the field
-- is absent.
ALTER TABLE messages
    ADD COLUMN to_addrs       text[],
    ADD COLUMN cc_addrs       text[],
    ADD COLUMN reply_to_addrs text[],
    -- Written together, so one column's NULL is the pending marker.
    ADD CONSTRAINT messages_recipients_together CHECK (
        (to_addrs IS NULL) = (cc_addrs IS NULL) AND (to_addrs IS NULL) = (reply_to_addrs IS NULL));

-- The backlog pass looks for unread rows on every reconcile tick; after
-- the first pass there are none, and this index stays empty, so the
-- lookup costs nothing on a large table. Thread correspondents need no
-- index: they are read per thread, through messages_thread_id.
CREATE INDEX messages_recipients_pending ON messages (id) WHERE to_addrs IS NULL;
