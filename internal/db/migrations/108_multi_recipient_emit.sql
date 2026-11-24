-- migrate:no-transaction
-- Multi-recipient emit (docs/subscribe-from-tools.md, "Multi-recipient
-- emit"; stage 1, additive). One table per statement, outside a
-- transaction, so no statement holds two hot tables' locks (apps is read on
-- every request); each is idempotent, as the migrator may run it again.
-- The previous revision neither reads nor writes any of it.
--
-- apps.emit_recipients_per_minute: our override of the app's limit on
-- recipients a minute across its multi-recipient emits (NULL: the default,
-- apps.DefaultEmitRecipientsPerMinute). Agent Chat and the games fan out
-- every message to their members, so they start at 60,000.
ALTER TABLE apps ADD COLUMN IF NOT EXISTS emit_recipients_per_minute integer CHECK (emit_recipients_per_minute BETWEEN 0 AND 10000000);
UPDATE apps SET emit_recipients_per_minute = 60000 WHERE id IN ('agentchat', 'secretagent', 'nightfall', 'sketch') AND emit_recipients_per_minute IS NULL;
-- The per-app counter: recipients of the app's multi-recipient emits in the
-- current minute (window_start). Platform-level, like apps: an app ID, a
-- time and a count. One row per app, updated with INSERT … ON CONFLICT …
-- RETURNING inside the emit's recording transaction, so the count needs no
-- scan and no advisory lock.
CREATE TABLE IF NOT EXISTS app_emit_counters (
    app_id       text        PRIMARY KEY REFERENCES apps(id) ON DELETE CASCADE,
    window_start timestamptz NOT NULL,
    recipients   integer     NOT NULL
);
-- topic_events.recipients: a multi-recipient emit's recipients as recorded
-- (its event, its arguments' key, each recipient), so a retry with the same
-- id uses them, not its own list. results: what its copies found (the
-- recipients reached, and those found to have no live subscription or no
-- workspace), so a duplicate returns the same ended and unmatched lists.
-- Both NULL for a publish or a single-user emit.
ALTER TABLE topic_events ADD COLUMN IF NOT EXISTS recipients jsonb;
ALTER TABLE topic_events ADD COLUMN IF NOT EXISTS results jsonb;
-- topic_fanout.direct: the subscription and native recipients to look for in
-- the workspace (their users' filters, as userWorkspaces gives them), copied
-- one event per subscription (ForAttr); NULL for a publish's rows.
ALTER TABLE topic_fanout ADD COLUMN IF NOT EXISTS direct jsonb;
-- usage_daily.billable: distinct (emit, agent) among the day's delivered or
-- acknowledged deliveries, an emit being an event's $emit attribute or its
-- own ID: one emit to several conversations of an agent counts once.
ALTER TABLE usage_daily ADD COLUMN IF NOT EXISTS billable integer NOT NULL DEFAULT 0;
