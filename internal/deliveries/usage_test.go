package deliveries_test

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/deliveries"
)

func TestUsageBucket(t *testing.T) {
	for ms, want := range map[float64]int{0: 0, 9.9: 0, 10: 1, 14.9: 1, 15: 2, 22.6: 3, 1e12: deliveries.UsageBuckets - 1} {
		if got := deliveries.UsageBucket(ms); got != want {
			t.Errorf("UsageBucket(%v) = %d, want %d", ms, got, want)
		}
	}
	for b := 0; b < deliveries.UsageBuckets-1; b++ {
		// Just under a bucket's upper bound is in it; at the bound, the next.
		if up := deliveries.UsageBucketUpper(b); deliveries.UsageBucket(up*0.999) != b || deliveries.UsageBucket(up*1.001) != b+1 {
			t.Errorf("bucket %d's bound %v", b, up)
		}
	}
	if !math.IsInf(deliveries.UsageBucketUpper(deliveries.UsageBuckets-1), 1) {
		t.Error("the last bucket is unbounded")
	}
}

// The rollup counts each day's finished deliveries by state, with a
// histogram of the delivered ones' ages that adds up to them, and running it
// again changes nothing.
func TestRollUpUsage(t *testing.T) {
	agentIDs := make([]string, 8)
	for i := range agentIDs {
		agentIDs[i] = fmt.Sprintf("agent-%d", i)
	}
	d, tenant, _ := redactSetup(t, false, agentIDs...)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	yesterday := now.Add(-24 * time.Hour)
	// state, finished at, age
	set := []struct {
		state    string
		finished time.Time
		age      time.Duration
	}{
		{"delivered", now.Add(-time.Hour), 5 * time.Millisecond},       // bucket 0
		{"acked", now.Add(-time.Hour), 12 * time.Millisecond},          // bucket 1
		{"acked", now.Add(-2 * time.Hour), 3 * time.Second},            // bucket 15: 10*1.5^14 = 2919 ms
		{"dead", now.Add(-time.Hour), time.Hour},                       //
		{"cancelled", now.Add(-time.Hour), time.Minute},                //
		{"delivered", yesterday.Add(time.Hour), 20 * time.Millisecond}, // yesterday, bucket 2
		{"delivered", now.Add(-50 * time.Hour), time.Millisecond},      // two days ago: not counted
		{"pending", now, 0}, // not finished
	}
	in(t, d, tenant, func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT id::text FROM deliveries ORDER BY agent_id`)
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for i, s := range set {
			var fin *time.Time
			if s.state != "pending" {
				f := s.finished
				fin = &f
			}
			if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = $2, finished_at = $3, created_at = $4 WHERE id = $1`,
				ids[i], s.state, fin, s.finished.Add(-s.age)); err != nil {
				return err
			}
		}
		return nil
	})
	check := func() {
		t.Helper()
		in(t, d, tenant, func(tx pgx.Tx) error {
			n, err := deliveries.RollUpUsage(ctx, tx, now)
			if err != nil || n != 2 {
				t.Fatalf("rolled up %d rows, %v", n, err)
			}
			return nil
		})
		type row struct {
			delivered, dead, cancelled int
			ages                       []int32
		}
		got := map[string]row{}
		rows, _ := d.Pool.Query(ctx, `SELECT day::text, delivered, dead, cancelled, age_ms FROM usage_daily WHERE tenant_id = $1`, tenant)
		for rows.Next() {
			var day string
			var r row
			if err := rows.Scan(&day, &r.delivered, &r.dead, &r.cancelled, &r.ages); err != nil {
				t.Fatal(err)
			}
			got[day] = r
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		today, yday := got["2026-10-23"], got["2026-10-22"]
		if len(got) != 2 || today.delivered != 3 || today.dead != 1 || today.cancelled != 1 || yday.delivered != 1 || yday.dead != 0 {
			t.Fatalf("usage: %+v", got)
		}
		sum := func(a []int32) (n int32) {
			for _, x := range a {
				n += x
			}
			return n
		}
		if len(today.ages) != deliveries.UsageBuckets || sum(today.ages) != 3 || sum(yday.ages) != 1 {
			t.Fatalf("histograms: %v %v", today.ages, yday.ages)
		}
		if today.ages[0] != 1 || today.ages[1] != 1 || today.ages[deliveries.UsageBucket(3000)] != 1 || yday.ages[deliveries.UsageBucket(20)] != 1 {
			t.Fatalf("buckets: today %v, yesterday %v", today.ages, yday.ages)
		}
	}
	check()
	check() // idempotent
}

