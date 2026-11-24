-- When a webhook's "once" subscription made its one delivery
-- (mcpevents.Finish). A finished webhook no longer counts toward its
-- user's MaxWebhooks, nor exists for a new subscribe with its ID (which
-- replaces it), and the deliverer removes it once nothing waits for it:
-- the delivery that finished it is sent first. NULL for every existing
-- row, and for any the previous revision makes while both serve (it
-- doesn't finish webhooks, so they count until they lapse, as before).
-- Nullable with no default, so adding it changes only the catalog.
ALTER TABLE mcp_event_webhooks ADD COLUMN IF NOT EXISTS finished_at timestamptz;
