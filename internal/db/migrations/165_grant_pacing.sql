-- Grant pacing (internal/pipeline, pace) reads a grant's recent wake-ups.
-- It joined deliveries to every subscription ever made under the grant
-- (ended ones too), one index probe per subscription, for every target of
-- every event: ~20,800 probes a second in production. A delivery now
-- carries its subscription's grant, so the window is one index range
-- (deliveries_grant_paced, 167). 166 fills it in for recent deliveries.
--
-- Every statement is a no-op where it has already run, and takes no lock
-- on deliveries then: production applies this file, 166 and 167's index
-- by hand ahead of the release (deliveries is the largest table), and the
-- release's migrations then change nothing.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = 'deliveries'::regclass AND attname = 'grant_id' AND NOT attisdropped) THEN
        ALTER TABLE deliveries ADD COLUMN grant_id uuid;
    END IF;
END $$;

-- Set on insert from the subscription (whose grant never changes), by the
-- database rather than deliveries.Insert: instances still on the previous
-- release insert deliveries while a deploy rolls out, and the new ones
-- must count those too.
CREATE OR REPLACE FUNCTION deliveries_grant() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.grant_id IS NULL THEN
        SELECT s.grant_id INTO NEW.grant_id FROM subscriptions s
            WHERE s.tenant_id = NEW.tenant_id AND s.id = NEW.subscription_id;
    END IF;
    RETURN NEW;
END $$;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = 'deliveries'::regclass AND tgname = 'deliveries_grant') THEN
        CREATE TRIGGER deliveries_grant BEFORE INSERT ON deliveries
            FOR EACH ROW EXECUTE FUNCTION deliveries_grant();
    END IF;
END $$;

-- Two steps pacing one grant at once must not both schedule from
-- snapshots without the other's wake-ups. A step that schedules a paced
-- wake-up writes its grant's row here; under repeatable read the second
-- writer waits for the first, then fails with a serialization conflict and
-- runs again on a snapshot that has the first one's wake-ups. It replaces
-- locking and rewriting the grant's own row every step (FOR NO KEY UPDATE
-- NOWAIT, UPDATE grants SET scope = scope): 14.1M updates left grants at
-- 669 MB for 471 rows, every read of a grant walked its dead versions, and
-- a request holding a grant FOR SHARE made steps spin. Nothing else reads
-- or locks this table; its rows are narrow, and it's vacuumed often.
-- Deleted with the workspace (eraseWorkspace) and the app (its grants).
CREATE TABLE IF NOT EXISTS grant_paces (
    tenant_id uuid        NOT NULL REFERENCES tenants(id),
    grant_id  uuid        NOT NULL,
    paced_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, grant_id)
) WITH (fillfactor = 50, autovacuum_vacuum_scale_factor = 0, autovacuum_vacuum_threshold = 500);
ALTER TABLE grant_paces ENABLE ROW LEVEL SECURITY;
ALTER TABLE grant_paces FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON grant_paces;
CREATE POLICY tenant_isolation ON grant_paces
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
DROP POLICY IF EXISTS admin_read ON grant_paces;
CREATE POLICY admin_read ON grant_paces FOR SELECT TO wakeshift_admin_read USING (true);
