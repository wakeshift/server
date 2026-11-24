-- Request limits counted in Postgres, so they hold across the core's
-- instances (internal/dblimit; first for Grok Bot's webhook setup and proof
-- routes, docs/grok-webhooks.md 3.1). One row per limit and subject, its
-- window starting at the subject's first counted request.
--
-- Platform-level (exempt in TestEveryTenantTableIsIsolated, AGENTS.md):
-- the routes count before they know any workspace (a client address, a
-- token that may not verify). Subjects are kept only as an HMAC of the
-- limit's name and the subject (an address, a setup link's ID, a user's
-- ID) under a core key, so the table holds no address or ID, only a hash,
-- a time and a count. Rows past their window are deleted by the core's
-- expiry pass.
CREATE TABLE rate_counts (
    key        bytea       PRIMARY KEY,
    started_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    n          integer     NOT NULL
);
CREATE INDEX rate_counts_expires ON rate_counts (expires_at);
