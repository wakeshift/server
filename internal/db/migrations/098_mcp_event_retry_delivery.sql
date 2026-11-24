-- The delivery a webhook's retry state (retry_attempts, retrying_since)
-- belongs to. The head delivery can end while it's being retried
-- (cancelled with its subscription), and the next one mustn't inherit its
-- count or retry window: the deliverer starts afresh when the delivery at
-- the head isn't this one (internal/mcpevents). NULL when nothing is being
-- retried, and for existing rows: their next attempt starts afresh, which
-- at worst grants a delivery being retried now a few more attempts.
-- Nullable with no default, so adding it changes only the catalog; the
-- migrator's lock_timeout keeps it from queueing behind live traffic.
ALTER TABLE mcp_event_webhooks ADD COLUMN IF NOT EXISTS retry_delivery_id uuid;
