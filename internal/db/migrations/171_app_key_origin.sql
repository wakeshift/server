-- Where each app key came from.
--
-- created_role: the role (orgs.Role) of the member who made or registered
-- the key, when they did. The app API refuses an owner- or admin-only action
-- (users/forget, orgs.AppForgetUser) from a key a developer made, as the
-- console refuses it to the developer. Null: a WAKESHIFT_APPS key (no
-- organization), or a console key whose maker has left the organization.
--
-- via: 'cli_link' for a key registered from @wakeshift/cli's link (the
-- console's confirm page); null for one the console made.
--
-- New nullable columns on one table: no table rewrite, and the previous
-- revision ignores them.
ALTER TABLE app_keys
    ADD COLUMN created_role text CHECK (created_role IN ('owner', 'admin', 'developer')),
    ADD COLUMN via text CHECK (via IN ('cli_link'));

-- Keys from before: their maker's role in the app's organization (a key
-- from before app_keys had created_by was its app's creator's), only where
-- that role is known to be the one they had when they made the key: they've
-- been a member since before it (or it's their personal organization, whose
-- membership 072 backfilled later than their old keys), and the
-- organization's log has no change of their role since. Anyone else's key
-- gets none, and may do only what a developer may. app_keys, apps,
-- organizations, organization_members and org_log are platform-level, so
-- this sees every app's.
UPDATE app_keys k SET created_role = m.role
FROM apps a
JOIN organizations o ON o.id = a.org_id
JOIN organization_members m ON m.org_id = a.org_id
WHERE a.id = k.app_id
  AND m.user_id::text = coalesce(substring(k.created_by FROM '^user:(.*)$'), a.created_by::text)
  AND (m.created_at <= k.created_at OR o.personal_of = m.user_id)
  AND NOT EXISTS (SELECT 1 FROM org_log l WHERE l.org_id = a.org_id AND l.action = 'org.role_changed'
                  AND l.detail->>'user_id' = m.user_id::text AND l.at >= k.created_at);

-- Keys registered from a link before: their app log says so.
UPDATE app_keys k SET via = 'cli_link'
WHERE EXISTS (SELECT 1 FROM app_log l WHERE l.app_id = k.app_id AND l.key_id = k.key_id
              AND l.action = 'app.key_created' AND l.detail->>'via' = 'cli');
