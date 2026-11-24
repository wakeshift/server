package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
)

var ctx = context.Background()

func list(t *testing.T, d *db.DB, tenant string, before int64, limit int) []audit.Item {
	t.Helper()
	var out []audit.Item
	if err := d.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		out, err = audit.List(ctx, tx, before, limit)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func record(d *db.DB, tenant string, e audit.Entry) error {
	return d.InTenant(ctx, tenant, func(tx pgx.Tx) error { return audit.Record(ctx, tx, e) })
}

// An entry commits with its action, or not at all.
func TestEntriesRollBackWithTheirTransaction(t *testing.T) {
	d := dbtest.New(t)
	tenant := dbtest.NewTenant(t, d)
	failed := errors.New("the action failed")
	err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if err := audit.Record(ctx, tx, audit.Entry{Actor: audit.User("u"), Action: audit.GrantRevoked}); err != nil {
			return err
		}
		return failed
	})
	if !errors.Is(err, failed) {
		t.Fatal(err)
	}
	if got := list(t, d, tenant, 0, 10); len(got) != 0 {
		t.Fatalf("a rolled-back action left %+v", got)
	}
	if err := record(d, tenant, audit.Entry{Actor: audit.User("u"), Action: audit.GrantRevoked, AgentID: "codex",
		Detail: map[string]int{"subscriptions_canceled": 2}}); err != nil {
		t.Fatal(err)
	}
	got := list(t, d, tenant, 0, 10)
	if len(got) != 1 || got[0].Actor != "user:u" || got[0].AgentID != "codex" || string(got[0].Detail) != `{"subscriptions_canceled": 2}` {
		t.Fatalf("entries %+v", got)
	}
}

// Entries written before the American spellings read as they're written
// now: the log can't be changed, so List maps them.
func TestBritishEntriesReadAmerican(t *testing.T) {
	d := dbtest.New(t)
	tenant := dbtest.NewTenant(t, d)
	for _, e := range []audit.Entry{
		{Actor: audit.User("u"), Action: "subscription.cancelled", Detail: map[string]any{"subscription_id": "s1", "event_type": "ci.failed"}},
		{Actor: audit.Hub, Action: audit.GrantExpired, Detail: map[string]any{"kind": "agent", "subscriptions_cancelled": 3}},
		{Actor: audit.User("u"), Action: audit.SubscriptionCanceled, Detail: map[string]any{"subscription_id": "s2"}},
	} {
		if err := record(d, tenant, e); err != nil {
			t.Fatal(err)
		}
	}
	got := list(t, d, tenant, 0, 10)
	if len(got) != 3 {
		t.Fatalf("entries %+v", got)
	}
	if got[0].Action != audit.SubscriptionCanceled || string(got[0].Detail) != `{"subscription_id": "s2"}` {
		t.Errorf("new entry %s %s", got[0].Action, got[0].Detail)
	}
	if got[1].Action != audit.GrantExpired || string(got[1].Detail) != `{"kind": "agent", "subscriptions_canceled": 3}` {
		t.Errorf("old revocation %s %s", got[1].Action, got[1].Detail)
	}
	if got[2].Action != audit.SubscriptionCanceled || string(got[2].Detail) != `{"event_type": "ci.failed", "subscription_id": "s1"}` {
		t.Errorf("old cancellation %s %s", got[2].Action, got[2].Detail)
	}
}

func TestEntriesAreTenantScoped(t *testing.T) {
	d := dbtest.New(t)
	a, b := dbtest.NewTenant(t, d), dbtest.NewTenant(t, d)
	if err := record(d, a, audit.Entry{Actor: audit.Hub, Action: audit.GrantExpired}); err != nil {
		t.Fatal(err)
	}
	got := list(t, d, b, 0, 10)
	if got == nil || len(got) != 0 {
		t.Fatalf("b sees %#v", got) // and [] rather than null for the console
	}
	if b, _ := json.Marshal(got); string(b) != "[]" {
		t.Fatalf("empty list marshals as %s", b)
	}
	if len(list(t, d, a, 0, 10)) != 1 {
		t.Fatal("a's entry is missing")
	}
	// Outside a workspace's transaction nothing can be written.
	if _, err := d.Pool.Exec(ctx, `INSERT INTO audit_log (tenant_id, actor, action) VALUES ($1, 'hub', 'x')`, a); err == nil {
		t.Fatal("wrote an entry outside a tenant transaction")
	}
}

func TestEntriesCantBeChanged(t *testing.T) {
	d := dbtest.New(t)
	tenant := dbtest.NewTenant(t, d)
	record(d, tenant, audit.Entry{Actor: audit.User("u"), Action: audit.GrantCreated})
	for _, stmt := range []string{`UPDATE audit_log SET actor = 'hub'`, `DELETE FROM audit_log`, `TRUNCATE audit_log`} {
		err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, stmt); return err })
		if err == nil {
			t.Errorf("%s succeeded", stmt)
		}
	}
	if got := list(t, d, tenant, 0, 10); len(got) != 1 || got[0].Actor != "user:u" {
		t.Fatalf("entries %+v", got)
	}
}

func TestPages(t *testing.T) {
	d := dbtest.New(t)
	tenant := dbtest.NewTenant(t, d)
	for _, action := range []string{"a", "b", "c", "d", "e"} {
		record(d, tenant, audit.Entry{Actor: audit.Hub, Action: action})
	}
	first := list(t, d, tenant, 0, 2)
	if len(first) != 2 || first[0].Action != "e" || first[1].Action != "d" {
		t.Fatalf("first page %+v", first)
	}
	rest := list(t, d, tenant, first[1].ID, 10)
	if len(rest) != 3 || rest[0].Action != "c" || rest[2].Action != "a" {
		t.Fatalf("next page %+v", rest)
	}
	if got := list(t, d, tenant, 0, 1000); len(got) != 5 {
		t.Fatalf("limit over the maximum: %d", len(got))
	}
}
