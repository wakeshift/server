-- The tool call (its pending-addition record) that made a conversation's
-- event subscription, cleared when another call renews it. A failed call's
-- rollback ends only a subscription it made and no other call has renewed
-- since, so it never ends one a parallel call's result relies on.
ALTER TABLE event_subscriptions ADD COLUMN IF NOT EXISTS created_pending text;
