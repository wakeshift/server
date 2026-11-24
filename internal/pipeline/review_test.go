package pipeline

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wakeshift/server/internal/subscriptions"
)

// A step woken for an event, finding the subscription's cursor locked by
// another transaction (a sweep whose snapshot predates the event), tries
// again once it's released rather than leaving the event to the next poll.
func TestLockedCursorIsTriedAgain(t *testing.T) {
	f := setup(t)
	s := f.subscribe("claude-code", "slack.message", nil)
	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- f.d.InTenant(ctx, f.tenant, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM subscription_cursors WHERE subscription_id = $1 FOR UPDATE`, s.ID); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	f.slack("e1", "C1")
	time.AfterFunc(150*time.Millisecond, func() { close(release) })
	n, err := (&Runner{DB: f.d}).Tenant(ctx, f.tenant)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err != nil || n != 1 || fmt.Sprint(f.owed("claude-code")) != "[e1]" {
		t.Fatalf("created %d (%v), owed %v", n, err, f.owed("claude-code"))
	}
}

// grant makes an app grant for agent with the given wake limits, expiring
// in expiresIn, and a subscription under it.
func (f *fixture) grant(agent string, limits string, expiresIn time.Duration, filters map[string]string) (grant string, sub subscriptions.Subscription) {
	f.t.Helper()
	if _, err := f.d.Pool.Exec(ctx, `INSERT INTO apps (id, name) VALUES ('pacer', 'Pacer') ON CONFLICT DO NOTHING`); err != nil {
		f.t.Fatal(err)
	}
	f.tx(func(tx pgx.Tx) (err error) {
		if err := tx.QueryRow(ctx, `INSERT INTO grants (tenant_id, app_id, kind, agent_id, scope, confirmed_by, expires_at)
			VALUES (current_tenant(), 'pacer', 'agent', $1, $2, 'app', now() + $3::interval) RETURNING id::text`,
			agent, `{"agent": {"wake": [], "post": [], "ttl_days": 1, "wake_limits": `+limits+`}}`, expiresIn.String()).Scan(&grant); err != nil {
			return err
		}
		sub, err = subscriptions.Create(ctx, tx, subscriptions.New{AgentID: agent, EventType: "slack.message", Filters: filters, CreatedBy: "app:pacer", GrantID: grant})
		return err
	})
	return grant, sub
}

// Two steps scheduling one grant's wake-ups at once (on different
// subscriptions) can't both count only what their own snapshot shows: the
// second to record a wake-up on the grant gets a serialization conflict,
// and runs again. Whether the first has committed by then or is still
// running (the second waits for it).
func TestConcurrentStepsShareAGrantsLimits(t *testing.T) {
	f := setup(t)
	_, s1 := f.grant("claude-code", `[{"seconds": 3600, "max": 1}]`, time.Hour, map[string]string{"channel": "C1"})
	var s2 subscriptions.Subscription
	f.tx(func(tx pgx.Tx) (err error) {
		var g string
		tx.QueryRow(ctx, `SELECT grant_id::text FROM subscriptions WHERE id = $1`, s1.ID).Scan(&g)
		s2, err = subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "claude-code", EventType: "slack.message",
			Filters: map[string]string{"channel": "C2"}, CreatedBy: "app:pacer", GrantID: g})
		return err
	})
	// What a step does for one target: schedule, then record the wake-up.
	paced := func(tx pgx.Tx, sub string) error {
		p, err := pacing(ctx, tx, sub, map[string]*pace{})
		if err != nil {
			return err
		}
		at, _, err := p.schedule(ctx, tx)
		if err != nil {
			return err
		}
		return p.record(ctx, tx, at)
	}
	for _, firstCommits := range []bool{true, false} {
		snapshot, first := make(chan struct{}), make(chan struct{})
		second := make(chan error, 1)
		go func() {
			second <- f.d.InTenantTx(ctx, f.tenant, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
				close(snapshot) // the snapshot is taken (set_config ran)
				<-first
				return paced(tx, s2.ID)
			})
		}()
		<-snapshot
		if firstCommits {
			f.tx(func(tx pgx.Tx) error { return paced(tx, s1.ID) })
			close(first)
		} else {
			err := f.d.InTenantTx(ctx, f.tenant, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
				if err := paced(tx, s1.ID); err != nil {
					return err
				}
				close(first)
				time.Sleep(200 * time.Millisecond) // the second reaches the grant's row, and waits
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		var pgErr *pgconn.PgError
		if err := <-second; !errors.As(err, &pgErr) || pgErr.Code != "40001" {
			t.Fatalf("the second step on the grant (the first committed first: %v): %v", firstCommits, err)
		}
	}
	// End to end: one wake-up now, the other an hour later.
	f.slack("e1", "C1")
	f.slack("e2", "C2")
	if _, err := (&Runner{DB: f.d}).Tenant(ctx, f.tenant); err != nil {
		t.Fatal(err)
	}
	var now, later int
	f.tx(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE available_at <= now()), count(*) FILTER (WHERE available_at > now() + interval '59 minutes') FROM deliveries`).Scan(&now, &later)
	})
	if now != 1 || later > 1 {
		t.Fatalf("%d wake-ups now, %d an hour later", now, later)
	}
}

