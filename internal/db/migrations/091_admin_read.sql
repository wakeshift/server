-- migrate:no-transaction
-- 080_admin.sql's work (the admin service's log, alert cursors, usage
-- rollup and read role; docs/admin.md), done without holding many tables'
-- locks at once. 080 created the admin_read policy on every tenant table in
-- one transaction: each CREATE POLICY takes an ACCESS EXCLUSIVE lock, held
-- to the end, so under production's load it deadlocked and timed out.
--
-- It replaces 080 (db.replaced): a database that hasn't applied 080 runs
-- this in its place and records both; one that has (staging) runs it in its
-- own turn, where everything exists already and it changes nothing. So
-- every statement is idempotent, and it uses nothing newer than 080.
-- Outside a transaction, each statement commits on its own, and the
-- migrator retries one that hits a lock conflict.

-- Wait at most 3 s for a table's lock, so queries queued behind it aren't
-- held up for long; the migrator retries.
SET lock_timeout = '3s';

-- Wakeshift's own admin actions (080): who, what, target, reason, when.
CREATE TABLE IF NOT EXISTS admin_log (
    id          bigserial   PRIMARY KEY,
    at          timestamptz NOT NULL DEFAULT now(),
    actor       text        NOT NULL,   -- the IAP-verified email
    action      text        NOT NULL,   -- app.suspended, app.workspace_cap_set, house.paused, house_transcripts.viewed, …
    target_kind text        NOT NULL,   -- app, org, user, house
    target_id   text        NOT NULL,
    reason      text        CHECK (length(reason) <= 500),
    detail      jsonb       NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS admin_log_target ON admin_log (target_kind, target_id, id DESC);
CREATE INDEX IF NOT EXISTS admin_log_at ON admin_log (at);

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'admin_log_append_only' AND pronamespace = 'public'::regnamespace) THEN
        CREATE FUNCTION admin_log_append_only() RETURNS trigger
            LANGUAGE plpgsql
            AS $f$ BEGIN RAISE EXCEPTION 'admin_log is append-only'; END $f$;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = 'admin_log'::regclass AND tgname = 'admin_log_append_only') THEN
        CREATE TRIGGER admin_log_append_only BEFORE UPDATE OR DELETE ON admin_log
            FOR EACH ROW EXECUTE FUNCTION admin_log_append_only();
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = 'admin_log'::regclass AND tgname = 'admin_log_no_truncate') THEN
        CREATE TRIGGER admin_log_no_truncate BEFORE TRUNCATE ON admin_log
            FOR EACH STATEMENT EXECUTE FUNCTION admin_log_append_only();
    END IF;
END $$;

-- The Slack notifier's cursors (080).
CREATE TABLE IF NOT EXISTS admin_alert_state (
    source  text        PRIMARY KEY,
    last_id bigint,
    last_at timestamptz,
    detail  jsonb       NOT NULL DEFAULT '{}'
);

-- Deliveries finished a day, per workspace (080).
CREATE TABLE IF NOT EXISTS usage_daily (
    day        date      NOT NULL,
    tenant_id  uuid      NOT NULL,
    app_id     text,                 -- tenants.app_id; null for personal workspaces
    delivered  integer   NOT NULL,   -- delivered or acked
    dead       integer   NOT NULL,
    cancelled  integer   NOT NULL,
    age_ms     integer[] NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (day, tenant_id)
);
CREATE INDEX IF NOT EXISTS usage_daily_app ON usage_daily (app_id, day);
CREATE INDEX IF NOT EXISTS usage_daily_tenant ON usage_daily (tenant_id, day DESC);

-- ── The admin service's read role (080, docs/admin.md) ─────────────────
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wakeshift_admin_read') THEN
        CREATE ROLE wakeshift_admin_read NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
    END IF;
EXCEPTION WHEN insufficient_privilege THEN
    RAISE EXCEPTION 'the role that migrates needs CREATEROLE to create wakeshift_admin_read: ALTER ROLE % CREATEROLE (docs/db-roles.md)', current_user;
END $$;
DO $$ BEGIN EXECUTE format('GRANT CONNECT ON DATABASE %I TO wakeshift_admin_read', current_database()); END $$;
GRANT USAGE ON SCHEMA public TO wakeshift_admin_read;

