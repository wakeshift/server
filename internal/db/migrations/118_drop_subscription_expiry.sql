-- migrate:no-transaction
-- subscriptions.ttl_seconds and expires_at (043) timed the memberships out;
-- the memberships and their expiry pass are gone (docs/subscribe-from-tools.md,
-- stage 7). Nothing has written them since, and the release before this one
-- stopped reading them (the wait's check for anything still subscribed was
-- the last reader), so this is safe while that release still serves. A topic
-- subscription's expiry is topic_subscriptions', which stays. The index goes
-- CONCURRENTLY first, so the column drop holds subscriptions' lock only for
-- a moment. Each statement commits on its own and is idempotent: the
-- migrator retries one that hits the 3 s lock_timeout.
SET lock_timeout = '3s';
DROP INDEX CONCURRENTLY IF EXISTS subscriptions_expiry;
ALTER TABLE subscriptions DROP COLUMN IF EXISTS ttl_seconds, DROP COLUMN IF EXISTS expires_at;
