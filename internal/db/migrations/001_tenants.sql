-- Tenants are the unit of isolation. The tenants table itself isn't
-- tenant-scoped: workers list tenants to schedule work across them.
CREATE TABLE tenants (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- current_tenant() is the tenant InTenant set for this transaction, or NULL,
-- which matches no rows. Every tenant-scoped table's policy uses it:
--
--   ALTER TABLE t ENABLE ROW LEVEL SECURITY;
--   ALTER TABLE t FORCE ROW LEVEL SECURITY;
--   CREATE POLICY tenant_isolation ON t
--       USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE FUNCTION current_tenant() RETURNS uuid
    LANGUAGE sql STABLE
    AS $$ SELECT nullif(current_setting('app.tenant_id', true), '')::uuid $$;
