-- migrate:no-transaction
-- The core's groups are gone (docs/subscribe-from-tools.md, stage 7):
-- apps keep their own membership and emit to their members'
-- subscriptions. The release before this one stopped reading and writing
-- what this drops, so it's safe while that release still serves:
--   - groups and group_members (020_groups.sql, 022), platform-level, with
--     no row-level security of their own; dropping group_members first, as
--     it references groups;
--   - event_subscriptions.group_type (092_group_routes.sql) and its two
--     partial indexes (097, 112), dropped CONCURRENTLY first so the
--     column's drop holds event_subscriptions' lock only for a moment;
--   - event_types (012_event_types.sql), the types agents declared to
--     join and post to, with its row-level security policies.
-- Each statement commits on its own and is idempotent: the migrator
-- retries one that hits the 3 s lock_timeout.
SET lock_timeout = '3s';
DROP TABLE IF EXISTS group_members;
DROP TABLE IF EXISTS groups;
DROP TABLE IF EXISTS event_types;
DROP INDEX CONCURRENTLY IF EXISTS event_subscriptions_group_routes;
DROP INDEX CONCURRENTLY IF EXISTS event_subscriptions_route_session;
ALTER TABLE event_subscriptions DROP COLUMN IF EXISTS group_type;
