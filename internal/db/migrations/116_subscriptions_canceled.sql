-- migrate:no-transaction
-- A canceled subscription's status is 'canceled', the American spelling
-- the API reports. The release before this one still writes 'cancelled'
-- while it serves alongside this one, so the check allows both, and code
-- from this release on reads 'cancelled' as canceled. A later migration
-- moves what that release wrote in the meantime and drops 'cancelled'
-- from the check (AGENTS.md, Pending removals).
--
-- The cancelled_at column keeps its name: the release before this one
-- reads and writes it, and the API reports it as canceled_at.
--
-- Outside a transaction, so subscriptions' ACCESS EXCLUSIVE lock is held
-- only to swap the check (added NOT VALID, then validated under a lock
-- that lets reads and writes through), and each workspace's rows are
-- updated and committed on their own. Every statement is idempotent: the
-- migrator runs the file again from the top after a lock conflict.
SET lock_timeout = '3s';
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_status_check;
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_status_check
    CHECK (status IN ('active', 'canceled', 'cancelled')) NOT VALID;
ALTER TABLE subscriptions VALIDATE CONSTRAINT subscriptions_status_check;

DO $$
DECLARE t uuid;
BEGIN
    FOR t IN SELECT id FROM tenants LOOP
        PERFORM set_config('app.tenant_id', t::text, true);
        UPDATE subscriptions SET status = 'canceled' WHERE status = 'cancelled';
        COMMIT;
    END LOOP;
END $$;
