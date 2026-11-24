-- migrate:no-transaction
-- The hourly workspace rate counts every new workspace an app makes
-- (tenants_app_created, 099), not only guests' and conversations'; no
-- revision reads this index (056) any more.
DROP INDEX CONCURRENTLY IF EXISTS tenants_unvouched_created;
