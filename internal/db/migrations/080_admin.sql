-- The admin service's database side (docs/admin.md, "How it reads the
-- database"): its log, its alert cursors, the daily usage rollup, and the
-- read role it logs in with.

-- Wakeshift's own admin actions: who, what, target, reason, when.
-- Platform-level and append-only, like audit_log (016). The core writes it
-- for its internal admin API's actions, in the action's transaction; the
-- admin service writes it for what it does outside the core (transcript
-- views, house controls). detail holds old and new values, never content
-- or secrets.
CREATE TABLE admin_log (
    id          bigserial   PRIMARY KEY,
    at          timestamptz NOT NULL DEFAULT now(),
    actor       text        NOT NULL,   -- the IAP-verified email
    action      text        NOT NULL,   -- app.suspended, app.workspace_cap_set, house.paused, house_transcripts.viewed, …
    target_kind text        NOT NULL,   -- app, org, user, house
    target_id   text        NOT NULL,
    reason      text        CHECK (length(reason) <= 500),
    detail      jsonb       NOT NULL DEFAULT '{}'
);
CREATE INDEX admin_log_target ON admin_log (target_kind, target_id, id DESC);
CREATE INDEX admin_log_at ON admin_log (at);

CREATE FUNCTION admin_log_append_only() RETURNS trigger
    LANGUAGE plpgsql
    AS $$ BEGIN RAISE EXCEPTION 'admin_log is append-only'; END $$;
CREATE TRIGGER admin_log_append_only BEFORE UPDATE OR DELETE ON admin_log
    FOR EACH ROW EXECUTE FUNCTION admin_log_append_only();
CREATE TRIGGER admin_log_no_truncate BEFORE TRUNCATE ON admin_log
    FOR EACH STATEMENT EXECUTE FUNCTION admin_log_append_only();

-- The Slack notifier's cursors, one per source it polls, and the state of
-- its threshold alerts (fired once per threshold per day) in detail.
CREATE TABLE admin_alert_state (
    source  text        PRIMARY KEY,
    last_id bigint,
    last_at timestamptz,
    detail  jsonb       NOT NULL DEFAULT '{}'
);

-- Deliveries finished a day, per workspace: written by the core's expiry
-- pass (deliveries.RollUpUsage) with absolute values, so a repeated pass
-- changes nothing. age_ms is a histogram of finished_at - created_at over
-- #240's buckets (10 ms x 1.5^n, 40 of them, and one for anything longer),
-- so percentiles add up across an app's workspaces. IDs and counts only.
CREATE TABLE usage_daily (
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
CREATE INDEX usage_daily_app ON usage_daily (app_id, day);
-- When each workspace was last rolled up, so the pass does it at most once
-- an hour.
CREATE INDEX usage_daily_tenant ON usage_daily (tenant_id, day DESC);

-- ── The admin service's read role ──────────────────────────────────────
--
-- wakeshift_admin_read can't log in. The admin service's login (an IAM
-- database user on Cloud SQL) is granted it by the core at startup
-- (db.AdminReadLogin). It doesn't bypass row-level security: a second,
-- SELECT-only policy on every tenant table admits it to every workspace's
-- rows, and only for SELECT; writes by any role still face tenant_isolation.
-- What it may read is column grants, never content, secrets or anything a
-- secret can hide in: a table or column added later gets nothing until a
-- migration grants it. Roles are cluster-wide, so it may exist already.
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

DO $$
DECLARE t regclass;
BEGIN
    FOR t IN SELECT p.polrelid::regclass FROM pg_policy p WHERE p.polname = 'tenant_isolation' LOOP
        EXECUTE format('CREATE POLICY admin_read ON %s FOR SELECT TO wakeshift_admin_read USING (true)', t);
    END LOOP;
END $$;

-- Platform tables.
GRANT SELECT (id, name, mcp_url, created_at, manifest, manifest_updated_at, mode, owner_id, deleted_at, live_approved_at, live_approved_by, org_id, created_by) ON apps TO wakeshift_admin_read;
GRANT SELECT (app_id, key_id, name, created_at, created_by, last_used_at, revoked_at, revoked_by) ON app_keys TO wakeshift_admin_read;
GRANT SELECT (id, app_id, at, kind, actor, action, key_id, detail, org_id) ON app_log TO wakeshift_admin_read;
-- Organisations (071): never organization_invites (tokens' hashes and
-- invited addresses).
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

-- Tenant tables, through admin_read: IDs, states, counts and times.
GRANT SELECT (tenant_id, id, created_at) ON agents TO wakeshift_admin_read;
GRANT SELECT (tenant_id, id, agent_id, harness, connector, created_at, last_active_at, connector_seen_at, watching, app_ids) ON sessions TO wakeshift_admin_read;
GRANT SELECT (tenant_id, id, app_id, kind, agent_id, created_at, expires_at, revoked_at) ON grants TO wakeshift_admin_read;
GRANT SELECT (tenant_id, id, agent_id, state, attempts, created_at, finished_at, connector) ON deliveries TO wakeshift_admin_read;
GRANT SELECT (tenant_id, id, app_id, created_at, expires_at, delivered, last_delivery_at, last_status, retry_attempts, retrying_since, dialect) ON mcp_event_webhooks TO wakeshift_admin_read;

-- Its own tables: the rollup to read, its log to append to, its cursors.
GRANT SELECT ON usage_daily TO wakeshift_admin_read;
GRANT SELECT, INSERT ON admin_log TO wakeshift_admin_read;
GRANT USAGE ON SEQUENCE admin_log_id_seq TO wakeshift_admin_read;
GRANT SELECT, INSERT, UPDATE ON admin_alert_state TO wakeshift_admin_read;

-- How many workspaces each app owns, by kind, without their external IDs
-- (an app's ID for a person can be an email): one per user, one per guest
-- sign-in, one per conversation without a user (docs/admin.md, One app).
-- A view runs with its owner's rights, so it reads tenants without the
-- admin role being granted external_id.
CREATE VIEW admin_app_workspaces AS
    SELECT id, app_id, created_at, active_at,
           CASE WHEN starts_with(external_id, 'guest:') THEN 'guest'
                WHEN starts_with(external_id, '~conversation:') THEN 'conversation'
                ELSE 'user' END AS kind
    FROM tenants WHERE kind = 'app' AND app_id IS NOT NULL;
GRANT SELECT ON admin_app_workspaces TO wakeshift_admin_read;
