-- Opaque topic subscription IDs: an app sends to a subscription by an ID
-- that names nothing (ts_<random>), mapped here to its workspace and
-- subscription. Platform-level, like topic_members (send looks it up before
-- it knows the workspace); only IDs. Rows outlive the subscription, so a
-- send to an ended one says closed.
CREATE TABLE topic_subscription_ids (
    id              text        PRIMARY KEY,
    app_id          text        NOT NULL REFERENCES apps(id),
    tenant_id       uuid        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    subscription_id uuid        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, subscription_id)
);

-- used_at: when a stream first opened on the link. A tool result doesn't
-- mint another link for a session while one minted a moment ago is unused.
ALTER TABLE stream_links ADD COLUMN used_at timestamptz;
CREATE INDEX stream_links_target ON stream_links (tenant_id, target, created_at DESC) WHERE revoked_at IS NULL;
