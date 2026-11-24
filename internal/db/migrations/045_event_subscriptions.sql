-- Event subscriptions (docs/mcp-events-bridge.md, docs/mcp-events-plan.md
-- (b)): a subscription to an event type by name and arguments, through
-- whichever path its client takes (a webhook for a client that speaks MCP
-- Events, the conversation's own session otherwise). Each is also the
-- consent record the console lists and revokes: who (the principal, as the
-- source names them), which source and event, which arguments, and where it
-- goes.
--
-- Keyed by source, not by app: source_kind 'app' is one of Wakeshift's
-- apps (source_id its ID). A third-party MCP Events server would be another
-- kind, with no change to the key.
--
-- Routing stays with topics: subscription_id is the topic subscription the
-- arguments made (topic(arguments), filter(arguments)).
CREATE TABLE event_subscriptions (
    tenant_id       uuid        NOT NULL REFERENCES tenants(id),
    id              uuid        NOT NULL DEFAULT gen_random_uuid(),
    source_kind     text        NOT NULL CHECK (source_kind IN ('app')),
    source_id       text        NOT NULL,
    event           text        NOT NULL,
    arguments       text        NOT NULL, -- canonical JSON (apps.Canonical)
    arguments_key   text        NOT NULL, -- hex SHA-256 of arguments
    principal       text        NOT NULL DEFAULT '', -- the source's user ('' in a conversation's own workspace)
    destination     text        NOT NULL CHECK (destination IN ('conversation', 'webhook')),
    agent_id        text        NOT NULL,
    session_id      uuid        NOT NULL,
    webhook_url     text,
    webhook_id      text,       -- mcp_event_webhooks.id
    subscription_id uuid        NOT NULL,
    created_by      text        NOT NULL, -- "app:<id>"
    created_at      timestamptz NOT NULL DEFAULT now(),
    refreshed_at    timestamptz NOT NULL DEFAULT now(),
    ended_at        timestamptz,
    ended_reason    text,       -- unsubscribed, revoked, expired, ended
    ended_by        text,
    PRIMARY KEY (tenant_id, id),
    CHECK ((destination = 'webhook') = (webhook_url IS NOT NULL AND webhook_id IS NOT NULL)),
    CHECK ((ended_at IS NULL) = (ended_reason IS NULL)),
    FOREIGN KEY (tenant_id, subscription_id) REFERENCES subscriptions (tenant_id, id)
);
-- One live subscription per conversation and (source, event, arguments);
-- one per principal, callback and (source, event, arguments) for webhooks.
CREATE UNIQUE INDEX event_subscriptions_conversation ON event_subscriptions
    (tenant_id, source_kind, source_id, event, arguments_key, session_id)
    WHERE ended_at IS NULL AND destination = 'conversation';
CREATE UNIQUE INDEX event_subscriptions_webhook ON event_subscriptions
    (tenant_id, source_kind, source_id, principal, webhook_url, event, arguments_key)
    WHERE ended_at IS NULL AND destination = 'webhook';
CREATE INDEX event_subscriptions_routing ON event_subscriptions (tenant_id, subscription_id);
CREATE INDEX event_subscriptions_listed ON event_subscriptions (tenant_id, created_at DESC);
ALTER TABLE event_subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_subscriptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON event_subscriptions
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

-- wakeshift.message, the one generic event of #83, is being retired
-- (docs/mcp-events-bridge.md): its webhooks end now, so a client lists the
-- app's events again; the SDKs stop offering it when they move to event
-- types (plan steps (c) and (d)). Each workspace in turn, since
-- mcp_event_webhooks is row-level secured.
DO $$
DECLARE t uuid;
BEGIN
    FOR t IN SELECT id FROM tenants LOOP
        PERFORM set_config('app.tenant_id', t::text, true);
        DELETE FROM mcp_event_targets WHERE tenant_id = t AND webhook_id IN
            (SELECT id FROM mcp_event_webhooks WHERE name = 'wakeshift.message');
        DELETE FROM mcp_event_webhooks WHERE name = 'wakeshift.message';
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
END $$;
