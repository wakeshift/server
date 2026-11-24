-- migrate:no-transaction
-- MCP Events' push delivery (events/stream, docs/mcp-events-push.md): a
-- client that holds a stream has an event subscription of its own, with a
-- session that only its stream reads (destination 'push', made through via
-- 'push', or tool:<name> for a voucher's), as a poll subscription has. The
-- release before this one never writes 'push' and reads it as any
-- destination that isn't a conversation's.
--
-- push_stream_holds counts each app's open push streams across the core's
-- instances (at most 2,000 an app): one row per open stream, its random
-- holder ID, the app's ID and when it last renewed. Platform-level (exempt
-- in TestEveryTenantTableIsIsolated, AGENTS.md): the count is per app over
-- every workspace, so it holds no workspace, user, agent or subscription,
-- only IDs that name nothing in one and a time. Rows are deleted when their
-- stream ends; one a stopped instance left behind stops counting four renewals
-- (at least two minutes) after its last and is deleted by the next open.
--
-- Outside a transaction, so event_subscriptions' ACCESS EXCLUSIVE lock is
-- held only to swap the checks (added NOT VALID, then validated under a
-- lock that lets reads and writes through). Every statement is idempotent:
-- the migrator runs the file again from the top after a lock conflict.
SET lock_timeout = '3s';
ALTER TABLE event_subscriptions DROP CONSTRAINT IF EXISTS event_subscriptions_destination_check;
ALTER TABLE event_subscriptions ADD CONSTRAINT event_subscriptions_destination_check
    CHECK (destination IN ('conversation', 'webhook', 'poll', 'push')) NOT VALID;
ALTER TABLE event_subscriptions VALIDATE CONSTRAINT event_subscriptions_destination_check;
ALTER TABLE event_subscriptions DROP CONSTRAINT IF EXISTS event_subscriptions_via_check;
ALTER TABLE event_subscriptions ADD CONSTRAINT event_subscriptions_via_check
    CHECK (via IN ('subscribe_events', 'native', 'poll', 'push') OR via LIKE 'tool:_%') NOT VALID;
ALTER TABLE event_subscriptions VALIDATE CONSTRAINT event_subscriptions_via_check;
CREATE TABLE IF NOT EXISTS push_stream_holds (
    holder     text        PRIMARY KEY,
    app_id     text        NOT NULL,
    renewed_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS push_stream_holds_app ON push_stream_holds (app_id, renewed_at);
