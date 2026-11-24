-- How many workspaces each app owns, by kind (080), without legacy guests'
-- kind: their workspaces are gone (100), so an app's workspaces are one per
-- user or one per conversation without a user. Same columns, so the admin
-- read role's grant stays.
CREATE OR REPLACE VIEW admin_app_workspaces AS
    SELECT id, app_id, created_at, active_at,
           CASE WHEN starts_with(external_id, '~conversation:') THEN 'conversation'
                ELSE 'user' END AS kind
    FROM tenants WHERE kind = 'app' AND app_id IS NOT NULL;
