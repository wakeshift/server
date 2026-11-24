-- migrate:no-transaction
-- A native subscription to a group's event (docs/delivery-destinations.md):
-- group_type names the group whose copies, for the subscribing agent's
-- memberships, go to the webhook's session instead of the member's
-- conversation (pipeline.Targets). NULL for every other event
-- subscription. Nullable with no default, so adding it only changes the
-- catalog; the index is built CONCURRENTLY.
SET lock_timeout = '3s';
ALTER TABLE event_subscriptions ADD COLUMN IF NOT EXISTS group_type text;
CREATE INDEX CONCURRENTLY IF NOT EXISTS event_subscriptions_group_routes
    ON event_subscriptions (tenant_id, agent_id, group_type) WHERE group_type IS NOT NULL AND ended_at IS NULL;
