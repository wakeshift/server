-- migrate:no-transaction
-- Modes, release N+2 (docs/api-keys.md, "Removing modes"): nothing has read
-- apps.mode or tenants.mode since release N, nor written them since 093
-- (v1.1.0 inserts apps and tenants without them, so their defaults applied).
-- The columns go, with the index and constraint built on them. One table per
-- statement, outside a transaction, so no statement holds both tables' locks;
-- each is idempotent, as the migrator may run it again.
DROP INDEX CONCURRENTLY IF EXISTS apps_live_mcp_url;
ALTER TABLE apps DROP COLUMN IF EXISTS mode;
ALTER TABLE tenants DROP CONSTRAINT IF EXISTS tenants_test_app_owned;
ALTER TABLE tenants DROP COLUMN IF EXISTS mode;
