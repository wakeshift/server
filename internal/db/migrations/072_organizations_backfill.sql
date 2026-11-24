-- Organisations' backfill (docs/organizations.md), apart from 071 so it can
-- run again: every statement skips what's already done, so a second run
-- changes nothing (TestOrganizationsBackfill runs it twice). The next
-- release runs a copy of it, for rows the previous revision wrote while
-- this one was deploying.
--
-- None of these tables holds tenant data, so the loop over tenants in
-- AGENTS.md's data-migration rule doesn't apply.

-- 1. A personal organisation for every user (not just those with apps, so
--    the console always has one to show), named after them, with them as
--    its owner.
WITH made AS (
    INSERT INTO organizations (name, personal_of, created_by, created_at)
    SELECT left(coalesce(nullif(btrim(u.name), ''), u.email), 100), u.id, u.id, u.created_at
    FROM users u
    WHERE NOT EXISTS (SELECT 1 FROM organizations o WHERE o.personal_of = u.id)
    RETURNING id, personal_of, created_at
), members AS (
    INSERT INTO organization_members (org_id, user_id, role, created_at)
    SELECT id, personal_of, 'owner', created_at FROM made
)
INSERT INTO org_log (org_id, at, actor, action, detail)
SELECT id, created_at, 'hub', 'org.created', '{"personal": true, "backfilled": true}' FROM made;

-- 2. Each console app belongs to its creator's personal organisation. Our
--    own apps (WAKESHIFT_APPS, owner_id IS NULL) keep org_id IS NULL:
--    nobody manages them in the console.
UPDATE apps a SET org_id = o.id, created_by = coalesce(a.created_by, a.owner_id)
FROM organizations o
WHERE o.personal_of = a.owner_id AND a.owner_id IS NOT NULL AND a.org_id IS NULL;

-- 3. The app log's existing entries name their app's organisation.
UPDATE app_log l SET org_id = a.org_id
FROM apps a
WHERE a.id = l.app_id AND a.org_id IS NOT NULL AND l.org_id IS NULL;
