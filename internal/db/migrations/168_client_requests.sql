-- client_requests: requests the core asks an app's SDK to make of the MCP
-- client during a subscribing call (appapi/clientrequests.go;
-- docs/design/elicit-install.md). The first kind is an elicitation form:
-- Codex's install offer, shown in Codex's own dialog. The SDK reports the
-- user's answer with the request's token (wse_<tenant>_…), which is looked
-- up by its SHA-256, names one session and app, takes one outcome (the first
-- wins) and expires within minutes. Deleted by appapi.CleanUp a day after
-- it expires, with the session, the app (finishAppDeletion) and the
-- workspace.
CREATE TABLE client_requests (
    tenant_id   uuid        NOT NULL REFERENCES tenants(id),
    id          uuid        NOT NULL DEFAULT gen_random_uuid(),
    token_hash  bytea       NOT NULL,
    session_id  uuid        NOT NULL,
    app_id      text        NOT NULL REFERENCES apps(id),
    harness     text        NOT NULL,
    kind        text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    outcome     text,
    outcome_at  timestamptz,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, token_hash),
    FOREIGN KEY (tenant_id, session_id) REFERENCES sessions (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX client_requests_expires ON client_requests (expires_at);
CREATE INDEX client_requests_app ON client_requests (app_id);
ALTER TABLE client_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE client_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON client_requests
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY admin_read ON client_requests FOR SELECT TO wakeshift_admin_read USING (true);
