-- Expired MCP Events webhooks left behind: the deliverer removed a
-- workspace's expired webhooks only while it had a live one, so once a
-- workspace's last webhook lapsed, its rows stayed for good (and the admin
-- service counted those that lapsed mid-retry as failing). The deliverer
-- now visits every workspace with a target, expired or not; this gives each
-- left-behind webhook a target again, so its next pass removes it. Targets
-- are written as lapsing now, so a pass has an hour to remove them before
-- its own cleanup of old targets would. Row-level security applies to
-- migrations too, so the webhooks are read one workspace at a time; only
-- mcp_event_targets (platform-level) is written.
DO $$
DECLARE t uuid;
BEGIN
    FOR t IN SELECT id FROM tenants LOOP
        PERFORM set_config('app.tenant_id', t::text, true);
        INSERT INTO mcp_event_targets (tenant_id, webhook_id, expires_at)
            SELECT tenant_id, id, now() FROM mcp_event_webhooks WHERE expires_at <= now()
            ON CONFLICT (tenant_id, webhook_id) DO NOTHING;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
END $$;
