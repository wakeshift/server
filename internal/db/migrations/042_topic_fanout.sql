-- A publish's copies still to be made, one row per workspace: a publish
-- records itself (topic_events) and these in one short transaction, then
-- copies the event into each workspace in a transaction of its own, deleting
-- the workspace's row in the same one. A failure in one workspace leaves its
-- row for a retry of the publish (same event ID) or the expiry pass, and
-- doesn't undo the others. Platform-level, like topic_members; subscriptions
-- are the topic's members when the publish was recorded (a later subscriber
-- gets it by replay, as before). No foreign key to tenants: a publish
-- doesn't wait for a workspace being forgotten (eraseWorkspace deletes its
-- rows; an orphan copies nothing).
CREATE TABLE topic_fanout (
    app_id        text        NOT NULL,
    source_id     text        NOT NULL,
    tenant_id     uuid        NOT NULL,
    subscriptions uuid[]      NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (app_id, source_id, tenant_id),
    FOREIGN KEY (app_id, source_id) REFERENCES topic_events (app_id, source_id) ON DELETE CASCADE
);
CREATE INDEX topic_fanout_age ON topic_fanout (created_at);
CREATE INDEX topic_fanout_tenant ON topic_fanout (tenant_id);