// Once a grant's end date passes (or it's revoked), its subscriptions wake
// no one, even before the expiry pass revokes it.
func TestExpiredGrantWakesNoOne(t *testing.T) {
	f := setup(t)
	g, _ := f.grant("claude-code", `[]`, time.Hour, nil)
	f.slack("e1", "C1")
	r := &Runner{DB: f.d}
	if n, err := r.Tenant(ctx, f.tenant); err != nil || n != 1 {
		t.Fatalf("live grant: created %d, %v", n, err)
	}
	f.tx(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE grants SET expires_at = now() - interval '1 second' WHERE id = $1`, g)
		return err
	})
	f.slack("e2", "C1")
	if n, err := r.Tenant(ctx, f.tenant); err != nil || n != 0 {
		t.Fatalf("expired grant: created %d, %v", n, err)
	}
	if got := fmt.Sprint(f.owed("claude-code")); got != "[e1]" {
		t.Fatalf("owed %s", got)
	}
}

// A request holding a grant (appapi's resolve takes it FOR SHARE) that
// cancels one of its subscriptions (touching the cursor) while a step holds
// that cursor and paces the grant isn't a deadlock: the step doesn't lock
// the grant, so it commits, and the cancel, which waited for its cursor,
// then cancels what the step made (subscriptions.Cancel).
func TestARequestHoldingTheGrantCancelsDuringAStep(t *testing.T) {
	f := setup(t)
	g, s := f.grant("claude-code", `[]`, time.Hour, nil)
	f.slack("e1", "C1")
	holding, stepping, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- f.d.InTenant(ctx, f.tenant, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM grants WHERE id = $1 FOR SHARE`, g); err != nil {
				return err
			}
			close(holding)
			<-stepping
			time.Sleep(200 * time.Millisecond) // the step reaches the grant's pacing
			_, err := subscriptions.Cancel(ctx, tx, s.ID)
			return err
		})
	}()
	<-holding
	stepped := make(chan error, 1)
	go func() {
		_, err := (&Runner{DB: f.d}).Tenant(ctx, f.tenant)
		stepped <- err
	}()
	close(stepping)
	if err := <-done; err != nil {
		t.Fatalf("the request: %v", err)
	}
	if err := <-stepped; err != nil {
		t.Fatalf("the step: %v", err)
	}
	var open int
	f.tx(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE state IN ('pending', 'in_flight')`).Scan(&open)
	})
	if open != 0 {
		t.Fatalf("%d deliveries still open for a canceled subscription (owed %v)", open, f.owed("claude-code"))
	}
}
