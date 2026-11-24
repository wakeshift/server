-- Organisations, release N+1 (docs/organizations.md, "Rollout", step 6):
-- apps.owner_id is neither written nor read from this release; org_id says
-- who manages an app, and created_by who made it. Only a console app (one
-- with an organisation) can be deleted, so the check that said so by
-- owner_id (025) says it by org_id: an app this release registers has no
-- owner_id. The previous revision (v1.1.0) writes org_id with owner_id on
-- every app it registers, and deletes only those, so it passes too. The next
-- release drops owner_id and its index, apps_owner.
ALTER TABLE apps DROP CONSTRAINT IF EXISTS apps_only_owned_deleted;
ALTER TABLE apps ADD CONSTRAINT apps_only_org_deleted CHECK (deleted_at IS NULL OR org_id IS NOT NULL);
