package core

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
)

// One of the expiry pass's jobs failing (a topic's expiry, say) doesn't roll
// back another's work, though they share a transaction: the expired grant
// is still revoked.
func TestExpiryJobsCommitSeparately(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	tenant := dbtest.NewTenant(t, d)
	if _, err := d.Pool.Exec(ctx, `INSERT INTO apps (id, name) VALUES ('pacer', 'Pacer')`); err != nil {
		t.Fatal(err)
	}
	var grant string
	err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := agents.Create(ctx, tx, "codex", ""); err != nil {
			return err
		}
		grant = dbtest.Grant(t, tx, "pacer", "codex", -time.Minute)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	saved := expiryJobs
	t.Cleanup(func() { expiryJobs = saved })
	failed := false
	expiryJobs = []expiryJob{
		{"failing", "", func(context.Context, pgx.Tx, expiryPass) (int64, error) {
			failed = true
			return 0, errors.New("deadlock detected")
		}},
		saved[0],
		{"failing after", "", func(context.Context, pgx.Tx, expiryPass) (int64, error) { return 0, errors.New("deadlock detected") }},
	}
	(&Core{db: d}).expireAll(ctx)
	var revoked bool
	d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM grants WHERE id = $1`, grant).Scan(&revoked)
	})
	if !failed || !revoked {
		t.Fatalf("failing job ran %v; grant revoked %v", failed, revoked)
	}
}

// Every expiry job runs bounded: its statements by jobStatementTimeout,
// its transaction by jobTransactionTimeout. In production a job's statement
// that planned badly ran for 25 hours, holding its locks and the snapshot
// vacuum had to keep.
func TestExpiryJobsAreBounded(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	dbtest.NewTenant(t, d)
	saved := expiryJobs
	t.Cleanup(func() { expiryJobs = saved })
	var statement, transaction string
	expiryJobs = []expiryJob{{"reading its bounds", "", func(ctx context.Context, tx pgx.Tx, _ expiryPass) (int64, error) {
		if err := tx.QueryRow(ctx, `SELECT current_setting('statement_timeout'), current_setting('transaction_timeout')`).Scan(&statement, &transaction); err != nil {
			return 0, err
		}
		return 0, nil
	}}}
	(&Core{db: d}).expireAll(ctx)
	if statement != "30s" || transaction != "2min" {
		t.Fatalf("a job ran with statement_timeout %q, transaction_timeout %q", statement, transaction)
	}
}

// expiryWorkspaces makes n workspaces and returns their IDs in ID order.
func expiryWorkspaces(t *testing.T, d *db.DB, n int) []string {
	t.Helper()
	var ids []string
	for range n {
		ids = append(ids, dbtest.NewTenant(t, d))
	}
	slices.Sort(ids)
	return ids
}

// The pass runs every job of a workspace in one transaction, not one each.
func TestExpiryPassOneTransactionPerWorkspace(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	tenants := expiryWorkspaces(t, d, 2)
	saved := expiryJobs
	t.Cleanup(func() { expiryJobs = saved })
	txids := map[string]map[int64]bool{}
	record := func(ctx context.Context, tx pgx.Tx, _ expiryPass) (int64, error) {
		var tenant string
		var txid int64
		if err := tx.QueryRow(ctx, `SELECT current_tenant()::text, txid_current()`).Scan(&tenant, &txid); err != nil {
			return 0, err
		}
		if txids[tenant] == nil {
			txids[tenant] = map[int64]bool{}
		}
		txids[tenant][txid] = true
		return 0, nil
	}
	expiryJobs = []expiryJob{{"one", "", record}, {"two", "", record}, {"three", "", record}}
	(&Core{db: d}).expireAll(ctx)
	for _, tenant := range tenants {
		if len(txids[tenant]) != 1 {
			t.Fatalf("workspace %s: jobs ran in %d transactions, want 1", tenant, len(txids[tenant]))
		}
	}
}

// A pass that stops part-way (a deploy, a scale-in) is resumed by the next
// pass after the last workspace it saved, not from the first workspace;
// once a pass finishes, the next starts from the first again.
func TestExpiryPassResumes(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	tenants := expiryWorkspaces(t, d, 4)
	saved, savedEvery := expiryJobs, expirySaveEvery
	t.Cleanup(func() { expiryJobs, expirySaveEvery = saved, savedEvery })
	expirySaveEvery = 0 // save after every workspace
	var visited []string
	stopAt := tenants[2]
	passCtx, stop := context.WithCancel(ctx)
	expiryJobs = []expiryJob{{"visiting", "", func(ctx context.Context, tx pgx.Tx, _ expiryPass) (int64, error) {
		var tenant string
		if err := tx.QueryRow(ctx, `SELECT current_tenant()::text`).Scan(&tenant); err != nil {
			return 0, err
		}
		if tenant == stopAt {
			stop() // the instance stops in the middle of this workspace
			return 0, ctx.Err()
		}
		visited = append(visited, tenant)
		return 0, nil
	}}}
	c := &Core{db: d}
	c.expireAll(passCtx)
	if !slices.Equal(visited, tenants[:2]) {
		t.Fatalf("first pass visited %v, want %v", visited, tenants[:2])
	}
	var after string
	if err := d.Pool.QueryRow(ctx, `SELECT coalesce(resume_after::text, '') FROM expiry_state`).Scan(&after); err != nil || after != tenants[1] {
		t.Fatalf("saved %q %v, want %s", after, err, tenants[1])
	}

	// Another instance resumes with the workspace that was interrupted.
	visited, stopAt = nil, ""
	(&Core{db: d}).expireAll(ctx)
	if !slices.Equal(visited, tenants[2:]) {
		t.Fatalf("resumed pass visited %v, want %v", visited, tenants[2:])
	}
	if err := d.Pool.QueryRow(ctx, `SELECT coalesce(resume_after::text, '') FROM expiry_state`).Scan(&after); err != nil || after != "" {
		t.Fatalf("after a finished pass, saved %q %v", after, err)
	}
	visited = nil
	c.expireAll(ctx)
	if !slices.Equal(visited, tenants) {
		t.Fatalf("next pass visited %v, want every workspace %v", visited, tenants)
	}
}

// The usage rollup runs at most once an hour and the full stranded check
// once a day, whichever instance ran the last: what the pass last did is
// in the database, not in the instance that did it.
func TestExpiryPassPeriodicJobsSurviveInstances(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	expiryWorkspaces(t, d, 2)
	saved, savedUsage := expiryJobs, usageJob
	t.Cleanup(func() { expiryJobs, usageJob = saved, savedUsage })
	rollups, full, partial := 0, 0, 0
	usageJob = expiryJob{usageJob.name, "", func(context.Context, pgx.Tx, expiryPass) (int64, error) { rollups++; return 0, nil }}
	expiryJobs = []expiryJob{{strandedJobName, "", func(_ context.Context, _ pgx.Tx, p expiryPass) (int64, error) {
		if p.strandedAll {
			full++
		} else {
			partial++
		}
		return 0, nil
	}}}
	(&Core{db: d}).expireAll(ctx)
	if rollups != 2 || full != 2 || partial != 0 {
		t.Fatalf("first pass: %d rollups, %d full and %d recent stranded checks; want 2, 2, 0", rollups, full, partial)
	}
	(&Core{db: d}).expireAll(ctx) // a new instance
	if rollups != 2 || full != 2 || partial != 2 {
		t.Fatalf("second pass: %d rollups, %d full and %d recent stranded checks; want 2, 2, 2", rollups, full, partial)
	}
	// An hour on, the rollup is due again; the full check isn't.
	if _, err := d.Pool.Exec(ctx, `UPDATE expiry_state SET rolled_up_at = now() - interval '61 minutes'`); err != nil {
		t.Fatal(err)
	}
	(&Core{db: d}).expireAll(ctx)
	if rollups != 4 || full != 2 || partial != 4 {
		t.Fatalf("an hour on: %d rollups, %d full and %d recent stranded checks; want 4, 2, 4", rollups, full, partial)
	}
	// A rollup that failed in a workspace is run again on the next pass.
	usageJob.run = func(context.Context, pgx.Tx, expiryPass) (int64, error) {
		rollups++
		return 0, errors.New("canceling statement due to statement timeout")
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE expiry_state SET rolled_up_at = now() - interval '61 minutes'`); err != nil {
		t.Fatal(err)
	}
	(&Core{db: d}).expireAll(ctx)
	(&Core{db: d}).expireAll(ctx)
	if rollups != 8 {
		t.Fatalf("after a failed rollup: %d rollups, want 8 (both passes)", rollups)
	}
}

