-- migrate:no-transaction
-- deliveries.grant_id (165) for the deliveries made before it that grant
-- pacing can still count: inside the longest wake limit's window (a day,
-- apps.Validate) or still scheduled (at most an hour ahead, so created in
-- the last 25 hours). Outside a transaction, with a COMMIT per tenant (as
-- 091_admin_read.sql), so 165's brief lock on deliveries isn't held while
-- this runs and no tenant's rows stay locked for longer than its own
-- update. Only rows still without a grant are updated, so a retry carries
-- on where it stopped. Row-level security applies to migrations too, so
-- one tenant at a time.
DO $$
DECLARE t uuid;
BEGIN
    FOR t IN SELECT id FROM tenants LOOP
        PERFORM set_config('app.tenant_id', t::text, true);
        UPDATE deliveries d SET grant_id = s.grant_id
            FROM subscriptions s
            WHERE s.tenant_id = d.tenant_id AND s.id = d.subscription_id AND s.grant_id IS NOT NULL
            AND d.grant_id IS NULL AND d.created_at > now() - interval '25 hours' AND d.available_at > now() - interval '1 day';
        COMMIT;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
END $$;
