-- session_setups: one-time URLs for a conversation's session-only setup
-- (appapi/sessionsetup.go; docs/harnesses.md, "Session-only setup, sent
-- when it's picked"). Claude Code's install offer asks the user how to be
-- notified, and only if they pick "this session" does the agent fetch the
-- steps (the link file, the Monitor), with curl, from the URL the offer
-- gave. The token names its workspace (wsu_<tenant>_…) and is looked up by
-- its SHA-256, like connector tickets; it names one session, works once,
-- and expires within minutes. Deleted by appapi.CleanUp a day after it
-- expires, with the session, the app (finishAppDeletion) and the workspace.
CREATE TABLE session_setups (
    tenant_id   uuid        NOT NULL REFERENCES tenants(id),
    id          uuid        NOT NULL DEFAULT gen_random_uuid(),
    token_hash  bytea       NOT NULL,
    session_id  uuid        NOT NULL,
    app_id      text        NOT NULL REFERENCES apps(id),
    grant_id    uuid        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    used_at     timestamptz,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, token_hash),
    FOREIGN KEY (tenant_id, session_id) REFERENCES sessions (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX session_setups_expires ON session_setups (expires_at);
CREATE INDEX session_setups_app ON session_setups (app_id);
ALTER TABLE session_setups ENABLE ROW LEVEL SECURITY;
ALTER TABLE session_setups FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON session_setups
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY admin_read ON session_setups FOR SELECT TO wakeshift_admin_read USING (true);
