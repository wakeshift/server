package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/subscriptions"
)

var ctx = context.Background()

type fixture struct {
	t      *testing.T
	d      *db.DB
	tenant string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	d := dbtest.New(t)
	tenant := dbtest.NewTenant(t, d)
	f := &fixture{t: t, d: d, tenant: tenant}
	f.tx(func(tx pgx.Tx) error {
		for _, a := range []string{"claude-code", "codex"} {
			if _, err := agents.Create(ctx, tx, a, ""); err != nil {
				return err
			}
		}
		return nil
	})
	return f
}

func (f *fixture) tx(fn func(tx pgx.Tx) error) {
	f.t.Helper()
	if err := f.d.InTenant(ctx, f.tenant, fn); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) subscribe(agent, typ string, filters map[string]string) subscriptions.Subscription {
	f.t.Helper()
	var s subscriptions.Subscription
	f.tx(func(tx pgx.Tx) (err error) {
		s, err = subscriptions.Create(ctx, tx, subscriptions.New{AgentID: agent, EventType: typ, Filters: filters, CreatedBy: "test"})
		return err
	})
	return s
}

func (f *fixture) slack(id, channel string) events.Event {
	f.t.Helper()
	var e events.Event
	f.tx(func(tx pgx.Tx) (err error) {
		e, _, err = events.Append(ctx, tx, events.Event{
			Type: "slack.message", Source: "slack", SourceEventID: id, Trust: events.External,
			From: events.Party{Kind: "slack_user", ID: "U1"}, Attrs: map[string]string{"channel": channel},
		})
		return err
	})
	return e
}

// owed lists an agent's deliveries by event source ID, in event order.
func (f *fixture) owed(agent string) []string {
	f.t.Helper()
	var ids []string
	f.tx(func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT e.source_event_id FROM deliveries d JOIN events e ON e.tenant_id = d.tenant_id AND e.id = d.event_id
			WHERE d.agent_id = $1 ORDER BY d.event_seq`, agent)
		var err error
		ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	return ids
}

func TestEventsBecomeDeliveries(t *testing.T) {
	f := setup(t)
	f.slack("before", "C1") // before the subscription: never delivered
	f.subscribe("claude-code", "slack.message", map[string]string{"channel": "C1"})
	f.subscribe("codex", "slack.*", nil)
	f.slack("e1", "C1")
	f.slack("e2", "C2") // filtered out for claude-code
	f.slack("e3", "C1")

	r := &Runner{DB: f.d}
	n, err := r.Tenant(ctx, f.tenant)
	if err != nil || n != 5 {
		t.Fatalf("created %d, %v", n, err)
	}
	if got := fmt.Sprint(f.owed("claude-code")); got != "[e1 e3]" {
		t.Fatalf("claude-code owed %s", got)
	}
	if got := fmt.Sprint(f.owed("codex")); got != "[e1 e2 e3]" {
		t.Fatalf("codex owed %s", got)
	}
	// Nothing new: running again creates nothing.
	if n, _ := r.Tenant(ctx, f.tenant); n != 0 {
		t.Fatalf("second run created %d", n)
	}
}

// due reports whether the tenant is marked for the sweep (pipeline_due).
func (f *fixture) due() bool {
	f.t.Helper()
	var n int
	if err := f.d.Pool.QueryRow(ctx, `SELECT count(*) FROM pipeline_due WHERE tenant_id = $1`, f.tenant).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n == 1
}

// A sweep visits only the tenants marked due (an event appended marks
// one; a sweep that leaves nothing behind unmarks it), and a full sweep
// every tenant: an event whose notification and mark were both lost waits
// for the full one.
func TestSweepVisitsMarkedTenants(t *testing.T) {
	f := setup(t)
	f.subscribe("claude-code", "slack.message", nil)
	f.slack("e1", "C1")
	if !f.due() {
		t.Fatal("an appended event didn't mark the tenant")
	}
	r := &Runner{DB: f.d}
	r.all(ctx, false)
	if got := fmt.Sprint(f.owed("claude-code")); got != "[e1]" {
		t.Fatalf("owed %s", got)
	}
	if f.due() {
		t.Fatal("still marked with nothing left")
	}

	f.slack("e2", "C1")
	if _, err := f.d.Pool.Exec(ctx, `DELETE FROM pipeline_due`); err != nil { // the mark lost
		t.Fatal(err)
	}
	r.all(ctx, false)
	if got := fmt.Sprint(f.owed("claude-code")); got != "[e1]" {
		t.Fatalf("an unmarked tenant was swept: owed %s", got)
	}
	r.all(ctx, true)
	if got := fmt.Sprint(f.owed("claude-code")); got != "[e1 e2]" {
		t.Fatalf("after a full sweep, owed %s", got)
	}
}

// A leased delivery keeps its tenant marked until it's finished, so an
// expired lease is reaped by the next sweep.
func TestLeasedDeliveriesKeepTheMark(t *testing.T) {
	f := setup(t)
	f.subscribe("claude-code", "slack.message", nil)
	f.slack("e1", "C1")
	r := &Runner{DB: f.d}
	r.all(ctx, false)
	if f.due() {
		t.Fatal("still marked with nothing left")
	}
	var claimed []deliveries.Delivery
	f.tx(func(tx pgx.Tx) (err error) {
		claimed, err = deliveries.Claim(ctx, tx, "claude-code", "", "holder", time.Millisecond, 10)
		return err
	})
	if len(claimed) != 1 || !f.due() {
		t.Fatalf("claimed %d, marked %v", len(claimed), f.due())
	}
	time.Sleep(10 * time.Millisecond)
	r.all(ctx, false) // reaps the expired lease; pending again, nothing leased
	var state string
	f.tx(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM deliveries`).Scan(&state)
	})
	if state != "pending" || f.due() {
		t.Fatalf("after the sweep: %s, marked %v", state, f.due())
	}
}

