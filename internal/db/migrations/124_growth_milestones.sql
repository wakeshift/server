-- Growth milestones (docs/admin.md, "Growth milestones"): the admin
-- service's Slack notifier posts an app's and an organization's first
-- wake-up, and each milestone of wake-ups after it (admin.Milestones), once.
-- A wake-up is an event reaching an agent: usage_daily.billable, the billing
-- unit (docs/pricing.md).
--
-- One row per milestone reached, inserted by the notifier in the poll's
-- transaction before it posts: the primary key makes a milestone's message
-- at most one, across restarts, replays and two revisions in a deploy. A
-- platform-level table, like admin_alert_state: app and organization IDs, a
-- number and times, never a workspace's data, so it's exempt from row-level
-- security (TestEveryTenantTableIsIsolated, AGENTS.md).
CREATE TABLE growth_milestones (
    subject     text        NOT NULL CHECK (subject IN ('app', 'org')),
    subject_id  text        NOT NULL,   -- apps.id, or organizations.id
    milestone   bigint      NOT NULL CHECK (milestone > 0),   -- 1: the first wake-up
    reached_on  date,                   -- the UTC day the count passed it, if known
    recorded_at timestamptz NOT NULL DEFAULT now(),
    posted      boolean     NOT NULL,   -- false: recorded without a message (reached before this table, or below one already recorded)
    PRIMARY KEY (subject, subject_id, milestone)
);

-- The admin service reads and inserts; it never updates or deletes a row.
GRANT SELECT, INSERT ON growth_milestones TO wakeshift_admin_read;

-- Milestones reached before this table are recorded without a message, so
-- the first deploy doesn't post every app's history. usage_daily is
-- platform-level (no row-level security), so this reads every workspace's
-- rows without looping over tenants. The milestones are admin.Milestones
-- as of this migration; the notifier records any later addition below an
-- app's highest without a message too. An organization counts only apps
-- that aren't ours, as the notifier does (admin.InternalApps as of this
-- migration).
INSERT INTO growth_milestones (subject, subject_id, milestone, posted)
SELECT 'app', u.app_id, m, false
FROM (SELECT app_id, sum(billable) AS n FROM usage_daily WHERE app_id IS NOT NULL GROUP BY app_id) u
CROSS JOIN unnest(ARRAY[1, 10, 100, 1000, 10000, 100000, 1000000, 10000000]::bigint[]) m
WHERE u.n >= m
ON CONFLICT DO NOTHING;

INSERT INTO growth_milestones (subject, subject_id, milestone, posted)
SELECT 'org', o.org_id, m, false
FROM (SELECT a.org_id::text AS org_id, sum(u.billable) AS n FROM usage_daily u JOIN apps a ON a.id = u.app_id
      WHERE a.org_id IS NOT NULL AND a.id NOT IN ('test-app', 'rehearsal', 'loadtest') GROUP BY a.org_id) o
CROSS JOIN unnest(ARRAY[1, 10, 100, 1000, 10000, 100000, 1000000, 10000000]::bigint[]) m
WHERE o.n >= m
ON CONFLICT DO NOTHING;
