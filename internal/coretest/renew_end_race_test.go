package coretest_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/coretest"
)

// A subscribe that renews a conversation's subscription while the app ends
// it (event.end: a game's close sparing a player's next join, #535) either
// renews it before the end, which then ends it, or comes after the end and
// makes a new one. It never answers "renewed" for a subscription the end
// finished meanwhile: the renewal found the record before the end committed,
// waited on its routing, then refreshed the ended rows, and the conversation
// was told it was subscribed with nothing routing to it.
func TestRenewingWhileTheAppEndsTheSubscription(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call := caller{User: "rae", Client: "claude-code"}
	first, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	call.Session = first.Session
	tenant, sub, _ := noteRow(t, c, first.ID)
	ctx := context.Background()
	// The test's own connections, outside the core's pool: holding one of
	// its few connections (four on CI's runners, some kept by its
	// listeners) would leave the core's requests waiting for a connection
	// rather than for the locks this test is about.
	conn := func() *pgx.Conn {
		t.Helper()
		cn, err := pgx.ConnectConfig(ctx, c.DB.Pool.Config().ConnConfig.Copy())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cn.Close(context.Background()) })
		return cn
	}
	mon, hold := conn(), conn()

	// blockedBy is the backend a lock of pid's keeps waiting (the core's
	// background work can wait on locks too, so a count of waiters isn't
	// enough).
	blockedBy := func(pid int) int {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			var got int
			err := mon.QueryRow(ctx, `SELECT pid FROM pg_stat_activity WHERE datname = current_database() AND $1 = ANY(pg_blocking_pids(pid)) LIMIT 1`, pid).Scan(&got)
			if err == nil {
				return got
			}
			if err != pgx.ErrNoRows {
				t.Fatal(err)
			}
			if time.Now().After(deadline) {
				rows, _ := mon.Query(ctx, `SELECT format('%s %s %s/%s blocked by %s: %s', pid, state, wait_event_type, wait_event, pg_blocking_pids(pid), left(query, 160))
					FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid()`)
				backends, _ := pgx.CollectRows(rows, pgx.RowTo[string])
				t.Fatalf("nothing waits on backend %d; backends:\n%s", pid, strings.Join(backends, "\n"))
			}
		}
	}

	// The routing's row held, so the end stops part-way: it holds the
	// record, and waits here.
	held, release, holder := make(chan int), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	let := func() { once.Do(func() { close(release) }) }
	t.Cleanup(let) // a failed wait doesn't leave it holding
	go func() {
		holder <- pgx.BeginFunc(ctx, hold, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
				return err
			}
			var pid int
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid() FROM (SELECT 1 FROM topic_subscriptions WHERE subscription_id = $1::uuid FOR NO KEY UPDATE) l`, sub).Scan(&pid); err != nil {
				return err
			}
			held <- pid
			<-release
			return nil
		})
	}()
	holderPID := <-held
	ended := make(chan error, 1)
	go func() {
		ok, err := endNote(app, "rae", first.ID)
		if err == nil && !ok {
			t.Error("the end ended nothing")
		}
		ended <- err
	}()
	endPID := blockedBy(holderPID) // the end, at the routing's row
	type renewal struct {
		sub noteSub
		err error
	}
	renewed := make(chan renewal, 1)
	go func() {
		s, err := subscribe(app, call, "ci/acme", subOpts{})
		renewed <- renewal{s, err}
	}()
	blockedBy(endPID) // the renewal, on what the end holds
	let()
	if err := <-holder; err != nil {
		t.Fatal(err)
	}
	if err := <-ended; err != nil {
		t.Fatalf("ending: %v", err)
	}
	r := <-renewed
	if r.err != nil {
		t.Fatalf("renewing: %v", r.err)
	}
	if !noteLive(t, c, r.sub.ID) {
		t.Fatalf("the renewal answered created: %v, but its subscription isn't live", r.sub.Created)
	}
	if r.sub.ID == first.ID || !r.sub.Created {
		t.Errorf("the renewal came after the end, so it's a new subscription: %+v", r.sub)
	}
}
