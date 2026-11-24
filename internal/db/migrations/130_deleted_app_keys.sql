-- A deleted app is kept without its keys (site/legal/privacy.md): deleting
-- an app now deletes its keys (apps.DeleteKeys), where it only revoked
-- them. What's kept is a one-way fingerprint of each deleted key's kid
-- (SHA-256 of the kid), nothing else: no public key, no app ID, so
-- registering a deleted key again is still refused (409 key_used,
-- docs/api-keys.md), as it was while the key's row stayed.
--
-- Platform-level, like app_keys: no tenant_id and no row-level security.
CREATE TABLE deleted_key_ids (
    kid_hash   bytea       PRIMARY KEY,
    deleted_at timestamptz NOT NULL DEFAULT now()
);

-- Apps deleted before: their keys' fingerprints first, then the keys. A key
-- made before kid was written has its kid from its public key (090's
-- formula, appkey.KeyID). app_keys is platform-level, so this sees every
-- app's. The app log keeps the entries naming them (by key_id, not a
-- reference).
INSERT INTO deleted_key_ids (kid_hash)
SELECT sha256(convert_to(coalesce(k.kid, translate(rtrim(encode(substring(sha256(k.public_key) FROM 1 FOR 16), 'base64'), '='), '+/', '-_')), 'UTF8'))
FROM app_keys k JOIN apps a ON a.id = k.app_id WHERE a.deleted_at IS NOT NULL
ON CONFLICT DO NOTHING;
DELETE FROM app_keys k USING apps a WHERE a.id = k.app_id AND a.deleted_at IS NOT NULL;
