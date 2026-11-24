-- The app log's and the admin log's entries for a forgotten user
-- (app.user_forgotten) carried user_sha256, an unkeyed SHA-256 of
-- "<app>:<user>": a user ID that can be guessed, such as an email, could be
-- matched to it. The core no longer writes it. This strips it from the
-- entries written before, and a trigger strips it from any entry written
-- by the previous revision while this one rolls out (it still serves for
-- up to 50 minutes), or later.
--
-- app_log and admin_log are platform-level (no row-level security), so
-- this sees every row. admin_log is append-only: its trigger is off only
-- for this one update, inside this transaction.

CREATE FUNCTION strip_user_sha256() RETURNS trigger
    LANGUAGE plpgsql
    AS $$ BEGIN NEW.detail := NEW.detail - 'user_sha256'; RETURN NEW; END $$;
CREATE TRIGGER app_log_strip_user_sha256 BEFORE INSERT ON app_log
    FOR EACH ROW WHEN (NEW.detail ? 'user_sha256') EXECUTE FUNCTION strip_user_sha256();
CREATE TRIGGER admin_log_strip_user_sha256 BEFORE INSERT ON admin_log
    FOR EACH ROW WHEN (NEW.detail ? 'user_sha256') EXECUTE FUNCTION strip_user_sha256();

UPDATE app_log SET detail = detail - 'user_sha256' WHERE detail ? 'user_sha256';

ALTER TABLE admin_log DISABLE TRIGGER admin_log_append_only;
UPDATE admin_log SET detail = detail - 'user_sha256' WHERE detail ? 'user_sha256';
ALTER TABLE admin_log ENABLE TRIGGER admin_log_append_only;
