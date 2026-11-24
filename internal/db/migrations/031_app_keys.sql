-- An app's keys (docs/apps.md): several named keys per app, Stripe-like,
-- each created (shown once), listed and revoked on its own. Wakeshift
-- keeps only each key's public half. An assertion signed by any key that
-- isn't revoked authenticates the app; a revoked key fails at once.
--
-- Platform-level, like apps: no tenant_id and no row-level security. Only
-- the app's owner manages its keys (apps.owner_id, checked in the
-- registry), and the app API reads them only to verify assertions.
CREATE TABLE app_keys (
    app_id       text        NOT NULL REFERENCES apps(id),
    key_id       text        NOT NULL, -- apps.KeyID: a hash of the public key, never the key
    name         text        NOT NULL,
    public_key   bytea       NOT NULL UNIQUE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   text,                 -- user:<id>, or null for WAKESHIFT_APPS and keys from before this table
    -- Updated at most once a minute per key (apps.Registry.Authenticate).
    last_used_at timestamptz,
    revoked_at   timestamptz,
    revoked_by   text,
    PRIMARY KEY (app_id, key_id)
);
CREATE INDEX app_keys_active ON app_keys (app_id) WHERE revoked_at IS NULL;

-- Each app's one key moves here.
INSERT INTO app_keys (app_id, key_id, name, public_key, created_at, last_used_at)
SELECT id, translate(encode(substring(sha256(public_key) FROM 1 FOR 6), 'base64'), '+/', '-_'),
       'Default', public_key, coalesce(key_created_at, created_at), key_used_at
FROM apps WHERE public_key IS NOT NULL;

-- Deleting an app now revokes its publish URLs with its keys; so do apps
-- deleted before.
UPDATE topic_publish_urls SET revoked_at = now()
WHERE revoked_at IS NULL AND app_id IN (SELECT id FROM apps WHERE deleted_at IS NOT NULL);

ALTER TABLE apps DROP COLUMN public_key, DROP COLUMN key_created_at, DROP COLUMN key_used_at;

-- The console's Activity reads an app's workspaces' deliveries newest
-- first.
CREATE INDEX deliveries_created ON deliveries (tenant_id, created_at DESC);
