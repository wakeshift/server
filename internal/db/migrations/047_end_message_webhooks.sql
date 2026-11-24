-- wakeshift.message is gone (plan step (d)): the core no longer serves it,
-- and the Go SDK no longer offers it. 045 ended its webhooks once; this
-- ends any made since. Each workspace in turn, since mcp_event_webhooks is
-- row-level secured.
DO $$
DECLARE t uuid;
BEGIN
    FOR t IN SELECT id FROM tenants LOOP
        PERFORM set_config('app.tenant_id', t::text, true);
        DELETE FROM mcp_event_targets WHERE tenant_id = t AND webhook_id IN
            (SELECT id FROM mcp_event_webhooks WHERE name = 'wakeshift.message');
        DELETE FROM mcp_event_webhooks WHERE name = 'wakeshift.message';
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
END $$;
