-- migrate:no-transaction
-- CancelStranded finds, for each delivery still waiting for a session, the
-- group routes (live or ended) for that agent and session. Without this the
-- lookup scanned the agent's routes for every waiting delivery; a workspace
-- whose agent joined a game an hour for days had thousands of ended routes,
-- and in production the statement ran for 25 hours. Partial: only group
-- routes. Built CONCURRENTLY, dropping first
-- (TestConcurrentIndexBuildsDropFirst).
SET lock_timeout = '3s';
DROP INDEX CONCURRENTLY IF EXISTS event_subscriptions_route_session;
CREATE INDEX CONCURRENTLY IF NOT EXISTS event_subscriptions_route_session
    ON event_subscriptions (tenant_id, agent_id, session_id) WHERE group_type IS NOT NULL;
