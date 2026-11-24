-- An emit's text, hashed (appapi.textFingerprint), beside its fingerprint
-- (125): a repeat of an id with another text is another emit, refused with
-- 409 id_reused. A column of its own, not a change to fingerprint's
-- format, so the previous revision, comparing fingerprint alone while both
-- serve, still matches the rows this one writes, and this one still
-- matches the rows it wrote. NULL (a publish by URL, a row written before
-- this column or by the previous revision, a redacted delete_on_delivery
-- emit) matches any text. Not granted to the admin read role.
ALTER TABLE topic_events ADD COLUMN text_fingerprint bytea;
