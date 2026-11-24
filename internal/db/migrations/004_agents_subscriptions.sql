-- Agents are durable roles (claude-code, codex, codex-reviewer). Until
-- Sign in with Wakeshift lands, the console creates them and issues static
-- tokens for the core's MCP endpoint.
CREATE TABLE agents (
    tenant_id    uuid        NOT NULL REFERENCES tenants(id),
    id           text        NOT NULL CHECK (id ~ '^[a-z0-9][a-z0-9-]{1,39}$'),
    display_name text        NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);
ALTER TABLE agents ENABLE ROW LEVEL SECURITY;
ALTER TABLE agents FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON agents
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

-- Only a hash of each token is stored; the token itself is shown once.
CREATE TABLE agent_tokens (
    tenant_id    uuid        NOT NULL,
    id           uuid        NOT NULL DEFAULT gen_random_uuid(),
    agent_id     text        NOT NULL,
    token_hash   bytea       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at   timestamptz,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, token_hash),
    FOREIGN KEY (tenant_id, agent_id) REFERENCES agents (tenant_id, id)
);
ALTER TABLE agent_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_tokens FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON agent_tokens
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

-- A subscription delivers events of a type (or type pattern) that pass its
-- filters to an agent. start_seq is where it began: it never receives events
-- from before it existed.
CREATE TABLE subscriptions (
    tenant_id    uuid        NOT NULL,
    id           uuid        NOT NULL DEFAULT gen_random_uuid(),
    agent_id     text        NOT NULL,
    event_type   text        NOT NULL,
    filters      jsonb       NOT NULL DEFAULT '{}',
    status       text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'cancelled')),
    reason       text        NOT NULL DEFAULT '',
    start_seq    bigint      NOT NULL,
    created_by   text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    cancelled_at timestamptz,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, agent_id) REFERENCES agents (tenant_id, id)
);
CREATE INDEX subscriptions_agent ON subscriptions (tenant_id, agent_id) WHERE status = 'active';
ALTER TABLE subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE subscriptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON subscriptions
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
