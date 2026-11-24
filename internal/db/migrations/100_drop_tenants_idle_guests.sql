-- migrate:no-transaction
-- Legacy guests' workspaces ("guest:…", from the SDKs' guest sign-in) are
-- gone: forgotten by the expiry pass in October 2026, and the pass no longer
-- looks for them (appapi.ExpireIdle). Their partial index (050) goes.
DROP INDEX CONCURRENTLY IF EXISTS tenants_idle_guests;
