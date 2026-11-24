-- Event types apps declare in a workspace (an Agent Chat channel is one:
-- app.agentchat.channel.<name>), so they can be listed with their members.
CREATE TABLE event_types (
    tenant_id   uuid        NOT NULL,
    type        text        NOT NULL,
    app_id      text        NOT NULL REFERENCES apps(id),
    description text        NOT NULL DEFAULT '',
    created_by  text        NOT NULL, -- the agent that declared it
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, type)
);
ALTER TABLE event_types ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_types FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON event_types
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

-- Publish rate limits count an agent's recent posts.
CREATE INDEX events_from ON events (tenant_id, from_kind, from_id, received_at);
