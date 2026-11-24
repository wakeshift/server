package deliveries

// The daily usage rollup (usage_daily, 080_admin.sql): per workspace and
// Pacific day, how many deliveries finished in each state, and a histogram
// of how long the delivered ones took. Every index on deliveries begins
// with tenant_id, so a per-app or platform count over the table would scan
// all of it; the expiry pass instead counts each workspace's on
// deliveries_finished and writes the totals here, for usage reports
// (the admin API) to read.

import (
	"context"
	"errors"
	"math"
	"time"
	_ "time/tzdata" // UsageZone, whatever zoneinfo the image has

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UsageBuckets is how many buckets a usage_daily histogram has: one under
// 10 ms, 40 growing by 1.5x from there (#240's delivery-age metric's
// buckets, so the two read alike), and one for anything longer.
const UsageBuckets = 42

// UsageRetention is how long usage_daily keeps a day.
const UsageRetention = 400 * 24 * time.Hour

// UsageZone is the time zone of usage_daily's days, and of every day the
// usage report shows: Pacific time.
var UsageZone = func() *time.Location {
	l, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		panic(err)
	}
	return l
}()

// usageZoneMigration is the migration from which usage_daily's days are
// Pacific dates; they were UTC dates before it (usageDays).
const usageZoneMigration = "156_usage_daily_pacific.sql"

// UsageDay is the Pacific date of t, as pgx reads a date (UTC midnight):
// usage_daily's day for a delivery finished at t, and the admin service's.
func UsageDay(t time.Time) time.Time {
	p := t.In(UsageZone)
	return time.Date(p.Year(), p.Month(), p.Day(), 0, 0, 0, 0, time.UTC)
}

// usageDays maps instants to usage_daily's days, given pacificFrom: the
// UTC midnight that began the (UTC) day usageZoneMigration was applied
// (zero: not applied). A delivery finished before it keeps its UTC date, as
// the rollup wrote it then; any later one gets its Pacific date
// (UsageDay). So the day before pacificFrom runs from its UTC midnight to
// the next Pacific midnight (31 or 32 hours), and no delivery counts on two
// days or on none.
type usageDays struct{ pacificFrom time.Time }

// day is the usage_daily day of a delivery finished at t.
func (u usageDays) day(t time.Time) time.Time {
	if u.pacificFrom.IsZero() || t.Before(u.pacificFrom) {
		return t.UTC().Truncate(24 * time.Hour)
	}
	return UsageDay(t)
}

// start is the first instant of day d (a date, at UTC midnight).
func (u usageDays) start(d time.Time) time.Time {
	if u.pacificFrom.IsZero() || d.Before(u.pacificFrom) {
		return d
	}
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, UsageZone)
}

// sql is day's expression for the timestamp col, with pacificFrom and the
// zone's name as the parameters $from and $zone (zero pacificFrom: pass a
// time after every delivery).
func (usageDays) sql(col, from, zone string) string {
	return `CASE WHEN ` + col + ` < ` + from + ` THEN (` + col + ` AT TIME ZONE 'UTC')::date ELSE (` + col + ` AT TIME ZONE ` + zone + `)::date END`
}

// readUsageDays reads when usage_daily's days became Pacific.
func readUsageDays(ctx context.Context, tx pgx.Tx) (usageDays, error) {
	var applied time.Time
	err := tx.QueryRow(ctx, `SELECT applied_at FROM schema_migrations WHERE name = $1`, usageZoneMigration).Scan(&applied)
	if errors.Is(err, pgx.ErrNoRows) {
		return usageDays{}, nil
	}
	return usageDays{pacificFrom: applied.UTC().Truncate(24 * time.Hour)}, err
}

// UsageBucket is the histogram bucket for a delivery that took ms.
func UsageBucket(ms float64) int {
	if ms < 10 {
		return 0
	}
	b := 1 + int(math.Floor(math.Log(ms/10)/math.Log(1.5)))
	return min(b, UsageBuckets-1)
}

// UsageBucketUpper is the upper bound of bucket b, in ms (+Inf for the last).
func UsageBucketUpper(b int) float64 {
	if b >= UsageBuckets-1 {
		return math.Inf(1)
	}
	return 10 * math.Pow(1.5, float64(b))
}

