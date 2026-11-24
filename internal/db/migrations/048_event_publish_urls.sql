-- An event's publish URL: a POST emits the event (its payload checked
-- against the event's schema) to topic, where the event's occurrences for
-- the URL's arguments go. NULL: a topic's publish URL, as before.
ALTER TABLE topic_publish_urls ADD COLUMN event text;
