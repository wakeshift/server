-- Wake-ups are paced, not capped: a delivery past a grant's wake limits is
-- scheduled for when there's room (streams hand out only deliveries that
-- are due), not dropped.
ALTER TABLE deliveries ADD COLUMN available_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX deliveries_paced ON deliveries (tenant_id, subscription_id, available_at);
