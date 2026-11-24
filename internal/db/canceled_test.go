package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/db"
)

// 116 moves every workspace's canceled subscriptions to 'canceled', and
// still takes 'cancelled' from the release before it while both serve.
func TestCanceledMigration(t *testing.T) {
	ctx := context.Background()
	d := emptyDB(t)
	if err := d.MigrateWith(ctx, migrationsUpTo(t, "115_drop_groups.sql"), db.MigrateOptions{Replaced: db.Replaced}); err != nil {
		t.Fatal(err)
	}
	inTenant := func(tenant string, f func(tx pgx.Tx) error) {
		t.Helper()
		if err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
				return err
			}
			return f(tx)
		}); err != nil {
			t.Fatal(err)
		}
	}
	var tenants []string
	for range 2 {
		var id string
		if err := d.Pool.QueryRow(ctx, `INSERT INTO tenants (name) VALUES ('test') RETURNING id`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		tenants = append(tenants, id)
		inTenant(id, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) VALUES (current_tenant(), 'codex');
				INSERT INTO subscriptions (tenant_id, agent_id, event_type, status, start_seq, created_by) VALUES
					(current_tenant(), 'codex', 'a.b', 'active', 0, 'x'),
					(current_tenant(), 'codex', 'a.b', 'cancelled', 0, 'x')`)
			return err
		})
	}
	if err := d.MigrateWith(ctx, db.Migrations, db.MigrateOptions{Replaced: db.Replaced}); err != nil {
		t.Fatal(err)
	}
	for _, tenant := range tenants {
		inTenant(tenant, func(tx pgx.Tx) error {
			var active, canceled, cancelled int
			if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status = 'active'), count(*) FILTER (WHERE status = 'canceled'),
				count(*) FILTER (WHERE status = 'cancelled') FROM subscriptions`).Scan(&active, &canceled, &cancelled); err != nil {
				return err
			}
			if active != 1 || canceled != 1 || cancelled != 0 {
				t.Errorf("tenant %s: %d active, %d canceled, %d cancelled", tenant, active, canceled, cancelled)
			}
			// What the release before writes, while it still serves.
			_, err := tx.Exec(ctx, `INSERT INTO subscriptions (tenant_id, agent_id, event_type, status, start_seq, created_by)
				VALUES (current_tenant(), 'codex', 'a.b', 'cancelled', 0, 'x')`)
			return err
		})
	}
	if err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenants[0]); err != nil {
			t.Fatal(err)
		}
		_, err := tx.Exec(ctx, `INSERT INTO subscriptions (tenant_id, agent_id, event_type, status, start_seq, created_by)
			VALUES (current_tenant(), 'codex', 'a.b', 'ended', 0, 'x')`)
		return err
	}); err == nil {
		t.Error("took a status that isn't one")
	}
}
