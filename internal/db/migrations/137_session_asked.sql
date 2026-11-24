-- asked_harness: the harness whose full install ask (or Claude Code's
-- how-to-be-notified question) a subscribing result gave this
-- conversation, once. A later subscribing result of the same conversation,
-- while nothing wakes it, points back to that ask in one sentence instead
-- of repeating it (harnesses.AskedNote, harnesses.PluginAskedNote). NULL:
-- never asked. A new nullable column: no table rewrite, and the previous
-- revision ignores it.
ALTER TABLE sessions ADD COLUMN asked_harness text;
