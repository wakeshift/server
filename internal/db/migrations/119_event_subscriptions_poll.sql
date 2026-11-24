-- migrate:no-transaction
-- MCP Events' poll delivery (events/poll, SEP-3415): a client that polls
-- holds an event subscription of its own, with a session that only its
-- polls read (destination 'poll'), as a webhook has. The release before
-- this one never writes 'poll' and reads it as any destination that isn't
-- a conversation's.
--
-- Outside a transaction, so event_subscriptions' ACCESS EXCLUSIVE lock is
-- held only to swap the check (added NOT VALID, then validated under a
-- lock that lets reads and writes through). Every statement is idempotent:
-- the migrator runs the file again from the top after a lock conflict.
SET lock_timeout = '3s';
ALTER TABLE event_subscriptions DROP CONSTRAINT IF EXISTS event_subscriptions_destination_check;
ALTER TABLE event_subscriptions ADD CONSTRAINT event_subscriptions_destination_check
    CHECK (destination IN ('conversation', 'webhook', 'poll')) NOT VALID;
ALTER TABLE event_subscriptions VALIDATE CONSTRAINT event_subscriptions_destination_check;
