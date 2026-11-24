-- A conversation that ended up with two sessions of the same app in one
-- workspace (its first calls in parallel, before either result gave it a
-- session_key) keeps one: its hook's redeem moves the other's
-- subscriptions into it (appapi.mergeInto). merged_into is the session a
-- merged one's subscriptions went to, so the session_key the conversation
-- was given for it still finds them.
ALTER TABLE sessions ADD COLUMN merged_into uuid;
