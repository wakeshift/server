-- A publish URL can require Standard Webhooks signatures (Svix and other
-- senders; site/docs/publish-urls.md, "Signed webhooks"): with a signing
-- secret set, a POST must carry a valid webhook-signature over
-- id.timestamp.body. The secret is sealed with a key derived from the
-- server's secret key, bound to the URL's ID (mcpevents.Sealer), like MCP
-- Events webhooks' (044); only a fingerprint (the first 12 hex digits of
-- the SHA-256 of the whsec_ string) is ever shown. After a rotation the
-- previous secret is accepted too, until previous_signing_until.
--
-- The previous revision neither reads nor writes them: while it drains, a
-- POST it serves isn't checked, as before.
ALTER TABLE topic_publish_urls ADD COLUMN IF NOT EXISTS signing_secret bytea;
ALTER TABLE topic_publish_urls ADD COLUMN IF NOT EXISTS signing_fingerprint text;
ALTER TABLE topic_publish_urls ADD COLUMN IF NOT EXISTS signing_set_at timestamptz;
ALTER TABLE topic_publish_urls ADD COLUMN IF NOT EXISTS previous_signing_secret bytea;
ALTER TABLE topic_publish_urls ADD COLUMN IF NOT EXISTS previous_signing_until timestamptz;
