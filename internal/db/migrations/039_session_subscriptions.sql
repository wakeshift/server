-- A subscription an app makes belongs to the conversation that made it:
-- the API refuses one without a session unless the app asks for agent_wide.
-- Ones apps made before that without a session (session_id NULL) woke
-- every conversation of the agent, including ones started days later, so
-- they end here, with their undelivered deliveries and their group
-- memberships (as subscriptions.Cancel does). The apps' next subscribe
-- binds the conversation's session. Subscriptions a person made in the
-- console (created_by isn't "app:…") are theirs and stay.
-- Row-level security applies to migrations too: one tenant at a time.
DO $$
DECLARE t uuid;
BEGIN
    FOR t IN SELECT id FROM tenants LOOP
        PERFORM set_config('app.tenant_id', t::text, true);
        CREATE TEMP TABLE ended ON COMMIT DROP AS
            SELECT id, agent_id, event_type FROM subscriptions
            WHERE status = 'active' AND session_id IS NULL AND created_by LIKE 'app:%';
        UPDATE subscriptions SET status = 'cancelled', cancelled_at = now()
            WHERE id IN (SELECT id FROM ended);
        UPDATE deliveries SET state = 'cancelled', finished_at = now(), lease_until = NULL, lease_holder = NULL
            WHERE subscription_id IN (SELECT id FROM ended) AND state IN ('pending', 'in_flight');
        DELETE FROM group_members m USING ended e
            WHERE m.type = e.event_type AND m.agent_id = e.agent_id AND m.session_id IS NULL
            AND NOT EXISTS (SELECT 1 FROM subscriptions s WHERE s.agent_id = e.agent_id AND s.event_type = e.event_type
                AND s.status = 'active' AND s.session_id IS NULL);
        DROP TABLE ended;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
END $$;
