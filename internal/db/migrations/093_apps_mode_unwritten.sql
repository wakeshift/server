-- Modes, release N+1 (docs/api-keys.md, "Removing modes"): nothing reads
-- apps.mode or tenants.mode, and from this release nothing writes them.
-- The previous revision still reads apps.mode during the deploy: to make
-- a new app's workspaces, and to format its keys. 'test' keeps what it does
-- for a new app as it was (wst_test_ keys, test workspaces), and keeps new
-- apps out of apps_live_mcp_url, which is unique among live apps. The next
-- release drops the columns, that index and tenants_test_app_owned.
ALTER TABLE apps ALTER COLUMN mode SET DEFAULT 'test';
