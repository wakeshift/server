-- An app's subscription to a declared type or a group (Agent Chat's
-- channels, the games' rooms) ends after a while without use, as a topic
-- subscription does: ttl_seconds is how long, expires_at when. A delivery,
-- a fresh subscribe and the conversation posting to the type move it on.
-- NULL: it doesn't expire (a person's own, from the console, and topics',
-- which topic_subscriptions times).
ALTER TABLE subscriptions ADD COLUMN ttl_seconds integer, ADD COLUMN expires_at timestamptz;

CREATE INDEX subscriptions_expiry ON subscriptions (tenant_id, expires_at) WHERE status = 'active' AND expires_at IS NOT NULL;

-- The apps' active subscriptions get 30 days from now. Row-level security
-- applies to migrations too: one tenant at a time.
DO $$
DECLARE t uuid;
BEGIN
    FOR t IN SELECT id FROM tenants LOOP
        PERFORM set_config('app.tenant_id', t::text, true);
        UPDATE subscriptions s SET ttl_seconds = 2592000, expires_at = now() + interval '30 days'
            WHERE s.status = 'active' AND s.created_by LIKE 'app:%'
            AND NOT EXISTS (SELECT 1 FROM topic_subscriptions ts WHERE ts.tenant_id = s.tenant_id AND ts.subscription_id = s.id);
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
END $$;
