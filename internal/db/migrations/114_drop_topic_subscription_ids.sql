-- Stage 6 of docs/subscribe-from-tools.md, the drop: the topic routes went
-- a release ago, and that release stopped reading and writing what's here.
--
-- topic_subscription_ids mapped the topic routes' opaque subscription IDs
-- (ts_…) to a workspace and subscription; only those routes made or read
-- them. Platform-level, so it has no row-level security policies to drop.
DROP TABLE IF EXISTS topic_subscription_ids;

-- Publish URLs made for a topic, before publish URLs moved onto events,
-- have answered 410 since that release. Platform-level, like the app.
DELETE FROM topic_publish_urls WHERE event IS NULL;
