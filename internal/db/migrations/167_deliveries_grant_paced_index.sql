-- migrate:no-transaction
-- A grant's wake-ups in time order (pace.load): its window is one range of
-- this index, however many subscriptions the grant has had. Built
-- CONCURRENTLY: deliveries is the busiest table.
--
-- No DROP first: production builds this index by hand ahead of the
-- release (the same statement), which makes this a no-op instead of a
-- rebuild at startup. A build that failed part-way leaves an invalid
-- index, which the migrator drops and builds again (dropInvalidIndexes,
-- internal/db/migrate.go).
CREATE INDEX CONCURRENTLY IF NOT EXISTS deliveries_grant_paced
    ON deliveries (tenant_id, grant_id, available_at) WHERE grant_id IS NOT NULL;
