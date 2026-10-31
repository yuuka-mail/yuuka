-- The identifier of the List-Id header (RFC 2919), what the rule field
-- list.id matches (§8.5): the one way to tell a discussion list's mail
-- from its individual senders. Derived (§4.2): mimeparse reads the
-- header from the canonical raw, rules.ListID reduces it to the part
-- between the angle brackets, and reparse rewrites it like every other
-- content column. The header is not verified; anyone can write it.
--
-- '' when the message has none. NULL means "not read yet": rows written
-- before this migration, which cannot be filled here since that takes
-- parsing the raw. As with 0007, the backlog pass (ingest.ProcessBacklog,
-- run by serve at start and on every reconcile tick, by reconcile and by
-- reparse) rebuilds them from their raw, so an upgrade needs nothing
-- beyond `yuuka migrate`. Rules at ingest always see the column
-- written, since the fold writes it before they run; a retroactive run
-- that reaches an unread row before the backlog pass does reads it as
-- having no List-Id.
ALTER TABLE messages ADD COLUMN list_id text;

-- Empty after the first backlog pass, so the lookup costs nothing.
CREATE INDEX messages_list_id_pending ON messages (id) WHERE list_id IS NULL;
