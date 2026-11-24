-- Lookups the review found unindexed: the consent dry run (recent events by
-- type), a trace's halt notice, and members of an event type.
-- (Plain CREATE INDEX: migrations run in a transaction. Fine at this size;
-- a large table would need CONCURRENTLY, outside one.)
CREATE INDEX events_received ON events (tenant_id, received_at);
CREATE INDEX events_trace ON events (tenant_id, trace_id);
CREATE INDEX subscriptions_type ON subscriptions (tenant_id, event_type) WHERE status = 'active';