// A claim that commits while a sweep unmarks its tenant keeps the mark:
// the sweep waits for it and sees its lease, so the lease is reaped once it
// runs out (TestReapedDeliveryWakesTheStream in internal/streams), not at
// the next full sweep, 15 minutes on.
func TestAClaimDuringTheSweepKeepsTheMark(t *testing.T) {
	f := setup(t)
	f.subscribe("claude-code", "slack.message", nil)
	f.slack("e1", "C1")
	r := &Runner{DB: f.d}
	if _, err := r.Tenant(ctx, f.tenant); err != nil {
		t.Fatal(err)
	}
	if !f.due() {
		t.Fatal("not marked before the claim")
	}

	// A claim, not committed yet: the tenant is marked already, so its
	// mark changes nothing.
	tx, err := f.d.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenant); err != nil {
		t.Fatal(err)
	}
	if claimed, err := deliveries.Claim(ctx, tx, "claude-code", "", "holder", time.Minute, 10); err != nil || len(claimed) != 1 {
		t.Fatalf("claimed %d: %v", len(claimed), err)
	}

	// The sweep unmarks the tenant meanwhile (it waits for the claim, or
	// finishes first, seeing nothing leased); then the claim commits.
	settled := make(chan error, 1)
	go func() { settled <- r.settle(ctx, f.tenant) }()
	time.Sleep(300 * time.Millisecond)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-settled:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep didn't finish after the claim committed")
	}
	if !f.due() {
		t.Fatal("a delivery is leased, but its tenant is no longer marked for the sweep")
	}
}

func TestOneDeliveryPerAgentAndEvent(t *testing.T) {
	f := setup(t)
	f.subscribe("claude-code", "slack.message", nil)
	f.subscribe("claude-code", "slack.*", map[string]string{"channel": "C1"})
	f.slack("e1", "C1")
	(&Runner{DB: f.d}).Tenant(ctx, f.tenant)
	if got := f.owed("claude-code"); len(got) != 1 {
		t.Fatalf("owed %v", got)
	}
}

func TestBatchesAdvanceTheCursor(t *testing.T) {
	f := setup(t)
	f.subscribe("claude-code", "slack.message", nil)
	for i := range 7 {
		f.slack(fmt.Sprint("e", i), "C1")
	}
	(&Runner{DB: f.d, Batch: 3}).Tenant(ctx, f.tenant)
	if got := f.owed("claude-code"); len(got) != 7 {
		t.Fatalf("owed %v", got)
	}
}

// A crash before commit leaves nothing behind, and the next run does the
// work exactly once.
func TestCrashMidStepLosesAndDuplicatesNothing(t *testing.T) {
	f := setup(t)
	f.subscribe("claude-code", "slack.message", nil)
	f.slack("e1", "C1")
	f.slack("e2", "C1")

	crash := errors.New("worker killed")
	if _, err := (&Runner{DB: f.d, beforeCommit: func() error { return crash }}).Tenant(ctx, f.tenant); !errors.Is(err, crash) {
		t.Fatalf("crashing run: %v", err)
	}
	if got := f.owed("claude-code"); len(got) != 0 {
		t.Fatalf("a crashed step left %v", got)
	}
	(&Runner{DB: f.d}).Tenant(ctx, f.tenant)
	if got := fmt.Sprint(f.owed("claude-code")); got != "[e1 e2]" {
		t.Fatalf("after recovery owed %s", got)
	}
}

