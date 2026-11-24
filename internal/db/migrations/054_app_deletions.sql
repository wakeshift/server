-- Deleting an app deletes what Wakeshift keeps for its users within 30 days
-- (appapi.FinishAppDeletions, run by the expiry pass): the workspaces the
-- app kept for them (guests' included) and everything in them; in other
-- workspaces, its grants, subscriptions, events, deliveries, event types,
-- webhooks and sign-ins; its groups, recent publishes and publish URLs.
-- The app's own log stays readable by its owner for 30 days after the
-- deletion, then it's deleted too; the app's row stays, so its ID isn't
-- reused.
--
-- A row is written in the same transaction as the deletion
-- (apps.Registry.Delete), so a deleted app always has one, and the pass
-- retries it until it's done. Platform-level, like apps: IDs and times only.
CREATE TABLE app_deletions (
    app_id          text        PRIMARY KEY REFERENCES apps(id),
    requested_at    timestamptz NOT NULL DEFAULT now(),
    attempts        integer     NOT NULL DEFAULT 0,
    last_attempt_at timestamptz,
    last_error      text,
    workspaces_erased integer   NOT NULL DEFAULT 0, -- the app's own, so far
    swept_to        uuid,        -- other workspaces cleaned so far, in ID order
    data_deleted_at timestamptz, -- users' data gone
    log_deleted_at  timestamptz  -- the app's log gone
);
CREATE INDEX app_deletions_pending ON app_deletions (requested_at) WHERE log_deleted_at IS NULL;

-- Apps deleted before this: their users' data goes now, their logs 30 days
-- after they were deleted.
INSERT INTO app_deletions (app_id, requested_at)
SELECT id, deleted_at FROM apps WHERE deleted_at IS NOT NULL
ON CONFLICT DO NOTHING;