// When the workspace's transaction itself fails (its transaction_timeout
// ends the session; here the session is ended outright), each job runs again in a
// transaction of its own, so the grants still get revoked.
func TestExpiryPassFallsBackToATransactionPerJob(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	tenant := dbtest.NewTenant(t, d)
	if _, err := d.Pool.Exec(ctx, `INSERT INTO apps (id, name) VALUES ('pacer', 'Pacer')`); err != nil {
		t.Fatal(err)
	}
	var grant string
	if err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := agents.Create(ctx, tx, "codex", ""); err != nil {
			return err
		}
		grant = dbtest.Grant(t, tx, "pacer", "codex", -time.Minute)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	saved := expiryJobs
	t.Cleanup(func() { expiryJobs = saved })
	slow := 0
	expiryJobs = []expiryJob{saved[0], {"slow", "", func(ctx context.Context, tx pgx.Tx, _ expiryPass) (int64, error) {
		slow++
		if slow > 1 {
			return 0, nil // on its own, quick
		}
		// As transaction_timeout does: the session ends.
		_, err := tx.Exec(ctx, `SELECT pg_terminate_backend(pg_backend_pid())`)
		return 0, err
	}}}
	(&Core{db: d}).expireAll(ctx)
	var revoked bool
	if err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM grants WHERE id = $1`, grant).Scan(&revoked)
	}); err != nil {
		t.Fatal(err)
	}
	if slow != 2 || !revoked {
		t.Fatalf("slow job ran %d times (want 2: together, then alone); grant revoked %v", slow, revoked)
	}
}

// A rollup that failed in a workspace before the pass was interrupted is
// remembered by the pass that resumes it: the rollup isn't recorded as done.
func TestExpiryPassRemembersFailuresAcrossInstances(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	tenants := expiryWorkspaces(t, d, 3)
	saved, savedUsage, savedEvery := expiryJobs, usageJob, expirySaveEvery
	t.Cleanup(func() { expiryJobs, usageJob, expirySaveEvery = saved, savedUsage, savedEvery })
	expirySaveEvery = 0
	current := func(ctx context.Context, tx pgx.Tx) string {
		var tenant string
		tx.QueryRow(ctx, `SELECT current_tenant()::text`).Scan(&tenant)
		return tenant
	}
	passCtx, stop := context.WithCancel(ctx)
	expiryJobs = []expiryJob{{"stopping", "", func(ctx context.Context, tx pgx.Tx, _ expiryPass) (int64, error) {
		if current(ctx, tx) == tenants[1] {
			stop()
			return 0, ctx.Err()
		}
		return 0, nil
	}}}
	usageJob = expiryJob{usageJob.name, "", func(ctx context.Context, tx pgx.Tx, _ expiryPass) (int64, error) {
		if current(ctx, tx) == tenants[0] {
			return 0, errors.New("canceling statement due to statement timeout")
		}
		return 0, nil
	}}
	(&Core{db: d}).expireAll(passCtx)
	stop = func() {}
	(&Core{db: d}).expireAll(ctx)
	var rolled *time.Time
	if err := d.Pool.QueryRow(ctx, `SELECT rolled_up_at FROM expiry_state`).Scan(&rolled); err != nil || rolled != nil {
		t.Fatalf("rolled up at %v %v; want never (a workspace's rollup failed)", rolled, err)
	}
}
