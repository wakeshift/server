-- Devices: the user's own native clients (Wakeshift for Mac). Each holds a
-- bearer token for the console API, minted when the user approves it in the
-- console and revocable there. A token names its tenant (wsd_<tenant>_…),
-- so it's looked up inside it, under row-level security. It stops working
-- if the user signs out everywhere (epoch) or leaves the workspace.
CREATE TABLE devices (
    tenant_id    uuid        NOT NULL,
    id           uuid        NOT NULL DEFAULT gen_random_uuid(),
    user_id      uuid        NOT NULL REFERENCES users(id),
    name         text        NOT NULL,
    token_hash   bytea       NOT NULL,
    epoch        int         NOT NULL, -- the user's session epoch when approved
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at   timestamptz,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, token_hash)
);
ALTER TABLE devices ENABLE ROW LEVEL SECURITY;
ALTER TABLE devices FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON devices
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
