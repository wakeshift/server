-- wait_command: Wakeshift's wait command on the machine whose hooks
-- connected the session (`/Users/<name>/.wakeshift/bin/wakeshift wait`),
-- as the connector said when it redeemed (docs/harnesses.md, "Pre-approving
-- Wakeshift's commands"). Harnesses woken by a command the agent starts in
-- the background are then told to run it instead of a loop on the
-- session's wait URL: the same in every conversation, with no secret in
-- it, so the harness's allowlist rule (which the install added) matches
-- it. NULL until a connector that has one redeems. Only read by the
-- release that adds it; the previous revision ignores it.
ALTER TABLE sessions ADD COLUMN wait_command text;
