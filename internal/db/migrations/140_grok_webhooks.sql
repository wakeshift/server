-- Grok Bot's Automations webhooks (docs/grok-webhooks.md): a Grok Bot
-- proves a signing key it keeps in a protected file in its own folder
-- (Ed25519; Wakeshift stores only the public key), the user pastes its
-- routine's webhook URL and key on a one-time page, and the webhook is
-- bound only once the run it starts signs a fresh challenge with that key.
-- Wakeshift then POSTs the bot's messages there. Behind
-- WAKESHIFT_GROK_WEBHOOKS.
--
-- Expand only: the previous revision ignores the new columns and tables.
-- It doesn't filter its deliverer by kind, so the flag stays off until
-- this release has replaced it everywhere (a cursor-automations row it
-- claimed would fail CheckURL and be marked dead).

-- A Cursor Automations destination is an agent_webhooks row of its own
-- kind: url holds the sealed {url, key}.
ALTER TABLE agent_webhooks DROP CONSTRAINT agent_webhooks_kind_check;
ALTER TABLE agent_webhooks ADD CONSTRAINT agent_webhooks_kind_check CHECK (kind IN ('capy', 'cursor-automations'));
ALTER TABLE agent_webhooks
    -- The bot's ID (CURSOR_CONVERSATION_ID) at setup: the POST's "bot".
    ADD COLUMN bot_id          text,
    -- SHA-256 of the bot key whose proof bound it.
    ADD COLUMN key_fingerprint bytea,
    -- Copied from another app's destination by a proof (a join), not set
    -- up through the page.
    ADD COLUMN joined          boolean NOT NULL DEFAULT false,
    -- A join not yet announced through the webhook (wakeshift.joined).
    ADD COLUMN join_due        boolean NOT NULL DEFAULT false,
    -- SHA-256 of the removal token (the "Stop wake-ups" page).
    ADD COLUMN removal_hash    bytea,
    -- When the once-only "stopped" line went out in a tool result.
    ADD COLUMN dead_told_at    timestamptz,
    -- When the bot's key was reported missing: the deliverer skips it.
    ADD COLUMN needs_setup_at  timestamptz,
    -- Since when every attempt failed (dead after a day of it).
    ADD COLUMN failing_since   timestamptz,
    -- Pacing: the last POST, and the POSTs in the current hour and day.
    ADD COLUMN last_post_at    timestamptz,
    ADD COLUMN hour_start      timestamptz,
    ADD COLUMN hour_posts      integer NOT NULL DEFAULT 0,
    ADD COLUMN day_start       timestamptz,
    ADD COLUMN day_posts       integer NOT NULL DEFAULT 0;

-- A session's bot key: the public half of the key the bot keeps in its
-- folder, registered by a proof over a register challenge. One per
-- session (each app's workspace has its own session for the bot).
CREATE TABLE bot_keys (
    tenant_id     uuid        NOT NULL REFERENCES tenants(id),
    session_id    uuid        NOT NULL,
    bot_id        text        NOT NULL,
    app_id        text        NOT NULL, -- whose session: deleted with the app's data
    public_key    bytea       NOT NULL CHECK (length(public_key) = 32),
    fingerprint   bytea       NOT NULL CHECK (length(fingerprint) = 32),
    registered_at timestamptz NOT NULL DEFAULT now(),
    replaced_at   timestamptz,
    -- The bot reported its key file missing: the session needs setup.
    missing_at    timestamptz,
    PRIMARY KEY (tenant_id, session_id),
    FOREIGN KEY (tenant_id, session_id) REFERENCES sessions (tenant_id, id) ON DELETE CASCADE
);
ALTER TABLE bot_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE bot_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON bot_keys
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY admin_read ON bot_keys FOR SELECT TO wakeshift_admin_read USING (true);

-- Challenges Wakeshift issued for proofs: register (in a subscribing
-- result) and setup (through the webhook). Only the nonce's SHA-256 is
-- kept; each works once, until expires_at, and three bad signatures end
-- it.
CREATE TABLE bot_challenges (
    tenant_id  uuid        NOT NULL REFERENCES tenants(id),
    id         uuid        NOT NULL,
    session_id uuid        NOT NULL,
    bot_id     text        NOT NULL,
    app_id     text        NOT NULL,
    grant_id   uuid,
    purpose    text        NOT NULL CHECK (purpose IN ('register', 'setup')),
    setup_id   uuid,
    nonce_hash bytea       NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    failures   integer     NOT NULL DEFAULT 0,
    used_at    timestamptz,
    -- Used up by the bot's report that its key file is gone (missing=1).
    missing    boolean     NOT NULL DEFAULT false,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, session_id) REFERENCES sessions (tenant_id, id) ON DELETE CASCADE,
    CHECK ((purpose = 'setup') = (setup_id IS NOT NULL))
);
CREATE INDEX bot_challenges_open ON bot_challenges (tenant_id, session_id, purpose) WHERE used_at IS NULL;
CREATE INDEX bot_challenges_expires ON bot_challenges (tenant_id, expires_at);
ALTER TABLE bot_challenges ENABLE ROW LEVEL SECURITY;
ALTER TABLE bot_challenges FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON bot_challenges
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY admin_read ON bot_challenges FOR SELECT TO wakeshift_admin_read USING (true);

