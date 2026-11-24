-- An emit's fingerprint (appapi.emitFingerprint): a SHA-256 of its event,
-- the topic its arguments render, its recipients and its payload, as
-- canonical JSON. An emit that repeats an id within the day it dedupes
-- returns the first's result only when the fingerprints match, and is
-- refused (409 id_reused) when they differ. NULL for publishes by URL, rows
-- written before this column (or by the previous revision while both
-- serve), and once a delete_on_delivery emit's content is redacted: a NULL
-- matches any retry, as every repeat did before. Not granted to the admin
-- read role.
ALTER TABLE topic_events ADD COLUMN fingerprint bytea;