-- The SELECT-only policy on every tenant table, one table per transaction
-- (the COMMIT, allowed in a DO block outside a transaction), so only one
-- table's ACCESS EXCLUSIVE lock is held at a time. A table that has it
-- already is skipped, so a retry carries on where a lock conflict stopped
-- it.
DO $$
DECLARE t regclass;
BEGIN
    FOR t IN SELECT p.polrelid::regclass FROM pg_policy p
             WHERE p.polname = 'tenant_isolation'
               AND NOT EXISTS (SELECT 1 FROM pg_policy a WHERE a.polrelid = p.polrelid AND a.polname = 'admin_read')
             ORDER BY p.polrelid::regclass::text LOOP
        EXECUTE format('CREATE POLICY admin_read ON %s FOR SELECT TO wakeshift_admin_read USING (true)', t);
        COMMIT;
    END LOOP;
END $$;

-- Platform tables (080). GRANT changes only the catalog; it doesn't lock
-- the table, and granting again changes nothing.
GRANT SELECT (id, name, mcp_url, created_at, manifest, manifest_updated_at, mode, owner_id, deleted_at, live_approved_at, live_approved_by, org_id, created_by) ON apps TO wakeshift_admin_read;
GRANT SELECT (app_id, key_id, name, created_at, created_by, last_used_at, revoked_at, revoked_by) ON app_keys TO wakeshift_admin_read;
GRANT SELECT (id, app_id, at, kind, actor, action, key_id, detail, org_id) ON app_log TO wakeshift_admin_read;
GRANT SELECT (id, name, personal_of, created_by, created_at, deleted_at) ON organizations TO wakeshift_admin_read;
GRANT SELECT (org_id, user_id, role, invited_by, created_at) ON organization_members TO wakeshift_admin_read;
GRANT SELECT (id, org_id, at, actor, action, detail) ON org_log TO wakeshift_admin_read;
GRANT SELECT (app_id, requested_at, attempts, last_attempt_at, workspaces_erased, data_deleted_at, log_deleted_at) ON app_deletions TO wakeshift_admin_read;
GRANT SELECT (app_id, switched_at, attempts, done_at) ON app_account_sweeps TO wakeshift_admin_read;
GRANT SELECT (id, kind, app_id, mode, created_at, active_at) ON tenants TO wakeshift_admin_read;
GRANT SELECT (id, email, name, created_at, last_sign_in_at, invited_by) ON users TO wakeshift_admin_read;
GRANT SELECT (tenant_id, user_id, role, created_at) ON memberships TO wakeshift_admin_read;
GRANT SELECT (created_by, uses_left, created_at) ON invites TO wakeshift_admin_read;
GRANT SELECT (app_id, tenant_id, subscription_id) ON topic_members TO wakeshift_admin_read;
GRANT SELECT (app_id, published_at, matched) ON topic_events TO wakeshift_admin_read;
GRANT SELECT (id, app_id, created_at, last_used_at, revoked_at) ON topic_publish_urls TO wakeshift_admin_read;

-- Tenant tables, through admin_read (080).
GRANT SELECT (tenant_id, id, created_at) ON agents TO wakeshift_admin_read;
GRANT SELECT (tenant_id, id, agent_id, harness, connector, created_at, last_active_at, connector_seen_at, watching, app_ids) ON sessions TO wakeshift_admin_read;
GRANT SELECT (tenant_id, id, app_id, kind, agent_id, created_at, expires_at, revoked_at) ON grants TO wakeshift_admin_read;
GRANT SELECT (tenant_id, id, agent_id, state, attempts, created_at, finished_at, connector) ON deliveries TO wakeshift_admin_read;
GRANT SELECT (tenant_id, id, app_id, created_at, expires_at, delivered, last_delivery_at, last_status, retry_attempts, retrying_since, dialect) ON mcp_event_webhooks TO wakeshift_admin_read;

-- Its own tables (080).
GRANT SELECT ON usage_daily TO wakeshift_admin_read;
GRANT SELECT, INSERT ON admin_log TO wakeshift_admin_read;
GRANT USAGE ON SEQUENCE admin_log_id_seq TO wakeshift_admin_read;
GRANT SELECT, INSERT, UPDATE ON admin_alert_state TO wakeshift_admin_read;

-- How many workspaces each app owns, by kind (080).
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_class WHERE relname = 'admin_app_workspaces' AND relnamespace = 'public'::regnamespace) THEN
        CREATE VIEW admin_app_workspaces AS
            SELECT id, app_id, created_at, active_at,
                   CASE WHEN starts_with(external_id, 'guest:') THEN 'guest'
                        WHEN starts_with(external_id, '~conversation:') THEN 'conversation'
                        ELSE 'user' END AS kind
            FROM tenants WHERE kind = 'app' AND app_id IS NOT NULL;
    END IF;
END $$;
GRANT SELECT ON admin_app_workspaces TO wakeshift_admin_read;
