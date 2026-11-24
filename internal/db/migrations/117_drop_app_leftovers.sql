-- migrate:no-transaction
-- What the apps table and account mode left behind, unread and unwritten
-- since v1.1.1 (docs/organizations.md, docs/headless.md, docs/api-keys.md),
-- so it's safe while v1.3.1, which references none of it, still serves:
--   - apps.owner_id (025) and its index, apps_owner: org_id says who
--     manages an app since 104;
--   - apps.accounts (081) and its unique index, apps_accounts_mcp_url:
--     account mode is off;
--   - apps.live_approved_at and live_approved_by (028): ApproveLive went
--     in v1.1.1;
--   - app_account_sweeps (055), platform-level: every sweep finished by
--     October 26, 2026, and v1.1.1 removed the sweep.
-- The indexes go CONCURRENTLY first, and owner_id's reference to users on
-- its own, so the column drop holds apps' lock only for a moment. Each
-- statement commits on its own and is idempotent: the migrator retries one
-- that hits the 3 s lock_timeout.
SET lock_timeout = '3s';
DROP TABLE IF EXISTS app_account_sweeps;
DROP INDEX CONCURRENTLY IF EXISTS apps_owner;
DROP INDEX CONCURRENTLY IF EXISTS apps_accounts_mcp_url;
ALTER TABLE apps DROP CONSTRAINT IF EXISTS apps_owner_id_fkey;
ALTER TABLE apps DROP COLUMN IF EXISTS owner_id, DROP COLUMN IF EXISTS accounts, DROP COLUMN IF EXISTS live_approved_at, DROP COLUMN IF EXISTS live_approved_by;
