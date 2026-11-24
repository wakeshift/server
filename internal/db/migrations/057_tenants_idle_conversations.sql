-- migrate:no-transaction
-- Topics' conversations without a user (~conversation:…) are forgotten once
-- idle, as guests are (appapi.ExpireIdleGuests); this finds them as
-- tenants_idle_guests (050) finds guests.
DROP INDEX CONCURRENTLY IF EXISTS tenants_idle_conversations;
CREATE INDEX CONCURRENTLY IF NOT EXISTS tenants_idle_conversations ON tenants ((coalesce(active_at, created_at))) WHERE kind = 'app' AND starts_with(external_id, '~conversation:');
