-- The Codex connector (docs/delivery-findings.md): a hook on the user's
-- machine reads a beacon from a tool result's _meta and redeems its one-time
-- ticket for the thread's stream link. Tickets are single-use and expire
-- within a minute; the ticket names its tenant (wst_<tenant>_…) and is
-- looked up by its hash, like authorization codes.
CREATE TABLE connector_tickets (
    tenant_id   uuid        NOT NULL REFERENCES tenants(id),
    id          uuid        NOT NULL DEFAULT gen_random_uuid(),
    token_hash  bytea       NOT NULL,
    session_id  uuid        NOT NULL,
    app_id      text        NOT NULL REFERENCES apps(id),
    grant_id    uuid        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    redeemed_at timestamptz,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, token_hash),
    FOREIGN KEY (tenant_id, session_id) REFERENCES sessions (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX connector_tickets_session ON connector_tickets (tenant_id, session_id, created_at DESC);
ALTER TABLE connector_tickets ENABLE ROW LEVEL SECURITY;
ALTER TABLE connector_tickets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON connector_tickets
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

-- watching: the session's conversation asked to be woken (watch) and hasn't
-- stopped (unwatch). While it's watching and its connector isn't connected,
-- tool results carry a fresh beacon, so the connector restarts itself.
ALTER TABLE sessions ADD COLUMN watching boolean NOT NULL DEFAULT false;
-- connector_seen_at: when a ticket for this session was last redeemed (the
-- connector is installed and trusted on that machine).
ALTER TABLE sessions ADD COLUMN connector_seen_at timestamptz;
