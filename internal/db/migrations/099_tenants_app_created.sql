-- migrate:no-transaction
-- An app may make only so many workspaces an hour, whoever they're for
-- (apps.CheckWorkspaceCap counts those it made in the last hour); before,
-- only guests' and topics' conversations' counted (tenants_unvouched_created,
-- 056, dropped in a later release once no revision reads it). Built
-- CONCURRENTLY so sign-ins aren't held up.
DROP INDEX CONCURRENTLY IF EXISTS tenants_app_created;
CREATE INDEX CONCURRENTLY IF NOT EXISTS tenants_app_created ON tenants (app_id, created_at) WHERE app_id IS NOT NULL;
