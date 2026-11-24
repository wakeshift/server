-- Sign in with Wakeshift: Wakeshift is the OAuth authorization server for
-- app MCP servers. Clients (Claude Code, Codex…) register, a signed-in user
-- picks which of their agents the client signs in as, and the client gets
-- tokens bound to one app's MCP URL.

-- Apps on the platform (Agent Chat, Sources). Platform-level, not
-- tenant-scoped: every workspace sees the same apps. Milestone 1b's G2 adds
-- manifests, credentials and grants.
CREATE TABLE apps (
    id         text        PRIMARY KEY CHECK (id ~ '^[a-z][a-z0-9-]{1,39}$'),
    name       text        NOT NULL,
    mcp_url    text        NOT NULL UNIQUE, -- the OAuth resource (RFC 8707)
    created_at timestamptz NOT NULL DEFAULT now()
);

-- OAuth clients, registered dynamically (RFC 7591). Platform-level: a client
-- isn't anyone's until a user signs it in to one of their agents.
CREATE TABLE oauth_clients (
    id            text        PRIMARY KEY,
    name          text        NOT NULL,
    redirect_uris text[]      NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz
);

-- Keys that sign access tokens. The private key is encrypted with a key
-- derived from WAKESHIFT_SECRET_KEY; JWKS publishes the public keys of every
-- key that isn't retired.
CREATE TABLE oauth_signing_keys (
    kid         text        PRIMARY KEY,
    public_key  bytea       NOT NULL,
    private_key bytea       NOT NULL, -- AES-GCM sealed
    created_at  timestamptz NOT NULL DEFAULT now(),
    retired_at  timestamptz
);

-- Authorization codes: single-use, short-lived. The code carries its tenant
-- (wsc_<tenant>_<secret>), so it's looked up inside that tenant.
CREATE TABLE oauth_codes (
    tenant_id      uuid        NOT NULL,
    code_hash      bytea       NOT NULL,
    client_id      text        NOT NULL REFERENCES oauth_clients(id),
    user_id        uuid        NOT NULL REFERENCES users(id),
    agent_id       text        NOT NULL,
    redirect_uri   text        NOT NULL,
    code_challenge text        NOT NULL,
    resource       text        NOT NULL,
    -- The refresh-token family this code starts: if the code is presented
    -- twice, that family is revoked (RFC 6749 §4.1.2).
    family_id      uuid        NOT NULL,
    expires_at     timestamptz NOT NULL,
    used_at        timestamptz,
    PRIMARY KEY (tenant_id, code_hash),
    FOREIGN KEY (tenant_id, agent_id) REFERENCES agents (tenant_id, id)
);
ALTER TABLE oauth_codes ENABLE ROW LEVEL SECURITY;
ALTER TABLE oauth_codes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON oauth_codes
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

-- Refresh tokens rotate on every use. Each belongs to a family (one
-- sign-in); presenting a token that was already used means it was copied,
-- so the whole family is revoked.
CREATE TABLE oauth_refresh_tokens (
    tenant_id  uuid        NOT NULL,
    token_hash bytea       NOT NULL,
    family_id  uuid        NOT NULL,
    client_id  text        NOT NULL REFERENCES oauth_clients(id),
    user_id    uuid        NOT NULL REFERENCES users(id),
    agent_id   text        NOT NULL,
    resource   text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    revoked_at timestamptz,
    PRIMARY KEY (tenant_id, token_hash),
    FOREIGN KEY (tenant_id, agent_id) REFERENCES agents (tenant_id, id)
);
CREATE INDEX oauth_refresh_tokens_family ON oauth_refresh_tokens (tenant_id, family_id);
CREATE INDEX oauth_refresh_tokens_agent ON oauth_refresh_tokens (tenant_id, agent_id) WHERE revoked_at IS NULL;
ALTER TABLE oauth_refresh_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE oauth_refresh_tokens FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON oauth_refresh_tokens
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
