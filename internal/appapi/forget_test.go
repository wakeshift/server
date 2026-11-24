package appapi_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/events"
)

// Forgetting a user while an event is being appended in their workspace
// waits for the append, rather than deadlocking with it: both take the
// workspace's event sequence first, then its row.
func TestForgetUserWaitsForAnAppend(t *testing.T) {
	d, tenant := quizzerUser(t)
	ctx := context.Background()
	ev := events.Event{Type: "test.ping", Source: "test", From: events.Party{Kind: "hub", ID: "hub"}, Trust: events.Internal}
	if err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error { _, _, err := events.Append(ctx, tx, ev); return err }); err != nil {
		t.Fatal(err)
	}
	appended, forgot := make(chan struct{}), make(chan error, 1)
	api := &appapi.API{DB: d, Issuer: "https://wakeshift.test"}
	err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		// An append between taking the sequence and inserting the event
		// (one statement in events.Append; held open here).
		if _, err := tx.Exec(ctx, `UPDATE event_counters SET last_seq = last_seq`); err != nil {
			return err
		}
		close(appended)
		go func() {
			_, err := api.ForgetUser(ctx, "quizzer", "bob", "app:quizzer")
			forgot <- err
		}()
		if err := waitForALockWait(ctx, d, tx); err != nil { // the forget is waiting now
			return err
		}
		_, _, err := events.Append(ctx, tx, ev)
		return err
	})
	<-appended
	if err != nil {
		t.Fatalf("the append: %v", err)
	}
	if err := <-forgot; err != nil {
		t.Fatalf("the forget: %v", err)
	}
	var left int
	d.Pool.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE id = $1`, tenant).Scan(&left)
	if left != 0 {
		t.Fatal("the workspace is still there")
	}
}

// Forgetting a user while a publish is being copied into their workspace
// waits for the copy, rather than deadlocking with it: both take the
// workspace's row of the publishes still to be copied first, then its
// event sequence.
func TestForgetUserWaitsForAPublishCopy(t *testing.T) {
	d, tenant := quizzerUser(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	ev := events.Event{Type: "test.ping", Source: "test", From: events.Party{Kind: "hub", ID: "hub"}, Trust: events.Internal}
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error { _, _, err := events.Append(ctx, tx, ev); return err }))
	_, err := d.Pool.Exec(ctx, `INSERT INTO topic_events (app_id, source_id, event_id, topics, text, matched) VALUES ('quizzer', 's1', gen_random_uuid(), '{u/bob/x}', 'hi', 0)`)
	must(err)
	_, err = d.Pool.Exec(ctx, `INSERT INTO topic_fanout (app_id, source_id, tenant_id, subscriptions) VALUES ('quizzer', 's1', $1, '{}')`, tenant)
	must(err)

	api := &appapi.API{DB: d, Issuer: "https://wakeshift.test"}
	forgot := make(chan error, 1)
	copyErr := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		// copyInto's first two statements, held open while the forget
		// starts.
		if _, err := tx.Exec(ctx, `SELECT subscriptions::text[] FROM topic_fanout WHERE app_id = 'quizzer' AND source_id = 's1' AND tenant_id = $1 FOR UPDATE`, tenant); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM topic_events WHERE app_id = 'quizzer' AND source_id = 's1' FOR UPDATE`); err != nil {
			return err
		}
		go func() {
			_, err := api.ForgetUser(ctx, "quizzer", "bob", "app:quizzer")
			forgot <- err
		}()
		if err := waitForALockWait(ctx, d, tx); err != nil { // the forget is waiting now
			return err
		}
		// Then the copy's append in the workspace.
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
			return err
		}
		_, _, err := events.Append(ctx, tx, ev)
		return err
	})
	ferr := <-forgot
	if copyErr != nil {
		t.Fatalf("the copy: %v", copyErr)
	}
	if ferr != nil {
		t.Fatalf("the forget: %v", ferr)
	}
	var left int
	d.Pool.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE id = $1`, tenant).Scan(&left)
	if left != 0 {
		t.Fatal("the workspace is still there")
	}
}

// Forgetting a user while an agent's request locks its grant and then
// appends an event (a.tx) still forgets them: the two deadlock (the
// erasure holds the event sequence and deletes the grant), and Postgres
// cancels one of them. It's usually the erasure, which waited first, and
// it's tried again once the request is done; if it's the request (which
// fails), the erasure goes on. Either way the user is forgotten.
func TestForgetUserRetriesAfterADeadlock(t *testing.T) {
	d, tenant := quizzerUser(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var grant string
	ev := events.Event{Type: "test.ping", Source: "test", From: events.Party{Kind: "hub", ID: "hub"}, Trust: events.Internal}
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, _, err := events.Append(ctx, tx, ev); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) VALUES (current_tenant(), 'claude-code')`); err != nil {
			return err
		}
		grant = dbtest.Grant(t, tx, "quizzer", "claude-code", 24*time.Hour)
		return nil
	}))

	api := &appapi.API{DB: d, Issuer: "https://wakeshift.test"}
	forgot := make(chan error, 1)
	var forgotten bool
	postErr := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		// a.tx's first statement, held open while the forget starts.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM grants WHERE id = $1 FOR SHARE`, grant); err != nil {
			return err
		}
		go func() {
			var err error
			forgotten, err = api.ForgetUser(ctx, "quizzer", "bob", "app:quizzer")
			forgot <- err
		}()
		if err := waitForALockWait(ctx, d, tx); err != nil { // the forget is waiting now
			return err
		}
		_, _, err := events.Append(ctx, tx, ev)
		return err
	})
	ferr := <-forgot
	var pgErr *pgconn.PgError
	if postErr != nil && !(errors.As(postErr, &pgErr) && pgErr.Code == "40P01") {
		t.Fatalf("the agent's request: %v", postErr)
	} else if postErr != nil {
		t.Log("Postgres canceled the request, not the erasure")
	}
	if ferr != nil || !forgotten {
		t.Fatalf("the forget: %v (forgotten %v)", ferr, forgotten)
	}
	var left int
	d.Pool.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE id = $1`, tenant).Scan(&left)
	if left != 0 {
		t.Fatal("the workspace is still there")
	}
}

// waitForALockWait waits until another connection to the test's database
// is waiting for a lock that holder's transaction holds: a deterministic
// "the forget is blocked now", where a fixed pause is too short on a slow
// runner.
func waitForALockWait(ctx context.Context, d *db.DB, holder pgx.Tx) error {
	var pid int
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		return err
	}
	for deadline := time.Now().Add(15 * time.Second); ; {
		var waiting bool
		if err := d.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database() AND $1::int = ANY (pg_blocking_pids(pid)))`, pid).Scan(&waiting); err != nil {
			return err
		}
		if waiting {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("nothing waited for the transaction's locks")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// quizzerUser is a database with the app quizzer and the workspace it
// keeps for its user bob, for the tests of what waits for what.
func quizzerUser(t *testing.T) (*db.DB, string) {
	t.Helper()
	d := dbtest.New(t)
	ctx := context.Background()
	if _, err := d.Pool.Exec(ctx, `INSERT INTO apps (id, name) VALUES ('quizzer', 'Quizzer')`); err != nil {
		t.Fatal(err)
	}
	var tenant string
	if err := d.Pool.QueryRow(ctx, `INSERT INTO tenants (name, kind, app_id, external_id) VALUES ('bob', 'app', 'quizzer', 'bob') RETURNING id::text`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	return d, tenant
}
