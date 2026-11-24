-- Apps are registered at deploy time with a public key (they sign client
-- assertions with the private half) and publish a manifest of what they may
-- do: events agents may be woken by and may post, and events the app may
-- publish into a workspace.
ALTER TABLE apps ADD COLUMN public_key bytea;
ALTER TABLE apps ADD COLUMN description text NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN manifest jsonb NOT NULL DEFAULT '{}';
ALTER TABLE apps ADD COLUMN manifest_updated_at timestamptz;

-- A grant is a user's consent, given once: either for an app to act for
-- one agent (kind 'agent'), or for an app to publish events into the
-- workspace (kind 'workspace', e.g. Sources connecting Slack). scope is
-- what was consented to; shown is the consent screen's text (risks, dry
-- run) as the user saw it.
CREATE TABLE grants (
    tenant_id  uuid        NOT NULL,
    id         uuid        NOT NULL DEFAULT gen_random_uuid(),
    app_id     text        NOT NULL REFERENCES apps(id),
    kind       text        NOT NULL CHECK (kind IN ('agent', 'workspace')),
    agent_id   text,
    scope      jsonb       NOT NULL,
    shown      jsonb       NOT NULL DEFAULT '{}',
    granted_by uuid        NOT NULL REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, agent_id) REFERENCES agents (tenant_id, id),
    CHECK ((kind = 'agent') = (agent_id IS NOT NULL))
);
-- One live grant per app and agent (or per app, for the workspace).
CREATE UNIQUE INDEX grants_live_agent ON grants (tenant_id, app_id, agent_id) WHERE kind = 'agent' AND revoked_at IS NULL;
CREATE UNIQUE INDEX grants_live_workspace ON grants (tenant_id, app_id) WHERE kind = 'workspace' AND revoked_at IS NULL;
ALTER TABLE grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE grants FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON grants
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

-- What a grant created, so revoking it undoes them.
ALTER TABLE subscriptions ADD COLUMN grant_id uuid;
ALTER TABLE stream_links ADD COLUMN grant_id uuid;
CREATE INDEX subscriptions_grant ON subscriptions (tenant_id, grant_id) WHERE grant_id IS NOT NULL;
CREATE INDEX stream_links_grant ON stream_links (tenant_id, grant_id) WHERE grant_id IS NOT NULL;
