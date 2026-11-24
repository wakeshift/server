-- slide_until: a session link that stays usable while it's used. Its
-- expires_at moves forward (SessionLinkTTL from each use) up to
-- slide_until, which its token carries as its expiry; unused for
-- SessionLinkTTL it expires as before. NULL: a link that doesn't slide.
-- So an agent's wait command can stay the same for its session, and a
-- harness's "always allow" for it keeps matching.
ALTER TABLE stream_links ADD COLUMN slide_until timestamptz;

-- connected_note_due: Wakeshift's hooks just connected the session, and
-- its agent hasn't been told by Wakeshift's core yet (a harness whose hook
-- notes reach the model late, such as Claude Code's): the next tool result
-- or the waiter's last line says so, once.
ALTER TABLE sessions ADD COLUMN connected_note_due boolean NOT NULL DEFAULT false;

-- handed_at: when a tool result last handed the link out (minted, or a
-- sliding link handed out again). used_at now moves with each use, so a
-- link handed out and not used since (handed_at after used_at) is one the
-- agent has and hasn't started: a tool result doesn't repeat it.
ALTER TABLE stream_links ADD COLUMN handed_at timestamptz NOT NULL DEFAULT now();
