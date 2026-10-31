-- Display names of a draft's recipients (§4, §8.9): "王小明
-- <x@example.com>" as the author wrote it, so a reply keeps the name of
-- the mail it answers and the composed To / Cc carry names like an SMTP
-- submission's do. The writer is internal/store/drafts.go.
--
-- Part of the draft, a decision (§4.2): what its author wrote, which
-- reparse never touches. One name per address, in the same order; ''
-- is none. Names live beside the address columns rather than in them so
-- that everything deciding about recipients (grants, recipient scope,
-- review, dedupe, first contact) keeps reading to_addrs, cc_addrs and
-- bcc_addrs alone: a name is the author's word, shown and written into
-- the header, never a fact any decision rests on.
--
-- outbox needs nothing: To and Cc names are in the sent copy's header,
-- and Bcc names stay on the draft the row came from.
--
-- Existing drafts get no names: `yuuka migrate` is all it takes.
ALTER TABLE drafts
    ADD COLUMN to_names  text[],
    ADD COLUMN cc_names  text[],
    ADD COLUMN bcc_names text[];

UPDATE drafts SET
    to_names  = array_fill(''::text, ARRAY[cardinality(to_addrs)]),
    cc_names  = array_fill(''::text, ARRAY[cardinality(cc_addrs)]),
    bcc_names = array_fill(''::text, ARRAY[cardinality(bcc_addrs)]);

ALTER TABLE drafts
    ALTER COLUMN to_names  SET DEFAULT '{}', ALTER COLUMN to_names  SET NOT NULL,
    ALTER COLUMN cc_names  SET DEFAULT '{}', ALTER COLUMN cc_names  SET NOT NULL,
    ALTER COLUMN bcc_names SET DEFAULT '{}', ALTER COLUMN bcc_names SET NOT NULL,
    ADD CONSTRAINT drafts_recipient_names CHECK (
        cardinality(to_names) = cardinality(to_addrs)
        AND cardinality(cc_names) = cardinality(cc_addrs)
        AND cardinality(bcc_names) = cardinality(bcc_addrs));
