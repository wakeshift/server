package mcpevents

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/sessions"
)

// A workspace whose only webhook has lapsed is still visited, and the
// webhook removed. Before, a pass visited only workspaces with a live
// webhook, so the last one to lapse stayed for good, and one that lapsed
// mid-retry was counted as failing by the admin service ever after.
func TestPassRemovesAWorkspacesLastExpiredWebhook(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	if _, err := d.Pool.Exec(ctx, `INSERT INTO apps (id, name) VALUES ('cards', 'Cards')`); err != nil {
		t.Fatal(err)
	}
	seal, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	// Two workspaces, each with one webhook; one lapsed mid-retry, as the
	// house agents' did after a deploy. The other is live.
	lapsed, live := dbtest.NewTenant(t, d), dbtest.NewTenant(t, d)
	for _, tenant := range []string{lapsed, live} {
		err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			if _, err := agents.Create(ctx, tx, "agent", ""); err != nil {
				return err
			}
			s, err := sessions.Register(ctx, tx, sessions.New{Agent: "agent", CreatedBy: "app:cards"})
			if err != nil {
				return err
			}
			// The app's grant, so the live webhook isn't ended for want of one.
			dbtest.Grant(t, tx, "cards", "agent", time.Hour)
			_, err = Subscribe(ctx, tx, seal, Webhook{ID: "sub_" + tenant[:8], App: "cards", Agent: "agent", Session: s.ID, SessionHandle: s.Handle,
				Name: "cards.turn", URL: "https://house.example/hooks/cards-9", Secret: []byte("secret"), TTL: time.Hour}, time.Now())
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	err = d.InTenant(ctx, lapsed, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE mcp_event_webhooks SET expires_at = now() - interval '1 minute', retrying_since = now() - interval '20 minutes',
			retry_attempts = 4, last_status = 401, next_attempt_at = now() - interval '5 minutes'`)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE mcp_event_targets SET expires_at = now() - interval '1 minute' WHERE tenant_id = current_tenant()`)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	dv := &Deliverer{DB: d, Seal: seal}
	if err := dv.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	count := func(tenant string) (hooks, targets int) {
		t.Helper()
		err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM mcp_event_webhooks`).Scan(&hooks); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM mcp_event_targets WHERE tenant_id = current_tenant()`).Scan(&targets)
		})
		if err != nil {
			t.Fatal(err)
		}
		return hooks, targets
	}
	if h, tg := count(lapsed); h != 0 || tg != 0 {
		t.Errorf("the lapsed workspace: %d webhooks, %d targets; want none", h, tg)
	}
	if h, tg := count(live); h != 1 || tg != 1 {
		t.Errorf("the live workspace: %d webhooks, %d targets; want its one", h, tg)
	}
}

// The webhooks already left behind (lapsed, their target removed) are
// given a target again by 105_expired_webhook_targets.sql, so the next
// pass removes them too.
func TestMigrationGivesLeftBehindWebhooksTargets(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	if _, err := d.Pool.Exec(ctx, `INSERT INTO apps (id, name) VALUES ('sketch', 'Sketch')`); err != nil {
		t.Fatal(err)
	}
	seal, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	tenant := dbtest.NewTenant(t, d)
	err = d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := agents.Create(ctx, tx, "agent", ""); err != nil {
			return err
		}
		s, err := sessions.Register(ctx, tx, sessions.New{Agent: "agent", CreatedBy: "app:sketch"})
		if err != nil {
			return err
		}
		if _, err := Subscribe(ctx, tx, seal, Webhook{ID: "sub_left", App: "sketch", Agent: "agent", Session: s.ID, SessionHandle: s.Handle,
			Name: "game.update", URL: "https://house.example/hooks/sketch-9", Secret: []byte("secret"), TTL: time.Hour}, time.Now()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE mcp_event_webhooks SET expires_at = now() - interval '3 days', retrying_since = now() - interval '3 days'`); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM mcp_event_targets WHERE tenant_id = current_tenant()`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../db/migrations/105_expired_webhook_targets.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	if err := (&Deliverer{DB: d, Seal: seal}).Pass(ctx); err != nil {
		t.Fatal(err)
	}
	var hooks, targets int
	err = d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM mcp_event_webhooks`).Scan(&hooks); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM mcp_event_targets`).Scan(&targets)
	})
	if err != nil || hooks != 0 || targets != 0 {
		t.Fatalf("after the migration and a pass: %d webhooks, %d targets (%v); want none", hooks, targets, err)
	}
}
