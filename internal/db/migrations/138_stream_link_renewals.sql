-- renewed_at: a session link that expired while nothing used it (a laptop
-- asleep, the network down) was renewed through the grace path
-- (streams.renewExpired): its holder got a successor, and the link itself
-- was revoked at the same moment (revoked_at), so it renews at most once.
-- Kept apart from revoked_at so a request on it says it was renewed, not
-- revoked by someone. A new nullable column: no table rewrite, and the
-- previous revision ignores it.
ALTER TABLE stream_links ADD COLUMN renewed_at timestamptz;
