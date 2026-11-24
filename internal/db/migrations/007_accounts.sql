-- People and workspaces.
--
-- A workspace is a tenant: everything else is scoped to one. Each user gets
-- a personal workspace on first sign-in; a team is another workspace with
-- several members. These tables aren't tenant-scoped: sign-in reads them
-- before a workspace is chosen, and they hold who is who and who belongs
-- where, not workspace content.

ALTER TABLE tenants ADD COLUMN kind text NOT NULL DEFAULT 'personal' CHECK (kind IN ('personal', 'team'));

CREATE TABLE users (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    email           text        NOT NULL UNIQUE CHECK (email = lower(email)),
    name            text        NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_sign_in_at timestamptz
);

-- How a user signs in: one row per provider account (Google's "sub").
CREATE TABLE identities (
    provider   text        NOT NULL,
    subject    text        NOT NULL,
    user_id    uuid        NOT NULL REFERENCES users(id),
    email      text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, subject)
);

CREATE TABLE memberships (
    tenant_id  uuid        NOT NULL REFERENCES tenants(id),
    user_id    uuid        NOT NULL REFERENCES users(id),
    role       text        NOT NULL CHECK (role IN ('owner', 'member')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, user_id)
);
CREATE INDEX memberships_user ON memberships (user_id);
