-- migrate:no-transaction
-- The expiry pass deletes the content of publishes that asked for it to go
-- once delivered and that nobody got within their day
-- (appapi.ExpirePublishContent); without this it reads every recent
-- publish. Small: a publish someone got loses its content at once.
DROP INDEX CONCURRENTLY IF EXISTS topic_events_to_redact;
CREATE INDEX CONCURRENTLY IF NOT EXISTS topic_events_to_redact
    ON topic_events (published_at) WHERE delete_on_delivery AND redacted_at IS NULL;
