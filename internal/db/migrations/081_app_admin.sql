-- What Wakeshift's own admin actions change on an app (docs/admin.md,
-- docs/api-keys.md): suspension, the workspace cap override, and approval
-- for account-mode features. Set only through the core's internal admin API
-- (internal/adminapi), each with its app_log and admin_log entries in the
-- same transaction.

-- Suspended: every key and publish URL refused at once (apps.ErrSuspended),
-- new ones refused in the console; deliveries already made drain; data,
-- grants and subscriptions kept, so unsuspending restores the app as it was.
-- The reason is written for the app's owners, who see it on Overview.
ALTER TABLE apps ADD COLUMN suspended_at timestamptz;
ALTER TABLE apps ADD COLUMN suspended_by text;
ALTER TABLE apps ADD COLUMN suspended_reason text CHECK (length(suspended_reason) <= 500);

-- The workspace cap override: null is the default for the app
-- (apps.WorkspaceCap).
ALTER TABLE apps ADD COLUMN workspace_cap integer CHECK (workspace_cap BETWEEN 0 AND 10000000);

-- Approved for account-mode features (docs/api-keys.md, "Removing modes"):
-- Sign in with Wakeshift, redirect_uris and the workspace scope, grants in
-- people's own workspaces. It replaces mode as what those gates read; mode
-- is still written, for the previous revision. Our own apps and any app
-- approved for live keys start approved, so nothing that works today stops.
ALTER TABLE apps ADD COLUMN accounts boolean NOT NULL DEFAULT false;
UPDATE apps SET accounts = true WHERE mode = 'live' OR live_approved_at IS NOT NULL;

-- An approved app's MCP URL names it as an OAuth resource (apps.ByMCPURL):
-- unique among approved apps that aren't deleted. (apps_live_mcp_url stays
-- until mode is dropped.)
CREATE UNIQUE INDEX apps_accounts_mcp_url ON apps (mcp_url) WHERE accounts AND deleted_at IS NULL;

-- The admin service reads them (080_admin.sql).
GRANT SELECT (suspended_at, suspended_by, suspended_reason, workspace_cap, accounts) ON apps TO wakeshift_admin_read;
