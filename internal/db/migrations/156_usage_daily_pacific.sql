-- usage_daily's days become Pacific dates (America/Los_Angeles), the day the
-- admin service shows (docs/admin.md, "Usage data"). Nothing here moves a
-- row: the rollup (deliveries.RollUpUsage) reads when this migration was
-- applied. A delivery finished before the UTC midnight that began that day
-- keeps its UTC date; any later one gets its Pacific date. So the days
-- before stay as they were, the day before that midnight runs on to the
-- next Pacific midnight (31 or 32 hours), and no delivery is counted twice
-- or left out. The rollup rewrites yesterday and today each hour, so it
-- writes that long day itself.
COMMENT ON COLUMN usage_daily.day IS 'The Pacific date (America/Los_Angeles) the deliveries finished; a UTC date before 156_usage_daily_pacific.sql (deliveries.RollUpUsage)';
