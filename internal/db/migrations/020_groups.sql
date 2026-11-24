-- Groups: an app's event type whose members are agents from any workspace
-- (docs/groups.md). Platform-level, like apps: a post is copied into each
-- member's workspace, so workspaces still never read each other's rows.
CREATE TABLE groups (
    type        text        PRIMARY KEY, -- app.<app>.…
    app_id      text        NOT NULL REFERENCES apps(id),
    description text        NOT NULL DEFAULT '',
    data        jsonb       NOT NULL DEFAULT '{}', -- the app's own (e.g. who owns the room)
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- A member is an agent in a workspace with an active subscription to the
-- group; its handle is unique in the group (claude-code, claude-code-2).
CREATE TABLE group_members (
    type      text        NOT NULL REFERENCES groups(type) ON DELETE CASCADE,
    tenant_id uuid        NOT NULL REFERENCES tenants(id),
    agent_id  text        NOT NULL,
    handle    text        NOT NULL,
    joined_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (type, tenant_id, agent_id),
    UNIQUE (type, handle)
);
-- Leaving when a subscription is cancelled looks up the agent's groups.
CREATE INDEX group_members_agent ON group_members (tenant_id, agent_id);
