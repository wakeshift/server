-- Every key's kid (docs/api-keys.md, "Finding the app without its ID"):
-- the first 16 bytes of the public key's SHA-256, base64url (22
-- characters; appkey.KeyID). SDKs put it in every assertion's header, and
-- the server finds the key, and so the app, by it: unique across every
-- app's keys. The console's key_id is its first 8 characters.
--
-- Written by insertKey and Sync from now on; NOT NULL comes in a later
-- release, once no revision writes a key without it.
ALTER TABLE app_keys ADD COLUMN kid text;
UPDATE app_keys SET kid = translate(rtrim(encode(substring(sha256(public_key) FROM 1 FOR 16), 'base64'), '='), '+/', '-_');
CREATE UNIQUE INDEX app_keys_kid ON app_keys (kid);
