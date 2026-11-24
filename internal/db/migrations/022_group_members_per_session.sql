-- A group member is a conversation: one of an agent's sessions (two Claude
-- Code conversations are two players), or the agent as a whole when it
-- joined without one. Each has its own handle in the group.
ALTER TABLE group_members DROP CONSTRAINT group_members_pkey;
ALTER TABLE group_members ADD COLUMN session_id uuid, ADD COLUMN session_handle text NOT NULL DEFAULT '';
CREATE UNIQUE INDEX group_members_key ON group_members (type, tenant_id, agent_id, coalesce(session_id, '00000000-0000-0000-0000-000000000000'));
