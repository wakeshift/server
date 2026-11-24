-- MCP Events webhook delivery (docs/connectors.md, "ChatGPT"): a client of
-- an app's MCP server (ChatGPT) subscribes to the app's wakeshift.message
-- event for one conversation (its session), with a callback URL and a
-- Standard Webhooks signing secret; Wakeshift POSTs that session's
-- deliveries to the callback, signed, and counts a 2xx as the ack.
--
-- One live webhook per session (the newest subscription replaces older
-- ones, as the newest stream does). Its ID is derived from the
-- subscription's identity (the workspace, session, callback URL and event
-- name), so subscribing again refreshes it. Secrets are sealed with a key
-- derived from the server's secret key.
CREATE TABLE mcp_event_webhooks (
    tenant_id        uuid        NOT NULL REFERENCES tenants(id),
    id               text        NOT NULL, -- sub_<hex>, sent as X-MCP-Subscription-Id
    app_id           text        NOT NULL REFERENCES apps(id),
    agent_id         text        NOT NULL,
    session_id       uuid        NOT NULL,
    session_handle   text        NOT NULL, -- what the conversation passes as wakeshift_session
    name             text        NOT NULL,
    url              text        NOT NULL,
    secret           bytea       NOT NULL,
    -- After a refresh with a new secret, deliveries are signed with both
    -- until previous_until.
    previous_secret  bytea,
    previous_until   timestamptz,
    expires_at       timestamptz NOT NULL, -- refreshBefore: ends unless refreshed by then
    created_at       timestamptz NOT NULL DEFAULT now(),
    refreshed_at     timestamptz NOT NULL DEFAULT now(),
    delivered        integer     NOT NULL DEFAULT 0,
    last_delivery_at timestamptz,
    last_status      integer,
    last_error       text,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, session_id) REFERENCES sessions (tenant_id, id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX mcp_event_webhooks_session ON mcp_event_webhooks (tenant_id, session_id);
ALTER TABLE mcp_event_webhooks ENABLE ROW LEVEL SECURITY;
ALTER TABLE mcp_event_webhooks FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON mcp_event_webhooks
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

-- Which workspaces have a webhook, so the deliverer visits only those:
-- platform-level, like topic_members; IDs and the expiry only.
CREATE TABLE mcp_event_targets (
    tenant_id  uuid        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    webhook_id text        NOT NULL,
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, webhook_id)
);
CREATE INDEX mcp_event_targets_live ON mcp_event_targets (expires_at);