// Several runners (as on several instances) and concurrent writers: every
// matching event is delivered exactly once.
func TestConcurrentRunners(t *testing.T) {
	f := setup(t)
	f.subscribe("claude-code", "slack.message", nil)
	f.subscribe("codex", "slack.message", nil)

	runCtx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			(&Runner{DB: f.d, Batch: 5, Poll: 20 * time.Millisecond}).Run(runCtx)
		}()
	}
	const n = 60
	var writers sync.WaitGroup
	for w := range 3 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := range n / 3 {
				f.slack(fmt.Sprintf("w%d-%d", w, i), "C1")
			}
		}()
	}
	writers.Wait()
	deadline := time.Now().Add(10 * time.Second)
	for len(f.owed("claude-code")) < n || len(f.owed("codex")) < n {
		if time.Now().After(deadline) {
			t.Fatalf("delivered %d and %d of %d", len(f.owed("claude-code")), len(f.owed("codex")), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	wg.Wait()
	for _, agent := range []string{"claude-code", "codex"} {
		seen := map[string]bool{}
		for _, id := range f.owed(agent) {
			if seen[id] {
				t.Fatalf("%s got %s twice", agent, id)
			}
			seen[id] = true
		}
		if len(seen) != n {
			t.Fatalf("%s got %d of %d", agent, len(seen), n)
		}
	}
}

func TestCancellingCancelsPendingDeliveries(t *testing.T) {
	f := setup(t)
	s := f.subscribe("claude-code", "slack.message", nil)
	e := f.slack("e1", "C1")
	(&Runner{DB: f.d}).Tenant(ctx, f.tenant)
	f.tx(func(tx pgx.Tx) error {
		_, err := subscriptions.Cancel(ctx, tx, s.ID)
		return err
	})
	f.tx(func(tx pgx.Tx) error {
		d, err := deliveries.Get(ctx, tx, deliveries.ID("claude-code", e.ID, ""))
		if err == nil && d.State != "cancelled" {
			t.Errorf("state %s", d.State)
		}
		return err
	})
	f.slack("e2", "C1")
	if n, err := (&Runner{DB: f.d}).Tenant(ctx, f.tenant); err != nil || n != 0 {
		t.Fatalf("canceled subscription produced %d (%v)", n, err)
	}
}

func (f *fixture) cancel(id string) {
	f.t.Helper()
	f.tx(func(tx pgx.Tx) error {
		_, err := subscriptions.Cancel(ctx, tx, id)
		return err
	})
}

func (f *fixture) state(agent string, e events.Event) string {
	f.t.Helper()
	var state string
	f.tx(func(tx pgx.Tx) error {
		d, err := deliveries.Get(ctx, tx, deliveries.ID(agent, e.ID, ""))
		state = d.State
		return err
	})
	return state
}

// A cancel that lands while the runner is mid-step on the subscription
// still cancels what that step creates.
func TestCancelDuringAStep(t *testing.T) {
	f := setup(t)
	s := f.subscribe("claude-code", "slack.message", nil)
	e := f.slack("e1", "C1")
	cancelled := make(chan struct{})
	var once sync.Once
	r := &Runner{DB: f.d, beforeCommit: func() error {
		once.Do(func() {
			go func() { f.cancel(s.ID); close(cancelled) }()
			time.Sleep(200 * time.Millisecond) // the cancel is now waiting on this step
		})
		return nil
	}}
	if _, err := r.Tenant(ctx, f.tenant); err != nil {
		t.Fatal(err)
	}
	<-cancelled
	if st := f.state("claude-code", e); st != "cancelled" {
		t.Fatalf("delivery for a canceled subscription is %s", st)
	}
}

// Canceling one of two subscriptions that both match an event keeps the
// delivery the other still owes.
func TestCancelKeepsWhatAnotherSubscriptionOwes(t *testing.T) {
	f := setup(t)
	broad := f.subscribe("claude-code", "slack.*", nil)
	f.subscribe("claude-code", "slack.message", map[string]string{"channel": "C1"})
	e := f.slack("e1", "C1")
	only := f.slack("e2", "C2") // matches only the broad one
	(&Runner{DB: f.d}).Tenant(ctx, f.tenant)
	f.cancel(broad.ID)
	if st := f.state("claude-code", e); st != "pending" {
		t.Fatalf("delivery the other subscription owes is %s", st)
	}
	if st := f.state("claude-code", only); st != "cancelled" {
		t.Fatalf("delivery only the canceled subscription owed is %s", st)
	}
}

// An event takes one step (one transaction) to deliver: no second step just
// to find nothing left. A full batch, or a step's worth of subscriptions,
// takes another.
func TestOneStepPerWake(t *testing.T) {
	f := setup(t)
	f.subscribe("claude-code", "slack.message", nil)
	f.subscribe("codex", "slack.message", nil)
	steps := 0
	r := &Runner{DB: f.d, beforeCommit: func() error { steps++; return nil }}
	f.slack("e1", "C1")
	if n, err := r.Tenant(ctx, f.tenant); err != nil || n != 2 || steps != 1 {
		t.Fatalf("created %d, %d steps, %v", n, steps, err)
	}
	// A full batch (Batch 2) may have more behind it: another step.
	steps = 0
	r.Batch = 2
	f.slack("e2", "C1")
	f.slack("e3", "C1")
	f.slack("e4", "C1")
	if n, err := r.Tenant(ctx, f.tenant); err != nil || n != 6 || steps != 2 {
		t.Fatalf("created %d, %d steps, %v", n, steps, err)
	}
	if got := fmt.Sprint(f.owed("codex")); got != "[e1 e2 e3 e4]" {
		t.Fatalf("owed %s", got)
	}
}
