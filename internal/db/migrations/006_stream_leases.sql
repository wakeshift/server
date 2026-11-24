-- Who holds each delivery stream (an agent's, or one session's). One
-- connection at a time: a new connection takes the lease (fence + 1) and the
-- old one is told to close. Claiming deliveries checks the lease inside the
-- same transaction, so a displaced connection that missed the notice can't
-- take anything more.
CREATE TABLE stream_leases (
    tenant_id uuid        NOT NULL,
    agent_id  text        NOT NULL,
    target    text        NOT NULL DEFAULT '',
    holder    text        NOT NULL,
    fence     bigint      NOT NULL DEFAULT 1,
    taken_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, agent_id, target),
    FOREIGN KEY (tenant_id, agent_id) REFERENCES agents (tenant_id, id)
);
ALTER TABLE stream_leases ENABLE ROW LEVEL SECURITY;
ALTER TABLE stream_leases FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON stream_leases
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
