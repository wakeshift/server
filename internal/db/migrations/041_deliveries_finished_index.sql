-- migrate:no-transaction
-- Retention (deliveries.Prune) deletes finished deliveries past the window,
-- oldest first.
DROP INDEX CONCURRENTLY IF EXISTS deliveries_finished;
CREATE INDEX CONCURRENTLY IF NOT EXISTS deliveries_finished
    ON deliveries (tenant_id, finished_at) WHERE state IN ('delivered', 'acked', 'dead', 'cancelled');
