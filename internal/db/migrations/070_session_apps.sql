-- The apps a session belongs to: the one that started it, and any that
-- registered it by its harness conversation's ID (a harness hands that only
-- to the MCP servers in the conversation). An app acting for an agent
-- resumes, watches, links or subscribes only its own sessions; another
-- app's handle is unknown to it, as it already is for headless apps.
-- created_by still counts for the app that started a session, so sessions
-- an earlier revision makes during a deploy keep working.
ALTER TABLE sessions ADD COLUMN app_ids text[] NOT NULL DEFAULT '{}';

-- What each app already did with a session (its subscriptions, links and
-- beacons for it) makes it one of the session's apps. Row-level security
-- applies to migrations too, so this goes one tenant at a time.
DO $$
DECLARE t uuid;
BEGIN
    FOR t IN SELECT id FROM tenants LOOP
        PERFORM set_config('app.tenant_id', t::text, true);
        UPDATE sessions s SET app_ids = x.apps
        FROM (
            SELECT session_id, array_agg(DISTINCT app_id ORDER BY app_id) AS apps FROM (
                SELECT id AS session_id, substr(created_by, 5) AS app_id FROM sessions WHERE created_by LIKE 'app:%'
                UNION ALL
                SELECT sub.session_id, g.app_id FROM subscriptions sub
                    JOIN grants g ON g.tenant_id = sub.tenant_id AND g.id = sub.grant_id
                    WHERE sub.session_id IS NOT NULL
                UNION ALL
                SELECT l.target::uuid, g.app_id FROM stream_links l
                    JOIN grants g ON g.tenant_id = l.tenant_id AND g.id = l.grant_id
                    WHERE l.target ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                UNION ALL
                SELECT session_id, app_id FROM connector_tickets
            ) a GROUP BY session_id
        ) x
        WHERE s.id = x.session_id;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
END $$;
