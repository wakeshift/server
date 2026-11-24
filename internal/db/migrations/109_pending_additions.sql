-- The pending-addition record (docs/subscribe-from-tools.md, "One block per
-- call: the pending addition"; stage 1, additive): one per session and tool
-- call, made by the call's first request that names a session (a deferred
-- subscribe, or /calls/session), updated by its later subscribes, and read
-- once by /calls/addition, which writes the call's one block (one beacon)
-- from it. first is computed before the call's first subscription is made:
-- whether the conversation had subscribed before (so it gets the full
-- instructions once). Expires 10 minutes after it's made; the expiry pass
-- deletes it an hour later, with its ID below. Tenant data: row-level
-- security, and the admin service reads none of its columns.
CREATE TABLE pending_additions (
    tenant_id  uuid        NOT NULL REFERENCES tenants(id),
    id         text        NOT NULL,
    app_id     text        NOT NULL REFERENCES apps(id),
    session_id uuid        NOT NULL,
    agent_id   text        NOT NULL,
    mode       text        NOT NULL CHECK (mode IN ('topic', 'agent')), -- the app's user's conversation, or an app acting for its agent
    first      boolean     NOT NULL,
    armed      boolean     NOT NULL DEFAULT false, -- a subscribe in the call made or renewed a subscription
    notes      text[]      NOT NULL DEFAULT '{}',  -- what the call's subscribes add before the instructions (once, a stale session_key)
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, session_id) REFERENCES sessions (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX pending_additions_expires ON pending_additions (tenant_id, expires_at);
ALTER TABLE pending_additions ENABLE ROW LEVEL SECURITY;
ALTER TABLE pending_additions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pending_additions
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY admin_read ON pending_additions FOR SELECT TO wakeshift_admin_read USING (true);

-- Which workspace a pending-addition record is in, by its random ID: IDs
-- only, platform-level, as topic_subscription_ids is, so /calls/addition
-- finds the record before it knows the workspace. Deleted with its record.
CREATE TABLE pending_addition_ids (
    id        text PRIMARY KEY,
    app_id    text NOT NULL REFERENCES apps(id),
    tenant_id uuid NOT NULL
);
CREATE INDEX pending_addition_ids_tenant ON pending_addition_ids (tenant_id);
CREATE INDEX pending_addition_ids_app ON pending_addition_ids (app_id);

-- reason: why the app subscribed the conversation, as it says it
-- (call.subscribe's reason), shown in the console. NULL if it didn't say.
-- The previous revision neither reads nor writes it.
ALTER TABLE event_subscriptions ADD COLUMN reason text;
