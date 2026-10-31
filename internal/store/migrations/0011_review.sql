-- M3: the review tier (§8.9). A submission under review is decided
-- synchronously: every deterministic check runs inside the decision, and
-- either one escalates it (a draft with hold_reason review and the
-- verdict below, never an outbox row) or all pass and it becomes an
-- outbox row in held, which the executor promotes to queued once its
-- recall window ends unless someone withdraws it first (§4.5).
--
-- §4.2 categories: everything added here is a decision or a judgement
-- made at a moment from facts that change afterwards (what the
-- submitter could see, which codes had arrived). None of it can be
-- recomputed from raw mail, and reparse touches none of it; the one
-- index on messages serves a lookup and stores nothing.
--
-- 0005 admitted held and withdrawn already and M2 wrote neither, so
-- the constraints added below hold for every existing row and the
-- migration needs nothing beyond `yuuka migrate`.

-- The verdict of the last review that escalated this draft: one entry
-- per check with its outcome and what it found (kind, source message,
-- place; never an extracted value itself). NULL for a draft that was
-- never escalated, and cleared when the draft is edited or held for
-- another reason, like hold_reason: it describes the content submitted
-- then. The verdict of a review that passed lives on the outbox row
-- (outbox.review, 0005), next to the message it let through.
ALTER TABLE drafts ADD COLUMN review jsonb;

-- Who withdrew a held row: a human recall (WebUI, Telegram, CLI), the
-- agent or service withdrawing its own submission, or system when the
-- grant was suspended or the identity disabled before the window ended.
-- The circuit breaker counts only withdrawals by humans, so it is the
-- principal and not a flag. No withdrawn_at: the breaker counts by
-- outbox.created_at like every other window of a grant (§13.5), and the
-- audit log has the moment (outbox.withdraw).
ALTER TABLE outbox ADD COLUMN withdrawn_by text COLLATE "C" REFERENCES principals (id);

-- A held row is a passed review waiting out its window: it has the
-- verdict, nobody has let it go yet, and it has a draft to return to
-- when it is withdrawn (an SMTP submission gets a draft backed by its
-- raw in the same transaction). 0005 already requires hold_until for
-- held and no next_retry_at, which is why ClaimOutbox never sees it.
ALTER TABLE outbox ADD CONSTRAINT outbox_held_shape
    CHECK (state <> 'held' OR (review IS NOT NULL AND draft_id IS NOT NULL AND approved_by IS NULL));

-- withdrawn and withdrawn_by come together, and a withdrawn row always
-- has the draft it went back to. Rows only ever reach withdrawn from
-- held, so approved_by stays NULL there (0005 allows that for withdrawn).
ALTER TABLE outbox ADD CONSTRAINT outbox_withdrawn_shape
    CHECK ((state = 'withdrawn') = (withdrawn_by IS NOT NULL) AND (state <> 'withdrawn' OR draft_id IS NOT NULL));

-- Promotion (store.PromoteHeld) asks for held rows whose window has
-- ended, oldest first, and the executor for the earliest hold_until
-- still ahead. Few rows are ever held at once; the partial index keeps
-- the lookup off the whole outbox.
CREATE INDEX outbox_held ON outbox (hold_until) WHERE state = 'held';

-- The recall window becomes nullable with the meaning the limits have
-- (0003): NULL is "no number of its own", and the default lives in code
-- (sendpolicy.DefaultHoldSeconds) so changing it needs no migration.
-- 0 stays a valid value and means "no window": released on the next
-- promotion pass. The CHECK (hold_seconds >= 0) of 0003 still applies.
ALTER TABLE identity_grants ALTER COLUMN hold_seconds DROP NOT NULL, ALTER COLUMN hold_seconds DROP DEFAULT;

-- 0 was the column default, and M2 held every review submission as a
-- draft whatever the number said, so no 0 stored so far was a choice of
-- "no window" that ever took effect. Taking them as "the default"
-- gives existing review grants the recall window the tier is for; an
-- operator who wants none sets it again (`yuuka grant set
-- --hold-seconds 0`).
UPDATE identity_grants SET hold_seconds = NULL WHERE hold_seconds = 0;

-- The link-domain check (§8.9): is the organizational domain of a link
-- one some authenticated sender wrote from? That is a suffix question
-- on the From domain (example.net matches example.net and
-- mail.example.net), which a btree answers as a prefix range on the
-- reversed domain. text_pattern_ops makes LIKE 'prefix%' use it under
-- any collation. from_addr is stored lowercased (mimeparse); an address
-- with a quoted @ in its local part splits at the wrong @ and simply
-- does not match, which reads as "not seen": the safe side.
CREATE INDEX messages_from_domain_rev ON messages (reverse(split_part(from_addr, '@', 2)) text_pattern_ops);
