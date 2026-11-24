-- App assertions' IDs (jti), until the assertion expires, so a replay is
-- refused by every instance, not only the one that saw it first
-- (apps.Registry.Authenticate). Platform-level: app IDs, opaque IDs and
-- times only. The expiry pass deletes expired rows.
CREATE TABLE app_assertion_ids (
    app_id     text        NOT NULL,
    jti        text        NOT NULL,
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (app_id, jti)
);
CREATE INDEX app_assertion_ids_expiry ON app_assertion_ids (expires_at);
