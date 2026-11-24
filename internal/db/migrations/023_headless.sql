-- Headless apps (docs/headless.md): an app identifies its own users and
-- Wakeshift keeps a workspace for each, owned by the app (no members). An
-- app's grant for such an agent has no person behind it: adding the app to
-- the agent was the user's consent (confirmed_by 'app').
ALTER TABLE tenants DROP CONSTRAINT tenants_kind_check;
ALTER TABLE tenants ADD CONSTRAINT tenants_kind_check CHECK (kind IN ('personal', 'team', 'app'));
ALTER TABLE tenants ADD COLUMN app_id text REFERENCES apps(id), ADD COLUMN external_id text;
ALTER TABLE tenants ADD CONSTRAINT tenants_app_owned CHECK ((kind = 'app') = (app_id IS NOT NULL AND external_id IS NOT NULL));
CREATE UNIQUE INDEX tenants_app_external ON tenants (app_id, external_id) WHERE app_id IS NOT NULL;

ALTER TABLE grants ALTER COLUMN granted_by DROP NOT NULL;
ALTER TABLE grants ADD COLUMN confirmed_by text NOT NULL DEFAULT 'user' CHECK (confirmed_by IN ('user', 'app'));
ALTER TABLE grants ADD CONSTRAINT grants_confirmed_by_user CHECK (confirmed_by <> 'user' OR granted_by IS NOT NULL);
