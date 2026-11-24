-- migrate:no-transaction
-- The expiry pass looks for flagged events whose content is still there
-- (deliveries.RedactDelivered); without this it walks every event. Built
-- CONCURRENTLY: events is one of the busiest tables.
DROP INDEX CONCURRENTLY IF EXISTS events_to_redact;
CREATE INDEX CONCURRENTLY IF NOT EXISTS events_to_redact
    ON events (tenant_id, seq) WHERE delete_on_delivery AND redacted_at IS NULL;
