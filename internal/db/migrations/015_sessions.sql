-- Sessions: an agent is a durable role (claude-code, codex); a session is
-- one conversation of it and a participant in its own right, with a handle
-- (codex#2, claude-code@pipe), its own deliveries (the outbox target),
-- presence while its stream is connected, and authorship of what it posts.
CREATE TABLE sessions (
    tenant_id      uuid        NOT NULL,
    id             uuid        NOT NULL DEFAULT gen_random_uuid(),
    agent_id       text        NOT NULL,
    handle         text        NOT NULL, -- agent#n or agent@label
    label          text        NOT NULL DEFAULT '',
    -- stream: it holds its own stream link; connector: delivered on the
    -- agent's connector stream (e.g. Wakeshift for Mac into a Codex thread).
    runtime        text        NOT NULL DEFAULT 'stream' CHECK (runtime IN ('stream', 'connector')),
    conversation   text,       -- the harness's conversation ID, if known
    created_by     text        NOT NULL, -- "app:<id>" or "user:<id>"
    created_at     timestamptz NOT NULL DEFAULT now(),
    last_active_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, handle),
    FOREIGN KEY (tenant_id, agent_id) REFERENCES agents (tenant_id, id)
);
CREATE UNIQUE INDEX sessions_conversation ON sessions (tenant_id, agent_id, conversation) WHERE conversation IS NOT NULL;
CREATE INDEX sessions_agent ON sessions (tenant_id, agent_id, last_active_at DESC);
ALTER TABLE sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON sessions
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

-- Presence: a stream's holder renews this while connected and clears it on
-- disconnect; a session is present while its stream's renewal is fresh.
ALTER TABLE stream_leases ADD COLUMN renewed_at timestamptz;

-- Who posted (which session), the session a subscription follows (the one
-- that joined), and, for connector sessions, which session a delivery on
-- the agent's stream is for.
ALTER TABLE events ADD COLUMN from_session uuid, ADD COLUMN from_handle text NOT NULL DEFAULT '';
ALTER TABLE subscriptions ADD COLUMN session_id uuid;
ALTER TABLE deliveries ADD COLUMN deliver_to uuid;
