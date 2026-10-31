-- Agent notes (§8.10): a signed piece of text one principal keeps on a
-- message or a thread, "what this mail is about", "this looks like
-- phishing because ...". The writer and reader are in
-- internal/store/notes.go.
--
-- A decision (§4.2), like drafts and proposals: what a principal wrote,
-- neither derived from raw mail nor an observed fact. It cannot be
-- rebuilt, and reparse never touches it.
--
-- A note is untrusted. It was written after reading mail, and whatever
-- the mail injected may have flowed into it. So it drives nothing:
-- rules cannot match it, and neither the §15 scorer nor the §8.9 review
-- reads it. Nothing in this schema refers to it.
CREATE TABLE notes (
    id         text COLLATE "C" PRIMARY KEY CHECK (id ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    author_id  text COLLATE "C" NOT NULL REFERENCES principals (id),
    -- The target is a message, or the thread of one. A thread note is
    -- anchored on a message, as an annotate proposal of scope thread is
    -- (0009): message ids are stable across reparse (§4.2), thread ids
    -- are not, since rethreading may merge, split and renumber threads.
    -- Which thread a thread note is about is read through its anchor
    -- (messages.thread_id) when it is read, so it follows its message
    -- through a rethread. One per author per thread is the writer's to
    -- keep (a unique index cannot see thread_id): writing a thread note
    -- moves the author's existing note of that thread onto the message
    -- named and drops any other, which only a merge of two threads can
    -- leave behind.
    --
    -- No ON DELETE: like message_state, annotation_log and proposals,
    -- a decision on a message is not dropped with it. Whatever merges
    -- messages away (message_aliases) has to carry decisions to the
    -- survivor first.
    message_id text COLLATE "C" NOT NULL REFERENCES messages (id),
    scope      text NOT NULL CHECK (scope IN ('message', 'thread')),
    -- The note itself, as long as a proposal's rationale may be. Never
    -- empty: deleting the row is how a note is taken back.
    body       text NOT NULL CHECK (length(body) BETWEEN 1 AND 4000),
    -- [{uri, quote}]: yuuka:// anchors and the text they point at (§8.1),
    -- the format of proposals.cites. Required (§12.5): a reader checks a
    -- note against what it cites, and the quote finds the passage again
    -- when an anchor drifts (§4.2).
    cites      jsonb NOT NULL CHECK (jsonb_typeof(cites) = 'array' AND jsonb_array_length(cites) >= 1),
    -- Overwriting keeps the row, its id and created_at; there is no
    -- history. What was overwritten is the author's own earlier text,
    -- and the audit log records the shape of every write, not its text.
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CHECK (updated_at >= created_at)
);
-- One note per author per target. For thread notes this is per anchor;
-- per thread is the writer's (above).
CREATE UNIQUE INDEX notes_once ON notes (author_id, message_id, scope);
-- Reading the notes of a message and of the messages of its thread.
CREATE INDEX notes_message ON notes (message_id);

-- Writing or deleting a note is logged as a change of kind note, its id
-- the target, like a draft or a proposal: a note appearing on old mail
-- is what the change log is for (§8.6), and the reading pane and the
-- notification card follow it. Its visibility is the note's own read
-- (§13.8); what is left of a deleted one, nothing but the id, goes to
-- the viewers that see every note, as for a deleted draft.
ALTER TABLE changes DROP CONSTRAINT changes_kind_check;
ALTER TABLE changes ADD CONSTRAINT changes_kind_check CHECK (kind IN
    ('message', 'delivery', 'thread', 'flag', 'prefix', 'proposal', 'draft', 'outbox', 'rule', 'note'));
