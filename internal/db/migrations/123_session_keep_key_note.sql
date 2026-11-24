-- keep_key_note_due: the conversation just unsubscribed from something in
-- a wrapped tool's call, so that call's result says its session key stays
-- the same (harnesses.KeepSessionNote), once: the next result Wakeshift
-- adds to clears it. Only read by the release that adds it; the previous
-- revision ignores it.
ALTER TABLE sessions ADD COLUMN keep_key_note_due boolean NOT NULL DEFAULT false;
