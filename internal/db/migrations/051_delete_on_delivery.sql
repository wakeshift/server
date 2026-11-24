-- Delete on delivery: an app's event type can ask that an occurrence's
-- content (its text and payload) be deleted once it's delivered
-- (apps.EventType.DeleteOnDelivery), e.g. Slackline's slack.message. The
-- publish and each workspace's copy carry the flag; redacted_at is when the
-- content went, leaving the row's metadata (IDs for dedupe and cursors,
-- type, times, who it was from) for the activity and audit.
--
-- Both are new columns with constant defaults: no table rewrite, and the
-- previous revision, which doesn't know them, writes rows without the flag.
ALTER TABLE events ADD COLUMN delete_on_delivery boolean NOT NULL DEFAULT false, ADD COLUMN redacted_at timestamptz;
ALTER TABLE topic_events ADD COLUMN delete_on_delivery boolean NOT NULL DEFAULT false, ADD COLUMN redacted_at timestamptz;
