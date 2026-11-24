-- Link claims (appapi/linkclaim.go; docs/harnesses.md, "Link claims"): a
-- one-time code in a background command that the command itself exchanges
-- for its conversation's link the first time it runs, so the agent never
-- writes the link to its file with a file tool whose approval shows it
-- (#1152). A claim is a session_setups row naming the link it hands over;
-- it's used up, expires and is deleted as a setup URL is, and revoking a
-- session's links uses up its claims with its setup URLs. Rows without
-- link_id are setup URLs, as before.
ALTER TABLE session_setups ADD COLUMN link_id uuid;
