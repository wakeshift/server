package mcpevents

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/sessions"
)

// A finished webhook (its once subscription made its one delivery) no
// longer counts toward MaxWebhooks nor exists for a subscribe, which
// replaces it when it has its ID; a pass removes it only once it's been
// finished a minute and nothing waits for it, and leaves the others.
func TestFinishedWebhookStopsCounting(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	if _, err := d.Pool.Exec(ctx, `INSERT INTO apps (id, name) VALUES ('jobs', 'Jobs')`); err != nil {
		t.Fatal(err)
	}
	seal, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	tenant := dbtest.NewTenant(t, d)
	subscribe := func(tx pgx.Tx, id string) error {
		s, err := sessions.Register(ctx, tx, sessions.New{Agent: "agent", CreatedBy: "app:jobs"})
		if err != nil {
			return err
		}
		_, err = Subscribe(ctx, tx, seal, Webhook{ID: id, App: "jobs", Agent: "agent", Session: s.ID, SessionHandle: s.Handle,
			Name: "job.done", URL: "https://client.example/cb", Secret: []byte("secret"), TTL: time.Hour}, time.Now())
		return err
	}
	in := func(f func(tx pgx.Tx) error) {
		t.Helper()
		if err := d.InTenant(ctx, tenant, f); err != nil {
			t.Fatal(err)
		}
	}
	in(func(tx pgx.Tx) error {
		if _, err := agents.Create(ctx, tx, "agent", ""); err != nil {
			return err
		}
		dbtest.Grant(t, tx, "jobs", "agent", time.Hour)
		for i := range MaxWebhooks {
			if err := subscribe(tx, fmt.Sprintf("sub_%d", i)); err != nil {
				return err
			}
		}
		return nil
	})
	if err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error { return subscribe(tx, "sub_more") }); !errors.Is(err, ErrTooManyWebhooks) {
		t.Fatalf("one past the limit: %v", err)
	}
	in(func(tx pgx.Tx) error {
		if err := Finish(ctx, tx, "sub_0"); err != nil {
			return err
		}
		if ok, err := Exists(ctx, tx, "sub_0"); err != nil || ok {
			t.Errorf("a finished webhook exists: %v %v", ok, err)
		}
		return subscribe(tx, "sub_more") // room for one
	})
	// Its ID subscribed again (a new subscription, in a session of its
	// own): replaced, and counted, so the next is refused.
	in(func(tx pgx.Tx) error {
		if err := Finish(ctx, tx, "sub_1"); err != nil {
			return err
		}
		if err := subscribe(tx, "sub_1"); err != nil {
			return err
		}
		var finished *time.Time
		if err := tx.QueryRow(ctx, `SELECT finished_at FROM mcp_event_webhooks WHERE id = 'sub_1'`).Scan(&finished); err != nil || finished != nil {
			t.Errorf("sub_1 subscribed again: finished %v, %v", finished, err)
		}
		return nil
	})
	if err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error { return subscribe(tx, "sub_last") }); !errors.Is(err, ErrTooManyWebhooks) {
		t.Fatalf("at the limit again: %v", err)
	}

	count := func() (hooks, targets int) {
		t.Helper()
		in(func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM mcp_event_webhooks`).Scan(&hooks); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM mcp_event_targets`).Scan(&targets)
		})
		return hooks, targets
	}
	dv := &Deliverer{DB: d, Seal: seal}
	if err := dv.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if h, tg := count(); h != MaxWebhooks+1 || tg != MaxWebhooks+1 {
		t.Fatalf("just finished: %d webhooks, %d targets; want sub_0 kept", h, tg)
	}
	in(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE mcp_event_webhooks SET finished_at = finished_at - interval '2 minutes' WHERE finished_at IS NOT NULL`)
		return err
	})
	if err := dv.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if h, tg := count(); h != MaxWebhooks || tg != MaxWebhooks {
		t.Fatalf("a minute on: %d webhooks, %d targets; want only sub_0 gone", h, tg)
	}

	// One with a delivery still waiting (scheduled for later, as pacing
	// does) stays, however long ago it finished.
	f := newFixture(t, d, "later")
	other := dbtest.NewTenant(t, d)
	id, err := f.hook(other, "later", "https://client.example/later", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.InTenant(ctx, other, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET available_at = now() + interval '1 hour'`); err != nil {
			return err
		}
		if err := Finish(ctx, tx, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE mcp_event_webhooks SET finished_at = now() - interval '1 hour'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := dv.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	var kept bool
	if err := d.InTenant(ctx, other, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM mcp_event_webhooks WHERE id = $1)`, id).Scan(&kept)
	}); err != nil || !kept {
		t.Fatalf("a finished webhook with a delivery waiting was removed (%v)", err)
	}
}
