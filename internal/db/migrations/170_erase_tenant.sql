-- Erasing a workspace (appapi.eraseWorkspace, when an app's user is
-- forgotten) deletes its audit log through this function instead of
-- deleting the rows itself, so the server's own role needs no DELETE on
-- audit_log: with an owner (WAKESHIFT_MIGRATE_DATABASE_URL), the server's
-- role is refused it (db.migrateAsOwner) and can't delete audit rows
-- outside an erasure, as 030_forget_users.sql's app.erasing_tenant alone
-- let it.
--
-- It runs as its owner (SECURITY DEFINER), the role that migrates, and
-- deletes only the transaction's own workspace (current_tenant(), with
-- row-level security still applying), and only together with the workspace
-- itself: after the audit log it deletes the tenants row, which fails while
-- anything else of the workspace is left (their foreign keys), undoing the
-- whole call. So it can't take a live workspace's audit log, only finish
-- erasing an otherwise empty one. Only the server's role may call it
-- (granted in db.migrateAsOwner), not PUBLIC: the admin service's read
-- role mustn't. Its search path is pg_catalog, then pg_temp, and every
-- table and function it names outside pg_catalog is schema-qualified, so
-- an object a caller could create in public or pg_temp is never what it
-- runs.
CREATE OR REPLACE FUNCTION erase_tenant() RETURNS void
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    t uuid := public.current_tenant();
BEGIN
    IF t IS NULL THEN
        RAISE EXCEPTION 'erase_tenant: no workspace (app.tenant_id) is set';
    END IF;
    PERFORM set_config('app.erasing_tenant', t::text, true);
    DELETE FROM public.audit_log WHERE tenant_id = t;
    PERFORM set_config('app.erasing_tenant', '', true);
    DELETE FROM public.tenants WHERE id = t;
END $$;

REVOKE ALL ON FUNCTION erase_tenant() FROM PUBLIC;
