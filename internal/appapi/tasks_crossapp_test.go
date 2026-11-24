package appapi

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/subscriptions"
)

// Two apps' wait tasks in one session: a call of app B's wait tool confirms
// only what B's previous task handed out, never app A's batch, which A's
// agent may not have read yet. A's next call confirms A's, even though B's
// task has replaced A's as the session's newest since.
func TestWaitTaskConfirmsOnlyItsOwnAppsBatch(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO apps (id, name) VALUES ('alpha', 'Alpha'), ('beta', 'Beta')`); err != nil {
		t.Fatal(err)
	}
	tenant := dbtest.NewTenant(t, d)
	const holderA, holderB, holderA2 = "poll_wait_task_a", "poll_wait_task_b", "poll_wait_task_a2"
	var session string
	held := map[string]string{} // app → its delivery held by its task
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) VALUES (current_tenant(), 'claude')`); err != nil {
			return err
		}
		s, err := sessions.Register(ctx, tx, sessions.New{Agent: "claude", Conversation: "thread-1", CreatedBy: "app:alpha"})
		if err != nil {
			return err
		}
		session = s.ID
		if _, err := tx.Exec(ctx, `UPDATE sessions SET app_ids = array_append(app_ids, 'beta') WHERE id = $1`, session); err != nil {
			return err
		}
		for app, holder := range map[string]string{"alpha": holderA, "beta": holderB} {
			grant := dbtest.Grant(t, tx, app, "claude", 24*time.Hour)
			sub, err := subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "claude", EventType: topicsType(app), CreatedBy: "app:" + app, GrantID: grant, SessionID: session})
			if err != nil {
				return err
			}
			e, _, err := events.Append(ctx, tx, events.Event{Type: topicsType(app), Source: "app:" + app, From: events.Party{Kind: "app", ID: app},
				Subject: "for " + app, Trust: events.External, Attrs: map[string]string{"app": app}})
			if err != nil {
				return err
			}
			if _, err := deliveries.Insert(ctx, tx, deliveries.New{AgentID: "claude", Target: session, SubscriptionID: sub.ID, Event: e}); err != nil {
				return err
			}
			id := deliveries.ID("claude", e.ID, session)
			held[app] = id
			// Handed out by the app's task (a claim, as TaskWait makes).
			if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'in_flight', lease_holder = $2, lease_until = now() + interval '1 hour', attempts = 1 WHERE id = $1`, id, holder); err != nil {
				return err
			}
		}
		// A's task first, then B's replaced it as the session's newest.
		_, err = tx.Exec(ctx, `UPDATE sessions SET wait_task = $2, wait_tasks = jsonb_build_object('alpha', $3::text), wait_task_seen_at = now() WHERE id = $1`, session, holderA, holderA)
		return err
	}))
	state := func(app string) string {
		t.Helper()
		var st string
		must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT state FROM deliveries WHERE id = $1`, held[app]).Scan(&st)
		}))
		return st
	}
	// B's agent calls B's wait tool: A's task was the session's newest.
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		prev, err := confirmPrevious(ctx, tx, "beta", "claude", session)
		if err != nil {
			return err
		}
		if prev == nil || *prev != holderA {
			t.Errorf("replaces %v, want A's task", prev)
		}
		_, err = tx.Exec(ctx, `UPDATE sessions SET wait_task = $2, wait_tasks = wait_tasks || jsonb_build_object('beta', $2::text) WHERE id = $1`, session, holderB)
		return err
	}))
	if st := state("alpha"); st != "in_flight" {
		t.Fatalf("B's call confirmed A's batch: %s", st)
	}
	// A's agent read A's result and calls A's wait tool again: A's batch
	// is confirmed, and B's still waits for B.
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := confirmPrevious(ctx, tx, "alpha", "claude", session); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE sessions SET wait_task = $2, wait_tasks = wait_tasks || jsonb_build_object('alpha', $2::text) WHERE id = $1`, session, holderA2)
		return err
	}))
	if a, b := state("alpha"), state("beta"); a != "acked" || b != "in_flight" {
		t.Fatalf("after A's call: A's %s (want acked), B's %s (want in_flight)", a, b)
	}
}
