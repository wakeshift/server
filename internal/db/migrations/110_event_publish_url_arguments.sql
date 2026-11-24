-- An event's publish URL keeps the arguments it was made for (canonical
-- JSON) and, for a personal event, the one subscriber it emits to, so the
-- console lists them (docs/subscribe-from-tools.md, "Publish URLs onto
-- events"). NULL for a topic's URL, and for an event's made before this.
-- The previous revision neither reads nor writes them.
ALTER TABLE topic_publish_urls ADD COLUMN IF NOT EXISTS arguments jsonb;
ALTER TABLE topic_publish_urls ADD COLUMN IF NOT EXISTS subscriber text;
