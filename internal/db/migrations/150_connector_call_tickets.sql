-- Redeem by tool call ID for Claude Code's plugin (docs/harnesses.md,
-- "Redeem by call"), behind WAKESHIFT_REDEEM_BY_TOOL_CALL (off by default). Claude Code tells an MCP server the tool
-- call's ID (_meta["claudecode/toolUseId"]); its synchronous PreToolUse hook
-- gets the same ID before the call goes out, and its PostToolUse hook gets
-- it the moment the result is in. So the plugin claims a marked call for
-- its session's key before the app's server sees the ID, the core binds the
-- call's ticket to that key at issue, and the hook redeems it by the ID,
-- signed by the key, instead of reading the beacon from the transcript.
-- The beacon stays on every result, and nothing here uses its ticket up.
--
-- connector_tickets, per ticket: call_hash, a SHA-256 of the call ID the
-- ticket was minted for, if the harness named one; key_hash and
-- claim_thread, the claiming key's SHA-256 and a SHA-256 of the
-- conversation the claim named, when the ticket is bound to a claim;
-- binding, what issue made of the call (stored, no_claim, app_mismatch, no_tool, thread_mismatch, key_changed; '' for a ticket
-- without a call); redeemed_via, how it was redeemed (call, beacon, late);
-- refused_attempts, how many redeems by call with another key it refused.
ALTER TABLE connector_tickets ADD COLUMN call_hash bytea;
ALTER TABLE connector_tickets ADD COLUMN key_hash bytea;
ALTER TABLE connector_tickets ADD COLUMN claim_thread bytea;
ALTER TABLE connector_tickets ADD COLUMN binding text NOT NULL DEFAULT '';
ALTER TABLE connector_tickets ADD COLUMN redeemed_via text;
ALTER TABLE connector_tickets ADD COLUMN refused_attempts integer NOT NULL DEFAULT 0;
CREATE INDEX connector_tickets_call ON connector_tickets (tenant_id, call_hash) WHERE call_hash IS NOT NULL;

-- connector_call_claims: a plugin's claim of a call, made before any
-- workspace is known (the call hasn't reached the app yet). Platform-level
-- (exempt in TestEveryTenantTableIsIsolated, docs/db-roles.md): SHA-256s of
-- the call ID, the key and the conversation, the app the hook's pin names
-- (if any) and an expiry. A second claim of a call with another key makes it
-- contested, and a contested claim binds nothing (both sides fall back to
-- the beacon): the plugin's claim is async, so someone who saw the call's ID
-- on its way to the app could otherwise get there first. Deleted by
-- appapi.PruneCallClaims an hour after it expires, and with its app.
CREATE TABLE connector_call_claims (
    call_hash   bytea       PRIMARY KEY,
    key_hash    bytea       NOT NULL,
    thread_hash bytea       NOT NULL,
    app_id      text        REFERENCES apps(id) ON DELETE CASCADE,
    contested   boolean     NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL
);
CREATE INDEX connector_call_claims_expires ON connector_call_claims (expires_at);
CREATE INDEX connector_call_claims_app ON connector_call_claims (app_id) WHERE app_id IS NOT NULL;

-- connector_sessions: a Claude Code session's key, registered for its
-- conversation by the plugin when the session starts, before any call
-- (SHA-256s of the conversation ID and the key). The first registration
-- keeps the conversation until it lapses (registeredFor after the last
-- refresh, which every start of the session's waiter makes); a claim for
-- a registered conversation with another key is refused. Platform-level,
-- like the claims, and pruned by appapi.PruneCallClaims once lapsed.
CREATE TABLE connector_sessions (
    thread_hash  bytea       PRIMARY KEY,
    key_hash     bytea       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL
);
CREATE INDEX connector_sessions_expires ON connector_sessions (expires_at);

-- connector_call_index: which workspace a bound ticket is in, so a redeem
-- by call ID finds it before any workspace is known. Platform-level, like
-- the claims: a SHA-256 of the call ID, the workspace, the app and an
-- expiry. One row per call ID; an expired row gives it up. A second
-- workspace or app minting a ticket for the same live call ID makes it
-- ambiguous, and an ambiguous call isn't redeemed by its ID. Deleted with
-- the workspace (eraseWorkspace), with the app (finishAppDeletion), and by
-- appapi.PruneCallClaims an hour after it expires.
CREATE TABLE connector_call_index (
    call_hash  bytea       PRIMARY KEY,
    tenant_id  uuid        NOT NULL,
    app_id     text        NOT NULL,
    ambiguous  boolean     NOT NULL DEFAULT false,
    expires_at timestamptz NOT NULL
);
CREATE INDEX connector_call_index_tenant ON connector_call_index (tenant_id);
CREATE INDEX connector_call_index_app ON connector_call_index (app_id);
CREATE INDEX connector_call_index_expires ON connector_call_index (expires_at);

-- connector_conversation_keys: the key a conversation was first connected
-- by call with (a SHA-256 of the conversation ID and of the key). A claim
-- or redeem by call for that conversation with another key binds nothing:
-- only the key that proved itself holds the conversation.
CREATE TABLE connector_conversation_keys (
    tenant_id    uuid        NOT NULL REFERENCES tenants(id),
    agent_id     text        NOT NULL,
    thread_hash  bytea       NOT NULL,
    key_hash     bytea       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, agent_id, thread_hash)
);
ALTER TABLE connector_conversation_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE connector_conversation_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON connector_conversation_keys
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY admin_read ON connector_conversation_keys FOR SELECT TO wakeshift_admin_read USING (true);
