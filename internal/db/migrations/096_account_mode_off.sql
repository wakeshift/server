-- Account mode (accounts "wakeshift": Sign in with Wakeshift, an app's
-- grants in people's own workspaces through Wakeshift's consent screen) is
-- turned off (docs/headless.md). A manifest can't ask for it any more, and
-- an app whose stored manifest still does is wound down as one that
-- switched to headless (055_app_account_sweeps.sql): the expiry pass
-- (appapi.FinishAccountSweeps) revokes its grants in people's own
-- workspaces and deletes what they allowed there, each workspace's audit
-- log recording it, and deletes its groups made before now. A sweep left
-- from an earlier switch, done or not, starts over, so grants made since
-- are covered. apps and app_account_sweeps are platform-level (no
-- row-level security), so no loop over tenants is needed.
INSERT INTO app_account_sweeps (app_id)
SELECT id FROM apps WHERE manifest->>'accounts' = 'wakeshift' AND deleted_at IS NULL
ON CONFLICT (app_id) DO UPDATE SET switched_at = now(), attempts = 0, last_attempt_at = NULL, last_error = NULL,
    swept_to = NULL, done_at = NULL;
