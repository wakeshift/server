-- An app whose manifest switches from accounts "wakeshift" to "app"
-- (headless) no longer acts in people's own workspaces: what it had there
-- under Sign in with Wakeshift (grants, subscriptions, events, deliveries,
-- event types, sign-ins) is deleted, as for a deleted app, and so are its
-- groups made before the switch (appapi.FinishAccountSweeps, run by the
-- expiry pass). The row is written in the manifest's transaction
-- (apps.Registry.SetManifestSigned); switching back before the sweep is
-- done removes it. Platform-level, like app_deletions: IDs and times only.
CREATE TABLE app_account_sweeps (
    app_id          text        PRIMARY KEY REFERENCES apps(id),
    switched_at     timestamptz NOT NULL DEFAULT now(),
    attempts        integer     NOT NULL DEFAULT 0,
    last_attempt_at timestamptz,
    last_error      text,
    swept_to        uuid,        -- people's workspaces cleaned so far, in ID order
    done_at         timestamptz
);
CREATE INDEX app_account_sweeps_pending ON app_account_sweeps (switched_at) WHERE done_at IS NULL;
