-- migrate:no-transaction
-- Resend and ResendLost look up what one connection marked delivered to an
-- agent in the last few minutes; without this they walk every delivery the
-- agent ever had. Built CONCURRENTLY: deliveries is the busiest table.
DROP INDEX CONCURRENTLY IF EXISTS deliveries_delivered;
CREATE INDEX CONCURRENTLY IF NOT EXISTS deliveries_delivered
    ON deliveries (tenant_id, agent_id, lease_holder, finished_at) WHERE state = 'delivered';