// RollUpUsage writes the workspace's (tx's) deliveries finished yesterday
// and today (usage_daily's days: Pacific, usageDays) to usage_daily, with
// absolute counts: running
// it again changes nothing but updated_at. A day with no finished
// deliveries gets no row. It reports how many rows it wrote.
//
// It holds the workspace's row (FOR KEY SHARE) until tx ends, so a
// workspace being erased (forgetting its user, deleting its app), which
// deletes its rows here, waits for it or is waited for: no row is written
// for a workspace that's gone.
func RollUpUsage(ctx context.Context, tx pgx.Tx, now time.Time) (int64, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = current_tenant() FOR KEY SHARE)`).Scan(&exists); err != nil || !exists {
		return 0, err
	}
	u, err := readUsageDays(ctx, tx)
	if err != nil {
		return 0, err
	}
	// Yesterday's and today's deliveries, each counted on its day as
	// usageDays.day gives it.
	today := u.day(now)
	from, to := u.start(today.AddDate(0, 0, -1)), u.start(today.AddDate(0, 0, 1))
	pacificFrom := u.pacificFrom
	if pacificFrom.IsZero() {
		pacificFrom = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	type day struct {
		delivered, dead, cancelled int
		billable                   int
		ages                       [UsageBuckets]int
	}
	days := map[time.Time]*day{}
	get := func(d time.Time) *day {
		if days[d] == nil {
			days[d] = &day{}
		}
		return days[d]
	}
	// The bucket is computed in Postgres so only counts come back; the
	// formula is UsageBucket's.
	rows, err := tx.Query(ctx, `
		SELECT `+u.sql("finished_at", "$4", "$5")+` AS day, state,
		       CASE WHEN state IN ('delivered', 'acked') THEN
		            CASE WHEN extract(epoch FROM finished_at - created_at) * 1000 < 10 THEN 0
		                 ELSE least($3::int - 1, 1 + floor(ln(extract(epoch FROM finished_at - created_at) * 1000 / 10) / ln(1.5))::int) END
		       END AS bucket,
		       count(*)
		FROM deliveries
		WHERE tenant_id = current_tenant() AND state IN ('delivered', 'acked', 'dead', 'cancelled')
		  AND finished_at >= $1 AND finished_at < $2
		GROUP BY 1, 2, 3`, from, to, UsageBuckets, pacificFrom, UsageZone.String())
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var d time.Time
		var state string
		var bucket *int
		var n int
		if err := rows.Scan(&d, &state, &bucket, &n); err != nil {
			rows.Close()
			return 0, err
		}
		c := get(d)
		switch state {
		case "delivered", "acked":
			c.delivered += n
			if bucket != nil && *bucket >= 0 && *bucket < UsageBuckets {
				c.ages[*bucket] += n
			}
		case "dead":
			c.dead += n
		case "cancelled":
			c.cancelled += n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	// Billable: an event reaching an agent, once however many of its
	// conversations got it. A multi-recipient emit's direct copies are
	// separate events, one per subscription, that name the emit ($emit);
	// any other event counts as itself (a publish's copy in a workspace is
	// one event, whatever it woke).
	rows, err = tx.Query(ctx, `
		SELECT `+u.sql("d.finished_at", "$3", "$4")+` AS day,
		       count(DISTINCT (coalesce(e.attrs->>'$emit', e.id::text), d.agent_id))
		FROM deliveries d JOIN events e ON e.tenant_id = d.tenant_id AND e.id = d.event_id
		WHERE d.tenant_id = current_tenant() AND d.state IN ('delivered', 'acked')
		  AND d.finished_at >= $1 AND d.finished_at < $2
		GROUP BY 1`, from, to, pacificFrom, UsageZone.String())
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var d time.Time
		var n int
		if err := rows.Scan(&d, &n); err != nil {
			rows.Close()
			return 0, err
		}
		get(d).billable = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var written int64
	for d, c := range days {
		_, err := tx.Exec(ctx, `INSERT INTO usage_daily (day, tenant_id, app_id, delivered, dead, cancelled, age_ms, billable, updated_at)
			SELECT $1::date, t.id, t.app_id, $2, $3, $4, $5, $6, now() FROM tenants t WHERE t.id = current_tenant()
			ON CONFLICT (day, tenant_id) DO UPDATE SET app_id = excluded.app_id, delivered = excluded.delivered, dead = excluded.dead,
				cancelled = excluded.cancelled, age_ms = excluded.age_ms, billable = excluded.billable, updated_at = excluded.updated_at`,
			d, c.delivered, c.dead, c.cancelled, c.ages[:], c.billable)
		if err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// PruneUsage deletes usage_daily's days older than UsageRetention, and the
// rows of workspaces that are gone, at most limit rows of each. Erasing a
// workspace (forgetting its user, deleting its app) deletes its rows
// itself; this catches any an older revision left behind, while it still
// serves during a rollout.
func PruneUsage(ctx context.Context, pool *pgxpool.Pool, now time.Time, limit int) (int64, error) {
	tag, err := pool.Exec(ctx, `DELETE FROM usage_daily WHERE ctid IN (SELECT ctid FROM usage_daily WHERE day < $1::date LIMIT $2)`,
		now.UTC().Add(-UsageRetention), limit)
	if err != nil {
		return 0, err
	}
	n := tag.RowsAffected()
	tag, err = pool.Exec(ctx, `DELETE FROM usage_daily WHERE ctid IN (SELECT u.ctid FROM usage_daily u
		WHERE NOT EXISTS (SELECT 1 FROM tenants t WHERE t.id = u.tenant_id) LIMIT $1)`, limit)
	return n + tag.RowsAffected(), err
}
