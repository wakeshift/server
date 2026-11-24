-- One routine, one binding (docs/grok-webhooks.md 3.8): a Grok Bot's
-- routine (its webhook URL, for one bot) has exactly one live key and one
-- removal link across every app's destination on it. bot_routines holds
-- the routine's current binding, and its row is the lock that binds,
-- joins and carry-overs take, so a re-setup reaches every destination on
-- the routine, even one in a workspace whose session never held the old
-- key, and a stale join or carry-over can't bring an old link back.
--
-- Platform-level (exempt in TestEveryTenantTableIsIsolated, AGENTS.md),
-- like bot_webhooks: an HMAC of the bot ID and the webhook URL under a
-- core key (never the URL), a removal token's hash and a public key's
-- fingerprint. Rows no destination names are deleted by the expiry pass.
CREATE TABLE bot_routines (
    routine      bytea       PRIMARY KEY,
    removal_hash bytea       NOT NULL,
    fingerprint  bytea       NOT NULL,
    bound_at     timestamptz NOT NULL DEFAULT now()
);

-- Each destination's routine, so a re-setup finds every destination on it.
-- Rows written before this migration have none and are found by key, as
-- before (the feature is off outside development).
ALTER TABLE bot_webhooks ADD COLUMN routine bytea;
CREATE INDEX bot_webhooks_routine ON bot_webhooks (routine);
