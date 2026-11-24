-- Connector versions (internal/connectorversion): what Wakeshift's
-- connectors on users' machines said they are, in their
-- X-Wakeshift-Connector header ("wakeshift/2026.10.20.1435+91faaf7 codex"),
-- or "<connector>/0 <harness>" for one from before the header.
--
-- stream_links.connector: set when a connector redeems a ticket for the
-- link (so a link with one is a connector's, which its successors keep),
-- then the latest that used it. NULL: a link an agent holds itself (a
-- Monitor, a wait command), which isn't checked.
ALTER TABLE stream_links ADD COLUMN connector text;

-- sessions.connector: the latest connector seen for the session (redeem,
-- stream, poll, wait, ack); the console shows them per harness, and a
-- tool result asks for an update while it's below the minimum.
-- connector_noted: the connector a tool result last said was out of date,
-- so other results say it once per connector, not on every call.
ALTER TABLE sessions ADD COLUMN connector text;
ALTER TABLE sessions ADD COLUMN connector_noted text;

-- deliveries.connector: the connector it was last handed to, for the
-- delivery activity.
ALTER TABLE deliveries ADD COLUMN connector text;
