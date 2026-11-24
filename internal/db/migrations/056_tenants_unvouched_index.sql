-- migrate:no-transaction
-- An app may make only so many workspaces an hour for guests and topics'
-- conversations without a user (apps.CheckWorkspaceCap counts those made in
-- the last hour). Built CONCURRENTLY so sign-ins aren't held up.
DROP INDEX CONCURRENTLY IF EXISTS tenants_unvouched_created;
CREATE INDEX CONCURRENTLY IF NOT EXISTS tenants_unvouched_created ON tenants (app_id, created_at) WHERE app_id IS NOT NULL AND (starts_with(external_id, 'guest:') OR starts_with(external_id, '~conversation:'));
