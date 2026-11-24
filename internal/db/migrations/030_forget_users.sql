-- Forgetting an app's user (POST /api/apps/v1/users/forget) deletes the
-- workspace the app keeps for them and everything in it, its audit log
-- included. The audit log stays append-only otherwise: a row can only be
-- deleted while its own workspace is being erased, which the erasing
-- transaction says by setting app.erasing_tenant to the workspace's ID
-- (appapi.eraseWorkspace). Updates and truncation are still refused.
CREATE OR REPLACE FUNCTION audit_log_append_only() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP = 'DELETE' AND OLD.tenant_id::text = current_setting('app.erasing_tenant', true) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'audit_log is append-only';
END $$;

-- A forgotten user's subscription IDs stay known, naming nothing, so the
-- app's sends and status calls on them say closed rather than unknown.
ALTER TABLE topic_subscription_ids ALTER COLUMN tenant_id DROP NOT NULL, ALTER COLUMN subscription_id DROP NOT NULL;
