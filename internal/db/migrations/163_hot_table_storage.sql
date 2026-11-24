-- migrate:no-transaction
-- Storage settings for the tables updated most (docs/capacity.md; the core
-- scalability review, finding 8). Every table had the defaults: vacuumed
-- once a fifth of its rows are dead, pages filled to the brim.
--
-- Small tables rewritten all the time (a pipeline step, a publish, a
-- delivery each update a row): vacuumed after a fixed number of dead rows,
-- not a share of a table this small, and pages left 30% free so an update
-- can stay on its page (a HOT update, with no new index entries).
--   grants: until grant_paces (165), every paced pipeline step updated
--     its grant; its 669 MB of dead versions need a repack, not this
--   subscription_cursors: every step advances its cursors
--   event_counters: every published event updates its workspace's counter
--   mcp_event_webhooks: every webhook delivery updates its webhook
--   pipeline_due: every published event marks its workspace
-- deliveries: the largest table, about four updates per delivery, vacuumed
-- at 2% dead rather than 20% (975,000 dead rows waited for a vacuum in
-- production), with pages 15% free for HOT updates (a lease renewal).
--
-- fillfactor applies to pages written from now on; it rewrites nothing.
-- Each ALTER takes SHARE UPDATE EXCLUSIVE on one table, which conflicts
-- only with vacuum and DDL (reads and writes go on), one table per
-- statement, so no two are held at once. Every statement is idempotent:
-- the migrator runs the file again from the top after a lock conflict.
SET lock_timeout = '3s';
ALTER TABLE grants SET (fillfactor = 70, autovacuum_vacuum_scale_factor = 0, autovacuum_vacuum_threshold = 500);
ALTER TABLE subscription_cursors SET (fillfactor = 70, autovacuum_vacuum_scale_factor = 0, autovacuum_vacuum_threshold = 1000);
ALTER TABLE event_counters SET (fillfactor = 70, autovacuum_vacuum_scale_factor = 0, autovacuum_vacuum_threshold = 500);
ALTER TABLE mcp_event_webhooks SET (fillfactor = 70, autovacuum_vacuum_scale_factor = 0, autovacuum_vacuum_threshold = 500);
ALTER TABLE pipeline_due SET (fillfactor = 70, autovacuum_vacuum_scale_factor = 0, autovacuum_vacuum_threshold = 500);
ALTER TABLE deliveries SET (fillfactor = 85, autovacuum_vacuum_scale_factor = 0.02, autovacuum_analyze_scale_factor = 0.02);
