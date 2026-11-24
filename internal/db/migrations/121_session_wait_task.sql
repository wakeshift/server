-- wait_task: the lease holder of the conversation's newest MCP wait task
-- (internal/waittask, docs/harnesses.md "Claude Code: MCP tasks as a wake
-- path"), derived from the task's signed ID; wait_task_seen_at: when its
-- client last asked about it (tasks/get) or started it. A task whose
-- holder isn't the session's newest was replaced, and starting a new one
-- confirms the batch the previous one handed out (the agent read it and
-- called again). NULL until the session's first wait task. Only read by
-- the release that adds it; the previous revision ignores it.
ALTER TABLE sessions ADD COLUMN wait_task text;
ALTER TABLE sessions ADD COLUMN wait_task_seen_at timestamptz;
