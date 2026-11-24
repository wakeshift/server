-- Where the expiry pass (internal/core, expireAll) stands, kept in the
-- database so it survives the instance running it: one row.
--
-- resume_after: the last workspace (by ID) a pass in progress saved
-- itself after, or NULL when the last pass finished. A pass a deploy or a
-- scale-in interrupted resumes after it, instead of starting again from
-- the first workspace (workspaces late in ID order would otherwise starve).
--
-- The pass's periodic jobs, each decided when a pass starts and kept for
-- the whole pass (a resumed pass keeps it): rolling_up, whether this pass
-- rolls up usage (at most once an hour, from rolled_up_at, whichever
-- instance ran the last one), and stranded_full, whether it checks every
-- ended subscription's waiting deliveries (daily, from stranded_full_at)
-- rather than only those of subscriptions ended in the last two days. A
-- workspace where either failed marks the pass (*_failed), so the next pass
-- runs it again instead of waiting its interval.
--
-- Platform-level (exempt in TestEveryTenantTableIsIsolated, AGENTS.md):
-- a workspace ID, flags and times, nothing about what is in the workspace.
CREATE TABLE expiry_state (
    id               boolean     PRIMARY KEY DEFAULT true CHECK (id),
    resume_after     uuid,
    rolling_up       boolean     NOT NULL DEFAULT false,
    rollup_failed    boolean     NOT NULL DEFAULT false,
    rolled_up_at     timestamptz,
    stranded_full    boolean     NOT NULL DEFAULT false,
    stranded_failed  boolean     NOT NULL DEFAULT false,
    stranded_full_at timestamptz
) WITH (fillfactor = 50);
INSERT INTO expiry_state DEFAULT VALUES;
