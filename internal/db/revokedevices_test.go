package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/db"
)

// 122 revokes every workspace's live devices (an earlier desktop
// app's tokens), recording each revocation in that workspace's audit log, and
// leaves ones already revoked alone.
func TestRevokeDevicesMigration(t *testing.T) {
	ctx := context.Background()
	d := emptyDB(t)
	if err := d.MigrateWith(ctx, migrationsUpTo(t, "121_session_wait_task.sql"), db.MigrateOptions{Replaced: db.Replaced}); err != nil {
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
	var user string
	if err := d.Pool.QueryRow(ctx, `INSERT INTO users (email, name) VALUES ('ada@example.com', 'Ada') RETURNING id::text`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	live := map[string]string{} // tenant → its live device
	var tenants []string
	for range 2 {
		var tenant string
		if err := d.Pool.QueryRow(ctx, `INSERT INTO tenants (name) VALUES ('test') RETURNING id`).Scan(&tenant); err != nil {
			t.Fatal(err)
		}
		tenants = append(tenants, tenant)
		inTenant(tenant, func(tx pgx.Tx) error {
			var id string
			if err := tx.QueryRow(ctx, `INSERT INTO devices (tenant_id, user_id, name, token_hash, epoch)
				VALUES (current_tenant(), $1, 'Mac', sha256(gen_random_uuid()::text::bytea), 0) RETURNING id::text`, user).Scan(&id); err != nil {
				return err
			}
			live[tenant] = id
			_, err := tx.Exec(ctx, `INSERT INTO devices (tenant_id, user_id, name, token_hash, epoch, revoked_at)
				VALUES (current_tenant(), $1, 'Old Mac', sha256(gen_random_uuid()::text::bytea), 0, now() - interval '1 day')`, user)
			return err
		})
	}
	if err := d.MigrateWith(ctx, db.Migrations, db.MigrateOptions{Replaced: db.Replaced}); err != nil {
		t.Fatal(err)
	}
	for _, tenant := range tenants {
		inTenant(tenant, func(tx pgx.Tx) error {
			var unrevoked, recent int
			if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE revoked_at IS NULL),
				count(*) FILTER (WHERE revoked_at > now() - interval '1 hour') FROM devices`).Scan(&unrevoked, &recent); err != nil {
				return err
			}
			if unrevoked != 0 || recent != 1 {
				t.Errorf("tenant %s: %d devices live, %d just revoked; want 0 and 1", tenant, unrevoked, recent)
			}
			rows, err := tx.Query(ctx, `SELECT actor, action, detail->>'device_id', detail->>'user_id', detail->>'reason' FROM audit_log`)
			if err != nil {
				return err
			}
			type entry struct{ Actor, Action, Device, User, Reason string }
			entries, err := pgx.CollectRows(rows, pgx.RowToStructByPos[entry])
			if err != nil {
				return err
			}
			want := entry{"hub", "device.revoked", live[tenant], user, "wakeshift_for_mac_removed"}
			if len(entries) != 1 || entries[0] != want {
				t.Errorf("tenant %s: audit log %v; want [%v]", tenant, entries, want)
			}
			return nil
		})
	}
}
