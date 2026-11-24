-- take (001) with two more limits, so one sender can't use up the global
-- hour for everyone:
--
--   * Posts per sending address (an IPv6 address by its /64), across every
--     webhook, in the hour: k_addr, a counter keyed by an HMAC of the
--     address, as new links per address are (server.ts). NULL: none.
--   * The global hour's last share is kept for webhooks agents use: once
--     the global count reaches global_idle, a post to a webhook with no
--     watching conversation and no MCP session seen since used_since is
--     refused, as if the global hour were over.
--
-- Counts a post against a webhook URL's day and hour, its address's hour
-- and the global hour, all or none. Returns 0 if allowed, else which limit
-- it's over: 1 the URL's day, 2 its hour, 3 the global hour, 4 the
-- address's hour. Rows are locked in one order (the URL's, the address's,
-- then the global one), as untake gives them back, so concurrent calls
-- can't deadlock, and a post over its URL's or address's limit never waits
-- on the global row.
CREATE FUNCTION take_post(h text, k_day text, k_hour text, k_addr text, k_global text, day_end timestamptz, hour_end timestamptz,
                          per_day integer, per_hour integer, per_addr integer, global_per_hour integer, global_idle integer,
                          used_since timestamptz) RETURNS integer
LANGUAGE plpgsql AS $$
DECLARE
    d integer;
    hr integer;
    a integer;
    g integer;
BEGIN
    -- Each limit is its own row: keys that named the same row would count
    -- a post against it twice, or check one limit against another's count.
    -- The callers' keys can't collide (state.ts: <hook>/<check>/d and /h,
    -- with the hook 20 hex digits; ip/<hmac>/posts/h; global/h); this makes
    -- sure, rather than counting wrong.
    IF k_day IS NULL OR k_hour IS NULL OR k_global IS NULL
       OR k_day = k_hour OR k_global IN (k_day, k_hour)
       OR k_addr IN (k_day, k_hour, k_global) THEN
        RAISE EXCEPTION 'take_post: each limit needs a key of its own';
    END IF;
    INSERT INTO counters (key, until, n) VALUES (k_day, day_end, 0), (k_hour, hour_end, 0) ON CONFLICT DO NOTHING;
    SELECT n INTO d FROM counters WHERE key = k_day AND until = day_end FOR UPDATE;
    SELECT n INTO hr FROM counters WHERE key = k_hour AND until = hour_end FOR UPDATE;
    IF d >= per_day THEN RETURN 1; END IF;
    IF hr >= per_hour THEN RETURN 2; END IF;
    IF k_addr IS NOT NULL THEN
        INSERT INTO counters (key, until, n) VALUES (k_addr, hour_end, 0) ON CONFLICT DO NOTHING;
        SELECT n INTO a FROM counters WHERE key = k_addr AND until = hour_end FOR UPDATE;
        IF a >= per_addr THEN RETURN 4; END IF;
    END IF;
    INSERT INTO counters (key, until, n) VALUES (k_global, hour_end, 0) ON CONFLICT DO NOTHING;
    SELECT n INTO g FROM counters WHERE key = k_global AND until = hour_end FOR UPDATE;
    IF g >= global_per_hour THEN RETURN 3; END IF;
    IF g >= global_idle
       AND NOT EXISTS (SELECT 1 FROM watchers WHERE hook = h)
       AND NOT EXISTS (SELECT 1 FROM sessions WHERE hook = h AND last_seen >= used_since) THEN
        RETURN 3;
    END IF;
    -- One statement per row, each by its own key (the rows are locked already).
    UPDATE counters SET n = n + 1 WHERE key = k_day AND until = day_end;
    UPDATE counters SET n = n + 1 WHERE key = k_hour AND until = hour_end;
    IF k_addr IS NOT NULL THEN
        UPDATE counters SET n = n + 1 WHERE key = k_addr AND until = hour_end;
    END IF;
    UPDATE counters SET n = n + 1 WHERE key = k_global AND until = hour_end;
    RETURN 0;
END
$$;
