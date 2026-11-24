-- Self-serve apps: a signed-in user registers an app in the console and
-- gets a test key at once (docs/apps.md). Apps from WAKESHIFT_APPS have no
-- owner and are live; console apps are owned by the user who made them and,
-- for now, are test apps.
--
-- A test app acts only in test workspaces, and test workspaces are only the
-- ones a test app keeps for its own users (headless): it can never reach
-- anyone's real workspace. Grants refuse to cross modes.
ALTER TABLE apps ADD COLUMN mode text NOT NULL DEFAULT 'live' CHECK (mode IN ('live', 'test'));
ALTER TABLE apps ADD COLUMN owner_id uuid REFERENCES users(id);
ALTER TABLE apps ADD COLUMN key_created_at timestamptz;
-- A deleted app keeps its row (grants, events and workspaces reference it,
-- and its ID isn't reused) but loses its key.
ALTER TABLE apps ADD COLUMN deleted_at timestamptz;
ALTER TABLE apps ADD CONSTRAINT apps_only_owned_deleted CHECK (deleted_at IS NULL OR owner_id IS NOT NULL);
CREATE INDEX apps_owner ON apps (owner_id) WHERE owner_id IS NOT NULL;

-- The MCP URL is the OAuth resource only for live apps; a test app naming
-- a URL can't take it from (or block) a live app.
ALTER TABLE apps DROP CONSTRAINT apps_mcp_url_key;
CREATE UNIQUE INDEX apps_live_mcp_url ON apps (mcp_url) WHERE mode = 'live';

ALTER TABLE tenants ADD COLUMN mode text NOT NULL DEFAULT 'live' CHECK (mode IN ('live', 'test'));
ALTER TABLE tenants ADD CONSTRAINT tenants_test_app_owned CHECK (mode = 'live' OR kind = 'app');
