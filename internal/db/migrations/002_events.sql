-- The event log: every event from a source or an agent, in one per-tenant
-- order (seq).
--
-- seq comes from event_counters, whose row stays locked until the inserting
-- transaction commits. So each tenant's events commit in seq order, and a
-- reader always sees a gap-free prefix: reading "after my cursor" can never
-- skip an event that commits later. (A sequence or identity column doesn't
-- give this: numbers are taken at insert, not at commit.) The price is that
-- one tenant's inserts are serialized, so keep those transactions short.
--
-- Partitioning by time comes later, if retention needs it: a partitioned
-- table can't enforce the idempotency key below across partitions.

CREATE TABLE event_counters (
    tenant_id uuid PRIMARY KEY REFERENCES tenants(id),
    last_seq  bigint NOT NULL
);
ALTER TABLE event_counters ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_counters FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON event_counters
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

CREATE TABLE events (
    tenant_id       uuid        NOT NULL REFERENCES tenants(id),
    seq             bigint      NOT NULL,
    id              uuid        NOT NULL DEFAULT gen_random_uuid(),
    type            text        NOT NULL,
    -- Where it came from: "slack", "agent", "app:agentchat", "hub". With
    -- source_event_id and source_revision it identifies a source event, so
    -- retries are stored once and edits (new revisions) are kept.
    source          text        NOT NULL,
    source_event_id text,
    source_revision text        NOT NULL DEFAULT '',
    occurred_at     timestamptz NOT NULL,
    received_at     timestamptz NOT NULL DEFAULT now(),
    from_kind       text        NOT NULL,
    from_id         text        NOT NULL,
    subject         text        NOT NULL DEFAULT '',
    body            text        NOT NULL DEFAULT '',
    data            jsonb,
    attrs           jsonb       NOT NULL DEFAULT '{}',
    mentions        text[]      NOT NULL DEFAULT '{}',
    -- Lineage: a trace is one causal chain; hop counts agent-to-agent steps.
    trace_id        uuid        NOT NULL,
    parent_id       uuid,
    hop             int         NOT NULL DEFAULT 0,
    -- external: content from outside the user's agents (a Slack message);
    -- internal: from the user's own agents or the hub.
    trust           text        NOT NULL CHECK (trust IN ('external', 'internal')),
    PRIMARY KEY (tenant_id, seq),
    UNIQUE (tenant_id, id)
);
CREATE UNIQUE INDEX events_source_key ON events (tenant_id, source, source_event_id, source_revision)
    WHERE source_event_id IS NOT NULL;
CREATE INDEX events_type ON events (tenant_id, type, seq);
ALTER TABLE events ENABLE ROW LEVEL SECURITY;
ALTER TABLE events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON events
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
