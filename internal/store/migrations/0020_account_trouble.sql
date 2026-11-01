-- M3.5: the account.trouble event (§8.4, §10.1.3), which 0018 left out
-- until something sent it.
--
-- §4.2 categories of what this changes; reparse touches none of it:
--   configuration   notification_subscriptions gaining account.trouble
--   configuration   account_sources' *_told columns live with it, but
--                   like budget_used they are counters, not configuration:
--                   what the pushes have already said. Writing them does
--                   not move the revision.
--
-- The writer is internal/store/pull.go.

ALTER TABLE notification_subscriptions DROP CONSTRAINT notification_subscriptions_event_check;
ALTER TABLE notification_subscriptions ADD CONSTRAINT notification_subscriptions_event_check
    CHECK (event IN ('mail.new', 'send.held', 'send.escalated', 'grant.suspended', 'account.trouble'));

-- A binding that took every event keeps taking every event: that is what
-- it asked for, and the event exists so that an account does not stop
-- silently. One narrowed by hand stated a preference and is left as it
-- is. The push itself only reaches who may read the account (§13.8).
INSERT INTO notification_subscriptions (binding_id, event)
SELECT binding_id, 'account.trouble' FROM notification_subscriptions GROUP BY binding_id HAVING count(*) = 4;

ALTER TABLE account_sources
    -- The paused_at of the auth_failed pause last told of. A pause told
    -- of is not told of again, across restarts too; a new pause has a
    -- new paused_at. Left NULL here, so a pause older than this migration
    -- gets the push it never had.
    ADD COLUMN refused_told_at  timestamptz,
    -- How many open failures the cursor had passed when last told of,
    -- and when; a smaller count lowers it, so that new failures count.
    ADD COLUMN failures_told    integer NOT NULL DEFAULT 0 CHECK (failures_told >= 0),
    ADD COLUMN failures_told_at timestamptz;
