-- migrate:no-transaction
-- 092_group_routes.sql built event_subscriptions_group_routes CONCURRENTLY
-- without dropping it first. A concurrent build that fails part-way (its
-- 3 s lock_timeout, waiting for an older transaction) leaves an INVALID
-- index of that name; the migrator's retry then found the name taken and
-- skipped it, so the planner never uses the index and every group copy's
-- route lookup (pipeline.groupRoutes) scans the workspace's event
-- subscriptions. Built again here, dropping whatever is there first, as
-- every CONCURRENTLY build must (TestConcurrentIndexBuildsDropFirst).
-- event_subscriptions is small; between the two statements the lookup
-- only reads it without the index.
SET lock_timeout = '3s';
DROP INDEX CONCURRENTLY IF EXISTS event_subscriptions_group_routes;
CREATE INDEX CONCURRENTLY IF NOT EXISTS event_subscriptions_group_routes
    ON event_subscriptions (tenant_id, agent_id, group_type) WHERE group_type IS NOT NULL AND ended_at IS NULL;
