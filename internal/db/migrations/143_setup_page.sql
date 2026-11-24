-- The page that pasted a Grok Bot webhook (docs/grok-webhooks.md 3.10):
-- the hash of a random value the setup page sends with its save, so the
-- removal token is handed only to that page (describe with the same
-- value), not to whoever else holds the setup link. Tenant data, in the
-- tenant table it extends; the admin read role gets no access to it.
ALTER TABLE agent_webhook_setups ADD COLUMN page_hash bytea;
