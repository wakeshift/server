-- Keys waiting on the waitlist (docs/agentic-onboarding.md, "The
-- waitlist"): @wakeshift/cli's init makes a key and sends its public half
-- in a console link. When the person who opens it lands on the waitlist,
-- the page holds that public key here, against their entry, so the app API
-- can answer the key's assertions with 403 waitlisted and init stops with a
-- plain message instead of waiting out its timeout. A pending key is never
-- an app's key and authenticates nothing.
--
-- Platform-level, like waitlist itself: a waitlisted person has no
-- workspace, so it's exempt from row-level security
-- (TestEveryTenantTableIsIsolated, AGENTS.md). Public keys and IDs only.
--
-- kid: the key's kid (appkey.KeyID), unique, so a key is held by one entry
-- at most (the first to hold it). expires_at: a day after it was held (or
-- held again). refused_at: the first time the app API told the key it's
-- waitlisted. Deleted with the entry, when it's registered as an app's key,
-- and once expired (apps.PrunePendingKeys).
CREATE TABLE waitlist_keys (
    kid        text        PRIMARY KEY CHECK (length(kid) = 22),
    entry_id   uuid        NOT NULL REFERENCES waitlist(id) ON DELETE CASCADE,
    public_key bytea       NOT NULL CHECK (length(public_key) = 32),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    refused_at timestamptz
);
CREATE INDEX waitlist_keys_entry ON waitlist_keys (entry_id);
CREATE INDEX waitlist_keys_expires ON waitlist_keys (expires_at);
