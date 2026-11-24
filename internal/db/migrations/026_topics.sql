-- Topics (docs/webhooks.md): an app subscribes the calling conversation to
-- one of its topics and publishes to it. Behind the scenes each conversation
-- is a session in a workspace the app owns (docs/headless.md), and a
-- topic subscription is an ordinary subscription there (to the app's
-- topics.<app> event type, with the topic as a filter), so the pipeline,
-- pacing, deliveries and streams work unchanged. A publish is copied into
-- the log of each workspace with a subscriber, as a group post is.

-- What a topic subscription adds to its subscription.
CREATE TABLE topic_subscriptions (
    tenant_id       uuid        NOT NULL,
    subscription_id uuid        NOT NULL,
    app_id          text        NOT NULL REFERENCES apps(id),
    topic           text        NOT NULL,
    session_id      uuid        NOT NULL,
    once            boolean     NOT NULL DEFAULT false,
    ttl_seconds     integer     NOT NULL,
    -- Ends without a delivery or a fresh subscribe by then.
    expires_at      timestamptz NOT NULL,
    -- Why Wakeshift ended it (unsubscribed, ttl, closed, forgotten). A
    -- subscription that ended after its one delivery (once) or with its
    -- grant shows only as the subscription cancelled.
    ended_reason    text,
    ended_at        timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, subscription_id),
    FOREIGN KEY (tenant_id, subscription_id) REFERENCES subscriptions (tenant_id, id)
);
CREATE INDEX topic_subscriptions_session ON topic_subscriptions (tenant_id, session_id, topic) WHERE ended_at IS NULL;
CREATE INDEX topic_subscriptions_expiry ON topic_subscriptions (tenant_id, expires_at) WHERE ended_at IS NULL;
ALTER TABLE topic_subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE topic_subscriptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON topic_subscriptions
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

-- Which workspaces a publish is copied into: platform-level, like
-- group_members (a publish spans workspaces; each copy is written inside
-- its own workspace). Only IDs; what the subscription is stays in the
-- workspace.
CREATE TABLE topic_members (
    app_id          text NOT NULL REFERENCES apps(id),
    topic           text NOT NULL,
    tenant_id       uuid NOT NULL REFERENCES tenants(id),
    subscription_id uuid NOT NULL,
    PRIMARY KEY (tenant_id, subscription_id)
);
CREATE INDEX topic_members_topic ON topic_members (app_id, topic, tenant_id);

-- The app's recent publishes, platform-level (they're the app's, not a
-- workspace's): for replay (the latest event within a window) and so a
-- retried publish with the same id is kept once. Kept for a day.
CREATE TABLE topic_events (
    app_id       text        NOT NULL REFERENCES apps(id),
    source_id    text        NOT NULL, -- the app's id for the event, or a generated one
    event_id     uuid        NOT NULL, -- the ID of its copies in each workspace
    topics       text[]      NOT NULL,
    text         text        NOT NULL,
    data         jsonb,
    matched      integer     NOT NULL DEFAULT 0,
    publish_url  uuid,                 -- the publish URL it came through, if any
    published_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (app_id, source_id)
);
CREATE INDEX topic_events_recent ON topic_events (app_id, published_at DESC);
CREATE INDEX topic_events_url ON topic_events (publish_url, published_at DESC) WHERE publish_url IS NOT NULL;

-- Publish URLs: capability URLs that publish to one topic. Platform-level
-- (the app's); looked up by the token's hash.
CREATE TABLE topic_publish_urls (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    app_id       text        NOT NULL REFERENCES apps(id),
    topic        text        NOT NULL,
    token_hash   bytea       NOT NULL UNIQUE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at   timestamptz
);
CREATE INDEX topic_publish_urls_app ON topic_publish_urls (app_id, topic);
