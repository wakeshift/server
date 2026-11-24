-- Wakeshift for Mac is removed (docs/removed/wakeshift-for-mac.md), and
-- with it the device tokens it signed in to the console API with: nothing
-- accepts one any more. This revokes every device still live, so a token on
-- disk is dead here too, whatever a previous revision still serving during
-- the rollout would make of it, and records each revocation in its
-- workspace's audit log (actor hub), as revoking one in the console did.
--
-- The devices table stays for now: the previous revision reads it until it
-- drains. A later migration drops it (AGENTS.md, Pending removals).
--
-- Row-level security applies to migrations too, so this runs one tenant at
-- a time. Run again, it finds nothing live and changes nothing.
DO $$
DECLARE t uuid;
BEGIN
    FOR t IN SELECT id FROM tenants LOOP
        PERFORM set_config('app.tenant_id', t::text, true);
        WITH revoked AS (
            UPDATE devices SET revoked_at = now()
            WHERE revoked_at IS NULL
            RETURNING id, user_id
        )
        INSERT INTO audit_log (tenant_id, actor, action, detail)
            SELECT current_tenant(), 'hub', 'device.revoked',
                jsonb_build_object('device_id', id::text, 'user_id', user_id::text, 'reason', 'wakeshift_for_mac_removed')
            FROM revoked;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
END $$;
