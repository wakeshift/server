-- The Codex thread an OpenClaw conversation's turns ran in when it
-- subscribed through Codex's MCP client (OpenClaw with its bundled codex
-- plugin; the call's _meta x-codex-turn-metadata.thread_id). In a user's
-- workspace, a later call from that thread that passes no session_key
-- still finds the session (appapi.resolve), so its result can carry the
-- beacon that lets a plugin installed in between connect it (issue #1125).
ALTER TABLE sessions ADD COLUMN codex_thread text;
CREATE INDEX sessions_codex_thread ON sessions (tenant_id, agent_id, codex_thread) WHERE codex_thread IS NOT NULL;
