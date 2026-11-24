-- Stream links are recorded, so they can be revoked and stop working when
-- their creator leaves the workspace. The URL carries only a signed
-- reference to a row here.
CREATE TABLE stream_links (
    tenant_id  uuid        NOT NULL,
    id         uuid        NOT NULL DEFAULT gen_random_uuid(),
    agent_id   text        NOT NULL,
    target     text        NOT NULL DEFAULT '',
    ack        boolean     NOT NULL DEFAULT false,
    created_by text        NOT NULL, -- "user:<id>" (or, later, "app:<id>")
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, agent_id) REFERENCES agents (tenant_id, id)
);
CREATE INDEX stream_links_agent ON stream_links (tenant_id, agent_id) WHERE revoked_at IS NULL;
ALTER TABLE stream_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE stream_links FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON stream_links
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

-- Signing out ends every session of the user: sessions carry the epoch they
-- were issued in, and signing out moves it on.
ALTER TABLE users ADD COLUMN session_epoch int NOT NULL DEFAULT 0;

-- Indexes for queries that run constantly (reaping, releasing, cancelling)
-- and for foreign keys.
CREATE INDEX deliveries_leased ON deliveries (tenant_id, lease_until) WHERE state = 'in_flight';
CREATE INDEX deliveries_holder ON deliveries (tenant_id, lease_holder) WHERE state = 'in_flight';
CREATE INDEX deliveries_subscription ON deliveries (tenant_id, subscription_id);
CREATE INDEX deliveries_event ON deliveries (tenant_id, event_id);
CREATE INDEX identities_user ON identities (user_id);
