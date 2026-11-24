-- Each subscription's position in the event log. The pipeline runner claims
-- a cursor row (FOR UPDATE SKIP LOCKED), reads the events after it, writes
-- deliveries and advances it, all in one transaction, so a crash at any
-- point redoes the work and never loses or doubles it.
CREATE TABLE subscription_cursors (
    tenant_id       uuid        NOT NULL,
    subscription_id uuid        NOT NULL,
    last_seq        bigint      NOT NULL,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, subscription_id),
    FOREIGN KEY (tenant_id, subscription_id) REFERENCES subscriptions (tenant_id, id)
);
ALTER TABLE subscription_cursors ENABLE ROW LEVEL SECURITY;
ALTER TABLE subscription_cursors FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON subscription_cursors
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

-- Existing subscriptions start where they began. Row-level security
-- applies to migrations too, so data is moved one tenant at a time.
DO $$
DECLARE t uuid;
BEGIN
    FOR t IN SELECT id FROM tenants LOOP
        PERFORM set_config('app.tenant_id', t::text, true);
        INSERT INTO subscription_cursors (tenant_id, subscription_id, last_seq)
            SELECT tenant_id, id, start_seq FROM subscriptions;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
END $$;

-- The outbox: one delivery per (agent, event, target). target is '' for the
-- agent as a whole, or a session. Its ID is derived from those, so redoing
-- the runner's work inserts nothing new.
--
-- States: pending → in_flight (leased to a stream) → delivered (written to a
-- consumer that can't acknowledge) or acked. A lease that expires puts an
-- in_flight delivery back to pending for another attempt; after too many
-- attempts it's dead. Revoking or cancelling makes pending ones cancelled.
CREATE TABLE deliveries (
    tenant_id       uuid        NOT NULL,
    id              uuid        NOT NULL,
    agent_id        text        NOT NULL,
    target          text        NOT NULL DEFAULT '',
    event_id        uuid        NOT NULL,
    event_seq       bigint      NOT NULL,
    subscription_id uuid        NOT NULL,
    state           text        NOT NULL DEFAULT 'pending'
                                CHECK (state IN ('pending', 'in_flight', 'delivered', 'acked', 'dead', 'cancelled')),
    attempts        int         NOT NULL DEFAULT 0,
    lease_until     timestamptz,
    lease_holder    text,
    last_error      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, agent_id, event_id, target),
    FOREIGN KEY (tenant_id, subscription_id) REFERENCES subscriptions (tenant_id, id),
    FOREIGN KEY (tenant_id, event_id) REFERENCES events (tenant_id, id)
);
-- A stream's queue: an agent's (target's) open deliveries in event order.
CREATE INDEX deliveries_queue ON deliveries (tenant_id, agent_id, target, event_seq)
    WHERE state IN ('pending', 'in_flight');
ALTER TABLE deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE deliveries FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON deliveries
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
