-- The app log (docs/apps.md): an app's own history, shown to its owner on
-- the app's page, the way Stripe, Vercel and GitHub keep one per app.
-- Apps are platform-level, so this is too: it has no tenant_id and no
-- row-level security. The console reads it only for the app's owner
-- (apps.owner_id), and the app API never reads it.
--
-- kind says what sort of row it is. Today every row is 'audit': something
-- the owner or the app did (created, key rotated, manifest published, key
-- first used, deleted). Request logs and the delivery inspector are meant
-- to add their own kinds here later, so one page can list them together.
CREATE TABLE app_log (
    id bigserial PRIMARY KEY,
    app_id text NOT NULL REFERENCES apps(id),
    at timestamptz NOT NULL DEFAULT now(),
    kind text NOT NULL DEFAULT 'audit' CHECK (kind IN ('audit')),
    actor text NOT NULL,       -- user:<id>, app:<id> or hub
    action text NOT NULL,
    key_id text,               -- the key the entry is about (never the key)
    detail jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX app_log_by_app ON app_log (app_id, id DESC);

-- When the current key was first used, so the log says so once per key.
-- Rotating clears it.
ALTER TABLE apps ADD COLUMN key_used_at timestamptz;

-- Live keys for console apps are approved by hand for now (docs/apps.md).
-- Approval is recorded here; issuing the live key reads it. No UI yet.
ALTER TABLE apps ADD COLUMN live_approved_at timestamptz;
ALTER TABLE apps ADD COLUMN live_approved_by text;

-- What each console app had before the log existed: when it was made.
INSERT INTO app_log (app_id, at, actor, action, detail)
SELECT id, created_at, 'user:' || owner_id::text, 'app.created', jsonb_build_object('name', name, 'mode', mode, 'from_before_the_log', true)
FROM apps WHERE owner_id IS NOT NULL;
