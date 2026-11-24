-- Agent-hosted webhook destinations (internal/agenthooks): a conversation
-- whose harness wakes it when a URL it made is POSTed to (Capy's
-- incoming_webhook watch) registers that URL with a registration link from
-- its subscribing result, and Wakeshift POSTs the conversation's
-- deliveries there. One destination per session; registering again
-- replaces it.
--
-- The URL is the credential (anyone holding it wakes the thread), so it's
-- sealed at rest with a key of its own, bound to the workspace and session
-- (mcpevents.Sealer), and never logged.
CREATE TABLE agent_webhooks (
    tenant_id        uuid        NOT NULL REFERENCES tenants(id),
    session_id       uuid        NOT NULL,
    agent_id         text        NOT NULL,
    kind             text        NOT NULL CHECK (kind IN ('capy')),
    url              bytea       NOT NULL, -- sealed
    -- The app whose subscribing result handed out the registration link,
    -- and the grant it was made under: the destination ends with it.
    app_id           text        NOT NULL,
    grant_id         uuid,
    -- dead: the destination answered that it's gone (404, 410) or that the
    -- watch filtered the message out (no-match); nothing more is sent
    -- there until the conversation registers again, and messages wait.
    state            text        NOT NULL DEFAULT 'live' CHECK (state IN ('live', 'dead')),
    dead_reason      text,
    dead_at          timestamptz,
    registered_at    timestamptz NOT NULL DEFAULT now(),
    -- The instance sending to it, and until when: one at a time, in order.
    busy_holder      text,
    busy_until       timestamptz,
    -- Consecutive failed attempts (5xx, timeouts, network errors) and when
    -- the next may go.
    failures         integer     NOT NULL DEFAULT 0,
    next_attempt_at  timestamptz,
    delivered        integer     NOT NULL DEFAULT 0,
    last_delivery_at timestamptz,
    last_status      integer,
    last_error       text, -- a category only: never the endpoint's words or the URL
    PRIMARY KEY (tenant_id, session_id),
    FOREIGN KEY (tenant_id, session_id) REFERENCES sessions (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX agent_webhooks_agent ON agent_webhooks (tenant_id, agent_id);
CREATE INDEX agent_webhooks_grant ON agent_webhooks (tenant_id, grant_id);
ALTER TABLE agent_webhooks ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_webhooks FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON agent_webhooks
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
-- The admin service's policy, with no column granted: it reads nothing here
-- (and never the sealed URL) unless a later migration grants columns.
CREATE POLICY admin_read ON agent_webhooks FOR SELECT TO wakeshift_admin_read USING (true);

-- Which workspaces have a destination, so the deliverer's pass visits only
-- those: platform-level, like mcp_event_targets; IDs only.
CREATE TABLE agent_webhook_targets (
    tenant_id  uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    session_id uuid NOT NULL,
    PRIMARY KEY (tenant_id, session_id)
);
