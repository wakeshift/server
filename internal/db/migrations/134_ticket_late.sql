-- A connector ticket that may be redeemed late (appapi.lateRedeemable):
-- its beacon went where the model never sees it and is bound to the tool
-- call (Claude Code's _meta), so a waiter that finds it in the
-- conversation's own record after the hooks were installed may still
-- redeem it, within the hour, instead of asking the agent to call the
-- app's tool again (which may have side effects). Unredeemed tickets are
-- kept that long (appapi.CleanUp).
ALTER TABLE connector_tickets ADD COLUMN late_ok boolean NOT NULL DEFAULT false;
