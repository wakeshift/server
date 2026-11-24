-- migrate:no-transaction
-- A conversation's deliveries in event order, whatever their state. A
-- webhook's watermark (mcpevents.Watermark: every events/subscribe and
-- refresh) takes the newest of them, and replay (setupReplay, replayable)
-- reads them by range. Without this, those walk every delivery the agent
-- ever had in the workspace (the unique key leads with agent_id, then
-- event_id), so subscribing slowed as a month of deliveries piled up.
-- Built CONCURRENTLY: deliveries is the busiest table.
DROP INDEX CONCURRENTLY IF EXISTS deliveries_target_seq;
CREATE INDEX CONCURRENTLY IF NOT EXISTS deliveries_target_seq
    ON deliveries (tenant_id, agent_id, target, event_seq);
