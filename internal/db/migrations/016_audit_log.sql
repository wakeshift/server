-- The audit log: who consented to what, and what was revoked, in each
-- workspace. Rows are written in the same transaction as the action, so one
-- exists exactly when the action committed. actor is "user:<id>",
-- "app:<id>", "client:<id>" or "hub"; detail is small (a grant's consent
-- screen stays in grants.shown, referenced by grant_id).
CREATE TABLE audit_log (
    tenant_id uuid        NOT NULL,
    id        bigint      GENERATED ALWAYS AS IDENTITY,
    at        timestamptz NOT NULL DEFAULT now(),
    actor     text        NOT NULL,
    action    text        NOT NULL, -- e.g. grant.created, grant.revoked, signin.revoked
    agent_id  text,
    app_id    text,
    grant_id  uuid,
    detail    jsonb       NOT NULL DEFAULT '{}',
    PRIMARY KEY (tenant_id, id)
);
ALTER TABLE audit_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON audit_log
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());

-- Append-only: the app (which owns the table, so a REVOKE wouldn't bind it)
-- can't change or remove what was recorded.
CREATE FUNCTION audit_log_append_only() RETURNS trigger
    LANGUAGE plpgsql
    AS $$ BEGIN RAISE EXCEPTION 'audit_log is append-only'; END $$;
CREATE TRIGGER audit_log_append_only BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_append_only();
CREATE TRIGGER audit_log_no_truncate BEFORE TRUNCATE ON audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION audit_log_append_only();
