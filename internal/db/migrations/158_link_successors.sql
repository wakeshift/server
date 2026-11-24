-- successor_id: the link a renewal (the grace path, streams.renewExpired)
-- minted in this one's place. A wait in loop mode (wait.go: loop=<id>)
-- on a renewed link follows it to the newest of its chain, so the link file
-- an agent wrote once keeps working without being rewritten. A new
-- nullable column: no table rewrite, and the previous revision ignores it.
ALTER TABLE stream_links ADD COLUMN successor_id uuid;
