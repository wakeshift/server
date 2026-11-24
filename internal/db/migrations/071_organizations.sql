-- Organisations (docs/organizations.md): apps belong to an organisation,
-- not to one user, and its members manage them with a role each (owner,
-- admin or developer). This replaces apps.owner_id over three releases
-- (expand, then contract): this one adds the tables and columns and
-- backfills them (072); the next stops writing owner_id; the one after
-- drops it.
--
-- All four tables are platform-level, like apps: they say who may manage
-- which platform-level app, and they're read before any workspace is
-- chosen, like users and apps. They have no tenant_id and no row-level
-- security (TestEveryTenantTableIsIsolated lists them, as AGENTS.md does).
CREATE TABLE organizations (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text        NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    personal_of uuid        UNIQUE REFERENCES users(id),  -- the one made for a user at sign-up or by the backfill
    created_by  uuid        REFERENCES users(id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    deleted_at  timestamptz
);
CREATE INDEX organizations_created_by ON organizations (created_by) WHERE deleted_at IS NULL;

CREATE TABLE organization_members (
    org_id     uuid        NOT NULL REFERENCES organizations(id),
    user_id    uuid        NOT NULL REFERENCES users(id),
    role       text        NOT NULL CHECK (role IN ('owner', 'admin', 'developer')),
    invited_by uuid        REFERENCES users(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id)
);
CREATE INDEX organization_members_user ON organization_members (user_id);

CREATE TABLE organization_invites (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       uuid        NOT NULL REFERENCES organizations(id),
    email        text        NOT NULL CHECK (email = lower(email)),
    role         text        NOT NULL CHECK (role IN ('owner', 'admin', 'developer')),
    token_sha256 bytea       NOT NULL UNIQUE,   -- never the token
    created_by   uuid        NOT NULL REFERENCES users(id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    accepted_at  timestamptz,
    accepted_by  uuid        REFERENCES users(id),
    revoked_at   timestamptz
);
-- An index can't compare with now(), so an expired invite still holds its
-- slot: inviting the address again revokes it in the same transaction.
CREATE UNIQUE INDEX organization_invites_pending ON organization_invites (org_id, email)
    WHERE accepted_at IS NULL AND revoked_at IS NULL;
-- For the per-address limit on invite emails.
CREATE INDEX organization_invites_email ON organization_invites (email);

-- The organisation's own history, like app_log is the app's. Entries carry
-- invite IDs and masked addresses, never tokens.
CREATE TABLE org_log (
    id     bigserial   PRIMARY KEY,
    org_id uuid        NOT NULL REFERENCES organizations(id),
    at     timestamptz NOT NULL DEFAULT now(),
    actor  text        NOT NULL,   -- user:<id> or hub
    action text        NOT NULL,
    detail jsonb       NOT NULL DEFAULT '{}'
);
CREATE INDEX org_log_by_org ON org_log (org_id, id DESC);
-- Invite emails sent per invite (the per-address limit counts them).
CREATE INDEX org_log_invite ON org_log ((detail->>'invite_id'), at) WHERE detail ? 'invite_id';

ALTER TABLE apps ADD COLUMN org_id uuid REFERENCES organizations(id);
ALTER TABLE apps ADD COLUMN created_by uuid REFERENCES users(id);
CREATE INDEX apps_org ON apps (org_id) WHERE org_id IS NOT NULL;
-- The app's organisation when the entry was written.
ALTER TABLE app_log ADD COLUMN org_id uuid;

-- Who made a publish URL (user:<id> or app:<id>), so removing a member can
-- list the ones they made. Null for ones made before this.
ALTER TABLE topic_publish_urls ADD COLUMN created_by text;
