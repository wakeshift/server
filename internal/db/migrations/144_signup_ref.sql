-- Where a sign-up came from (docs/analytics.md, "Sign-up refs"): a link
-- straight to the console can carry ?ref=<tag> (gbrain's guide, the
-- quickstart's console link, @wakeshift/cli's link). Sign-in carries the tag
-- in its signed flow cookie and saves it on the account it makes, asking
-- nothing; a sign-in that lands on the waitlist keeps it on the entry, and
-- the account made from that entry later takes it. A short tag
-- (accounts.CleanRef) or NULL; never a URL, never anything the person
-- typed, and never sent to PostHog.
--
-- users and waitlist are account tables, read before a workspace is
-- chosen, so exempt from row-level security (AGENTS.md). The admin
-- service's Sign-ups page reads the tag (its read role's column grants:
-- TestAdminReadRole).
--
-- Numbered 144, after main's 140-143 (the Grok Bot webhooks, #1021 and #1022); it
-- was 140_signup_ref.sql, then 143, on its branch, so IF NOT EXISTS lets a database
-- that applied it under either name runs it again.
ALTER TABLE users ADD COLUMN IF NOT EXISTS signup_ref text CHECK (signup_ref ~ '^[a-z0-9][a-z0-9_-]{0,31}$');
ALTER TABLE waitlist ADD COLUMN IF NOT EXISTS signup_ref text CHECK (signup_ref ~ '^[a-z0-9][a-z0-9_-]{0,31}$');

GRANT SELECT (signup_ref) ON users TO wakeshift_admin_read;
GRANT SELECT (signup_ref) ON waitlist TO wakeshift_admin_read;
