-- When an app last acted for one of its workspaces (resolving a call for
-- its user, returning it, minting its agent's token), at most hourly
-- (db.Touch): guest workspaces idle for long are forgotten
-- (appapi.ExpireIdleGuests). NULL: not since this column was added.
ALTER TABLE tenants ADD COLUMN active_at timestamptz;
CREATE INDEX tenants_idle_guests ON tenants ((coalesce(active_at, created_at)))
    WHERE kind = 'app' AND starts_with(external_id, 'guest:');