-- One-time setup links: minted once the session has proved a key, with
-- that key fixed (public_key, key_fingerprint) and the bot ID the session
-- recorded. The link is a signature over the row, so it can be shown again
-- without storing it. sealed holds the pasted {url, key} only while an
-- attempt awaits the bot's proof.
CREATE TABLE agent_webhook_setups (
    tenant_id       uuid        NOT NULL REFERENCES tenants(id),
    id              uuid        NOT NULL,
    session_id      uuid        NOT NULL,
    agent_id        text        NOT NULL,
    app_id          text        NOT NULL,
    grant_id        uuid,
    link_id         text        NOT NULL, -- the session's stream link: the setup works only while it does
    bot_id          text        NOT NULL,
    public_key      bytea       NOT NULL CHECK (length(public_key) = 32),
    key_fingerprint bytea       NOT NULL CHECK (length(key_fingerprint) = 32),
    rekey           boolean     NOT NULL DEFAULT false, -- the bind replaces the bot's key
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    claim_id        text,
    claimed_until   timestamptz,
    failures        integer     NOT NULL DEFAULT 0,
    last_error      text, -- a category for the page, never Cursor's words
    sealed          bytea,
    challenge_id    uuid,
    proof_due       timestamptz,
    used_at         timestamptz,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, session_id) REFERENCES sessions (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX agent_webhook_setups_session ON agent_webhook_setups (tenant_id, session_id);
CREATE INDEX agent_webhook_setups_expires ON agent_webhook_setups (tenant_id, expires_at);
ALTER TABLE agent_webhook_setups ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_webhook_setups FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON agent_webhook_setups
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY admin_read ON agent_webhook_setups FOR SELECT TO wakeshift_admin_read USING (true);

-- Which destinations each bot key has, across workspaces: so a proof by
-- the key from another app's session can join the bot's live webhook, a
-- re-setup can find every workspace on it, and the removal page can stop
-- all of them. Platform-level (exempt in TestEveryTenantTableIsIsolated),
-- like agent_webhook_targets: IDs, a public key's fingerprint and a
-- removal token's hash only. Deleted with its destination.
CREATE TABLE bot_webhooks (
    tenant_id    uuid  NOT NULL,
    session_id   uuid  NOT NULL,
    fingerprint  bytea NOT NULL,
    removal_hash bytea NOT NULL,
    PRIMARY KEY (tenant_id, session_id),
    FOREIGN KEY (tenant_id, session_id) REFERENCES agent_webhooks (tenant_id, session_id) ON DELETE CASCADE
);
CREATE INDEX bot_webhooks_fingerprint ON bot_webhooks (fingerprint);
CREATE INDEX bot_webhooks_removal ON bot_webhooks (removal_hash);

-- Which workspaces have open setups or challenges, so the expiry pass
-- visits only those: platform-level, IDs only, like agent_webhook_targets.
CREATE TABLE bot_setup_targets (
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE PRIMARY KEY
);