// From the day 156_usage_daily_pacific.sql was applied, a delivery counts on
// its Pacific date; one finished before that day's UTC midnight keeps its
// UTC date, so the day before runs on to the next Pacific midnight and every
// delivery counts once, whichever day the rollup ran.
func TestRollUpUsagePacific(t *testing.T) {
	d, tenant, _ := redactSetup(t, false, "agent-0", "agent-1", "agent-2", "agent-3", "agent-4", "agent-5")
	// Applied 2026-11-14 05:00 UTC (Pacific: 11-13 21:00): from 11-14 00:00
	// UTC, days are Pacific (PST, UTC-8).
	if _, err := dbtest.Owner(t, d).Exec(ctx, `UPDATE schema_migrations SET applied_at = '2026-11-14T05:00:00Z' WHERE name = '156_usage_daily_pacific.sql'`); err != nil {
		t.Fatal(err)
	}
	finished := []string{
		"2026-11-12T23:00:00Z", // UTC 11-12
		"2026-11-13T01:00:00Z", // UTC 11-13 (Pacific would be 11-12)
		"2026-11-14T03:00:00Z", // Pacific 11-13 19:00: the long day
		"2026-11-14T09:00:00Z", // Pacific 11-14 01:00
		"2026-11-15T07:00:00Z", // Pacific 11-14 23:00
		"2026-11-15T09:00:00Z", // Pacific 11-15 01:00
	}
	in(t, d, tenant, func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT id::text FROM deliveries ORDER BY agent_id`)
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for i, f := range finished {
			if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'delivered', finished_at = $2, created_at = $2::timestamptz - interval '1 second' WHERE id = $1`,
				ids[i], f); err != nil {
				return err
			}
		}
		return nil
	})
	got := func() map[string]int {
		rows, _ := d.Pool.Query(ctx, `SELECT day::text, delivered FROM usage_daily WHERE tenant_id = $1`, tenant)
		m := map[string]int{}
		for rows.Next() {
			var day string
			var n int
			if err := rows.Scan(&day, &n); err != nil {
				t.Fatal(err)
			}
			m[day] = n
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return m
	}
	for _, c := range []struct {
		now  string
		want map[string]int
	}{
		// Before the change: UTC days (the long day 11-13 also holds the
		// delivery after 11-14 00:00 UTC: here it has already finished).
		{"2026-11-13T12:00:00Z", map[string]int{"2026-11-12": 1, "2026-11-13": 2}},
		// Pacific 11-13 18:00: yesterday (UTC 11-12) and the long day so far.
		{"2026-11-14T02:00:00Z", map[string]int{"2026-11-12": 1, "2026-11-13": 2}},
		// Pacific 11-14 23:30: the long day, and Pacific 11-14.
		{"2026-11-15T07:30:00Z", map[string]int{"2026-11-12": 1, "2026-11-13": 2, "2026-11-14": 2}},
		// Pacific 11-15 01:30: Pacific 11-14 and 11-15.
		{"2026-11-15T09:30:00Z", map[string]int{"2026-11-12": 1, "2026-11-13": 2, "2026-11-14": 2, "2026-11-15": 1}},
	} {
		now, _ := time.Parse(time.RFC3339, c.now)
		in(t, d, tenant, func(tx pgx.Tx) error {
			_, err := deliveries.RollUpUsage(ctx, tx, now)
			return err
		})
		if g := got(); fmt.Sprint(g) != fmt.Sprint(c.want) {
			t.Fatalf("rolled up at %s: %v, want %v", c.now, g, c.want)
		}
	}
}

func TestUsageDay(t *testing.T) {
	for in, want := range map[string]string{
		"2026-11-13T07:59:00Z": "2026-11-12", // PST
		"2026-11-13T08:00:00Z": "2026-11-13",
		"2026-07-01T06:59:00Z": "2026-06-30", // PDT
		"2026-07-01T07:00:00Z": "2026-07-01",
	} {
		at, _ := time.Parse(time.RFC3339, in)
		if got := deliveries.UsageDay(at); got.Format(time.DateOnly) != want || got.Location() != time.UTC || got.Hour() != 0 {
			t.Errorf("UsageDay(%s) = %v, want %s", in, got, want)
		}
	}
}

// Pruning deletes days past retention, and the rows of workspaces that are
// gone (left by an erasure before erasing deleted them); a live
// workspace's recent days stay.
func TestPruneUsage(t *testing.T) {
	d, tenant, _ := redactSetup(t, false, "agent-0")
	now := time.Now()
	if _, err := d.Pool.Exec(ctx, `INSERT INTO usage_daily (day, tenant_id, app_id, delivered, dead, cancelled, age_ms) VALUES
		(current_date - 1, $1, NULL, 1, 0, 0, '{}'),
		(current_date - 401, $1, NULL, 1, 0, 0, '{}'),
		(current_date - 1, gen_random_uuid(), 'gone', 1, 0, 0, '{}')`, tenant); err != nil {
		t.Fatal(err)
	}
	if n, err := deliveries.PruneUsage(ctx, d.Pool, now, 100); err != nil || n != 2 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	var left int
	d.Pool.QueryRow(ctx, `SELECT count(*) FROM usage_daily WHERE tenant_id = $1 AND day = current_date - 1`, tenant).Scan(&left)
	var all int
	d.Pool.QueryRow(ctx, `SELECT count(*) FROM usage_daily`).Scan(&all)
	if left != 1 || all != 1 {
		t.Fatalf("left %d of the live workspace's, %d in all", left, all)
	}
}
