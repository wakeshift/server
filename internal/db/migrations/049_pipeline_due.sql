-- Which workspaces may have events the pipeline hasn't turned into
-- deliveries, so its sweep visits only those (and every workspace now and
-- then): platform-level, like mcp_event_targets; the ID and the latest
-- event's sequence number only. events.Append marks a workspace; the sweep
-- unmarks it once nothing is behind (pipeline.Runner).
CREATE TABLE pipeline_due (
    tenant_id uuid   PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
    seq       bigint NOT NULL
);
