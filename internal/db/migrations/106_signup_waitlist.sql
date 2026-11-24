-- Sign-ups (docs/organizations.md, "Sign-up and invites"): a cap on accounts, set at runtime from the
-- admin service, beyond which a new sign-in joins the waitlist instead of
-- getting an account. Both tables are platform-level, read before the
-- person has any workspace, like users and identities, so they're exempt
-- from row-level security (TestEveryTenantTableIsIsolated, AGENTS.md).

-- The cap: one row. Accounts (users) at or past it waitlist new sign-ins;
-- invite links, organisation invites and people invited off the waitlist
-- still get accounts, and count towards it. It starts at today's accounts
-- plus 50.
CREATE TABLE signup_cap (
    id         boolean     PRIMARY KEY DEFAULT true CHECK (id),
    cap        integer     NOT NULL CHECK (cap >= 0),
    updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO signup_cap (cap) SELECT count(*) + 50 FROM users;

-- The waitlist: one entry per person (their sign-in identity, and their
-- verified email, each once), with an optional note on what they'll build.
-- Inviting an entry admits its next sign-in; the account it makes is
-- recorded, and the entry deleted 30 days later (accounts.PruneWaitlist).
-- Removing one deletes it.
CREATE TABLE waitlist (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    provider     text        NOT NULL,
    subject      text        NOT NULL,
    email        text        NOT NULL CHECK (email = lower(email)),
    name         text        NOT NULL DEFAULT '',
    note         text        CHECK (length(note) <= 500),
    created_at   timestamptz NOT NULL DEFAULT now(),
    noted_at     timestamptz,
    invited_at   timestamptz,
    invited_by   text,
    emailed_at   timestamptz,
    signed_up_at timestamptz,
    user_id      uuid        REFERENCES users(id) ON DELETE SET NULL,
    UNIQUE (provider, subject),
    UNIQUE (email)
);
CREATE INDEX waitlist_created ON waitlist (created_at, id);

-- The admin service reads both (its sign-ups page and Slack), never the
-- provider's subject; and which provider each account signs in with.
GRANT SELECT (cap, updated_at) ON signup_cap TO wakeshift_admin_read;
GRANT SELECT (id, provider, email, name, note, created_at, noted_at, invited_at, invited_by, emailed_at, signed_up_at, user_id) ON waitlist TO wakeshift_admin_read;
GRANT SELECT (user_id, provider, created_at) ON identities TO wakeshift_admin_read;
