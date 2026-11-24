-- The wait endpoint's loop mode (internal/streams, wait.go: loop=<id>): a
-- shell loop that runs the same one-line wait again and again, the server
-- keeping what today's loop keeps in shell variables. Per stream:
--   loop_id: the loop (the id its requests carry) whose wait took the
--     stream last; NULL once anything else takes it.
--   loop_flushed: the holder of that loop's last batch, once its answer was
--     written in full. The loop's next round confirms that batch, but only
--     while the holder still holds the stream and the batch is still leased.
--   loop_replaced: the last loop another loop took the stream from. Its next
--     round, while the newer one is live, is refused (410), so two loops
--     on one link don't take the stream from each other forever.
--   loop_since: when loop_id first took the stream, so a Claude Code
--     loop ends itself (410, hub.restart) before Claude Code's 30-minute
--     limit on a Monitor stops it mid-round.
-- New nullable columns: no table rewrite, and the previous revision
-- ignores them.
ALTER TABLE stream_leases ADD COLUMN loop_id text;
ALTER TABLE stream_leases ADD COLUMN loop_flushed text;
ALTER TABLE stream_leases ADD COLUMN loop_replaced text;
ALTER TABLE stream_leases ADD COLUMN loop_since timestamptz;
