-- Open challenges by bot (docs/grok-webhooks.md, section 7, "The
-- connector proves"): `wakeshift grok prove` fetches the challenge from
-- POST /v1/stream/proof/challenge by the bot's ID (and, for a register
-- challenge, its ID), instead of anyone copying it from a webhook's body
-- or a tool's result into a command. This finds it across workspaces.
--
-- Platform-level (exempt in TestEveryTenantTableIsIsolated,
-- docs/db-roles.md), like bot_webhooks: a SHA-256 of the bot's ID, IDs and
-- an expiry only. Deleted with its challenge.
CREATE TABLE bot_open_challenges (
    tenant_id    uuid        NOT NULL,
    challenge_id uuid        NOT NULL,
    bot_hash     bytea       NOT NULL,
    purpose      text        NOT NULL CHECK (purpose IN ('register', 'setup')),
    expires_at   timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, challenge_id),
    FOREIGN KEY (tenant_id, challenge_id) REFERENCES bot_challenges (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX bot_open_challenges_bot ON bot_open_challenges (bot_hash, purpose, expires_at);

-- A challenge leaves the index once it's used (proved, reported missing,
-- failed, expired by the pass): whatever path sets used_at.
CREATE FUNCTION bot_open_challenges_used() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM bot_open_challenges WHERE tenant_id = NEW.tenant_id AND challenge_id = NEW.id;
    RETURN NEW;
END
$$;
CREATE TRIGGER bot_challenges_used AFTER UPDATE OF used_at ON bot_challenges
    FOR EACH ROW WHEN (NEW.used_at IS NOT NULL AND OLD.used_at IS NULL) EXECUTE FUNCTION bot_open_challenges_used();
