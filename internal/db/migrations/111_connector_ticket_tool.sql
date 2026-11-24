-- tool: the MCP tool whose result carried the ticket's beacon (beacon.Claims.Tool;
-- '' if the app didn't say). A result holds back a fresh beacon while one
-- minted for the same tool hasn't been redeemed (appapi.recentTicket), not
-- while any has: the result that told the user to install (watch's) mustn't
-- keep the next tool's result, the first a newly installed hook sees, from
-- carrying one. The previous revision doesn't write it ('').
ALTER TABLE connector_tickets ADD COLUMN IF NOT EXISTS tool text NOT NULL DEFAULT '';
