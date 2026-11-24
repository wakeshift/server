-- wait_tasks: the lease holder of each app's newest MCP wait task in the
-- session, by app ID ({"<app>": "<holder>"}). wait_task stays the
-- session's newest task of any app (the one holding its wait); this is
-- what a new task of the same app confirms, so one app's wait task never
-- confirms what another app's handed out (internal/appapi/tasks.go). Only
-- read by the release that adds it; the previous revision ignores it.
ALTER TABLE sessions ADD COLUMN wait_tasks jsonb NOT NULL DEFAULT '{}'::jsonb;
