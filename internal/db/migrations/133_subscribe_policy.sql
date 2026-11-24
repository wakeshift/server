-- migrate:no-transaction
-- Who may subscribe to an app's event (docs/sdk-design-next.md, §3): an
-- event the manifest marks subscribe "tools" is subscribed to only through
-- the app's own tools; "any" (and an event never given a setting) also
-- through subscribe_events and native MCP Events (subscribe and poll).
--
-- apps.event_subscribe is every setting a manifest has given, by event
-- name: a manifest that leaves it out keeps it (an old SDK's republish
-- can't reopen an event), even across the event leaving the manifest and
-- coming back. The manifest's own events carry it too, filled in from here.
--
-- event_subscriptions.via is how each subscription was made: tool:<name>
-- (an app's tool), subscribe_events (the SDKs' built-in tool), native
-- (events/subscribe) or poll (events/poll). Closing an event ends the
-- ones made through the last three. Rows from before this column, and a
-- conversation's from a call that named no tool, have none.
--
-- The previous revision neither reads nor writes either column.
--
-- Outside a transaction, so event_subscriptions' ACCESS EXCLUSIVE lock is
-- held only to add the column (no rewrite) and the check (added NOT VALID,
-- then validated under a lock that lets reads and writes through). Every
-- statement is idempotent: the migrator runs the file again from the top
-- after a lock conflict.
SET lock_timeout = '3s';
ALTER TABLE apps ADD COLUMN IF NOT EXISTS event_subscribe jsonb NOT NULL DEFAULT '{}';
ALTER TABLE event_subscriptions ADD COLUMN IF NOT EXISTS via text;
ALTER TABLE event_subscriptions DROP CONSTRAINT IF EXISTS event_subscriptions_via_check;
ALTER TABLE event_subscriptions ADD CONSTRAINT event_subscriptions_via_check
    CHECK (via IN ('subscribe_events', 'native', 'poll') OR via LIKE 'tool:_%') NOT VALID;
ALTER TABLE event_subscriptions VALIDATE CONSTRAINT event_subscriptions_via_check;
