-- migrate:no-transaction
-- One live poll subscription per principal, agent and (source, event,
-- arguments): a poll finds its subscription by them, and two first polls
-- at once can't both make one. Built CONCURRENTLY, dropping first
-- (TestConcurrentIndexBuildsDropFirst).
SET lock_timeout = '3s';
DROP INDEX CONCURRENTLY IF EXISTS event_subscriptions_poll;
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS event_subscriptions_poll ON event_subscriptions
    (tenant_id, source_kind, source_id, principal, agent_id, event, arguments_key) WHERE ended_at IS NULL AND destination = 'poll';
