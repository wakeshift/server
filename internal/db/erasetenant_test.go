package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
)

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// auditRows counts tenant's audit log rows, as d's role.
func auditRows(t *testing.T, d *db.DB, tenant string) int {
	t.Helper()
	var n int
	err := d.InTenant(context.Background(), tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM audit_log`).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// With an owner, the server's role can't delete audit log rows, even by
// claiming an erasure (app.erasing_tenant, 030_forget_users.sql): only
// erase_tenant (170_erase_tenant.sql) deletes them, and only together with
// the workspace once nothing else of it is left. The admin service's read
// role can't call it.
func TestAuditLogDeletedOnlyByEraseTenant(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	tenant := newTenant(t, d, "a")
	other := newTenant(t, d, "b")
	for _, id := range []string{tenant, other} {
		err := d.InTenant(ctx, id, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) VALUES ($1, 'claude-code')`, id); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO audit_log (tenant_id, actor, action) VALUES ($1, 'user:u', 'grant.created')`, id)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// A SECURITY DEFINER function with a search path no caller can write
	// to.
	var definer bool
	var config []string
	if err := d.Pool.QueryRow(ctx, `SELECT prosecdef, proconfig FROM pg_proc WHERE oid = 'erase_tenant'::regproc`).Scan(&definer, &config); err != nil {
		t.Fatal(err)
	}
	if !definer || len(config) != 1 || config[0] != "search_path=pg_catalog, pg_temp" {
		t.Fatalf("erase_tenant: SECURITY DEFINER %v, config %q", definer, config)
	}

	// Deleting directly, as an erasure says it may.
	err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.erasing_tenant', current_tenant()::text, true)`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM audit_log`)
		return err
	})
	if pgCode(err) != "42501" {
		t.Fatalf("deleting the audit log as the server's role during an erasure: %v, want permission denied", err)
	}
	if n := auditRows(t, d, tenant); n != 1 {
		t.Fatalf("audit rows after the refused delete: %d, want 1", n)
	}

	// erase_tenant on a live workspace: refused, and nothing deleted.
	err = d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT erase_tenant()`)
		return err
	})
	if pgCode(err) != "23503" {
		t.Fatalf("erase_tenant with the workspace's agents left: %v, want a foreign key violation", err)
	}
	if n := auditRows(t, d, tenant); n != 1 {
		t.Fatalf("audit rows after the refused erase: %d, want 1", n)
	}

	// Without a workspace: refused.
	if _, err := d.Pool.Exec(ctx, `SELECT erase_tenant()`); err == nil {
		t.Fatal("erase_tenant with no workspace set: no error")
	}

	// The admin service's read role may not call it.
	admin, err := pgx.Connect(ctx, dbtest.AdminReadURL(t, d))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, `SELECT set_config('app.tenant_id', $1, false)`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `SELECT erase_tenant()`); pgCode(err) != "42501" {
		t.Fatalf("erase_tenant as the admin read role: %v, want permission denied", err)
	}

	// Once nothing else is left: the audit log and the workspace go
	// together, and no other workspace's audit log.
	err = d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM agents`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT erase_tenant()`)
		return err
	})
	if err != nil {
		t.Fatalf("erase_tenant on an emptied workspace: %v", err)
	}
	if n := auditRows(t, d, tenant); n != 0 {
		t.Fatalf("audit rows after the erase: %d, want 0", n)
	}
	var left bool
	if err := d.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1)`, tenant).Scan(&left); err != nil || left {
		t.Fatalf("the erased workspace's row: left %v (%v)", left, err)
	}
	if n := auditRows(t, d, other); n != 1 {
		t.Fatalf("the other workspace's audit rows: %d, want 1", n)
	}
	// Still append-only otherwise, for the owner too.
	owner := dbtest.Owner(t, d)
	if _, err := owner.Exec(ctx, `SELECT set_config('app.tenant_id', $1, false)`, other); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `DELETE FROM audit_log`); err == nil {
		t.Fatal("the owner deleted audit rows outside an erasure")
	}
}

// Without an owner (one role, which owns the tables), erase_tenant works
// the same.
func TestEraseTenantAsTheOwner(t *testing.T) {
	ctx := context.Background()
	d := dbtest.NewOwner(t)
	tenant := newTenant(t, d, "a")
	err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO audit_log (tenant_id, actor, action) VALUES ($1, 'user:u', 'grant.created')`, tenant); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT erase_tenant()`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := auditRows(t, d, tenant); n != 0 {
		t.Fatalf("audit rows after the erase: %d, want 0", n)
	}
}
