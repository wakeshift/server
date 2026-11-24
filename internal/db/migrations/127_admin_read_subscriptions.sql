-- The admin service's Harnesses page (docs/admin.md, "Harnesses"): active
-- subscriptions counted by the harness of the conversation that holds
-- them, by app, and new ones a day. It reads a subscription's workspace,
-- ID (to find its app in topic_members), agent and session (to find the
-- harness: the agent's ID is the harness's for a client Wakeshift
-- recognizes, and sessions.harness, already granted, records an accepted
-- claim), status and creation time. IDs, a state and a time: never the
-- event type's filters, the reason or who created it. The admin_read
-- policy on subscriptions is already in place (091_admin_read.sql).
GRANT SELECT (tenant_id, id, agent_id, session_id, status, created_at) ON subscriptions TO wakeshift_admin_read;
