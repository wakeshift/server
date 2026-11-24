-- An MCP Events webhook in the spec's dialect (event.DialectSpec) sends
-- each delivery with a webhook-id of its own (SEP-3415: unique per
-- message, the same on its retries, never derived from eventId): made when
-- the delivery is first claimed for the webhook and kept here, so a retry,
-- or a resend after an instance stopped mid-send, reuses it. A replay from
-- a cursor is a new message: setting it up clears the replayed deliveries'
-- IDs. NULL for every other delivery. Nullable without a default, so
-- adding it rewrites nothing.
ALTER TABLE deliveries ADD COLUMN message_id text;
