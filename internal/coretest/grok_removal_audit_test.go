package coretest_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/grants"
	"github.com/wakeshift/server/internal/streams"
)

// unregisteredFor is the workspace's agent_webhook.unregistered entries
// with reason, and their actor.
func unregisteredFor(t *testing.T, c *coretest.Core, tenant, reason string) (n int, actor string) {
	t.Helper()
	err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*), coalesce(max(actor), '') FROM audit_log WHERE action = 'agent_webhook.unregistered'
			AND detail->>'reason' = $1 AND detail->>'kind' = 'cursor-automations' AND detail ? 'session'`, reason).Scan(&n, &actor)
	})
	if err != nil {
		t.Fatal(err)
	}
	return n, actor
}

// Every path that removes a webhook binding records it in the audit log,
// in the removal's own transaction: unwatching, the grant's revocation,
// its expiry (the deliverer's pass), revoking the agent's links, and the
// app's deletion.
func TestGrokRemovalsAudited(t *testing.T) {
	setUp := func(t *testing.T, hookID string) (*coretest.Core, func() (string, string, string)) {
		c := startGrok(t, nil)
		f := newFakeCursor(t, c)
		app := noteApp(t, c, "ci")
		bot := &grokBot{c: c, vm: newGrokVM(t), id: grokBotID}
		sub, _, _ := grokSetUp(t, c, f, app, bot, "gina", "ci/grok", hookID)
		tenant, _ := topicSubRow(t, c, sub.ID)
		return c, func() (string, string, string) {
			var agent, grant string
			c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT agent_id, coalesce(grant_id::text, '') FROM agent_webhooks`).Scan(&agent, &grant)
			})
			return tenant, agent, grant
		}
	}
	gone := func(t *testing.T, c *coretest.Core, tenant, reason, actor string) {
		t.Helper()
		waitFor(t, "the destination to go", func() bool { st, _, _ := hookState(t, c, tenant); return st == "" })
		if n, got := unregisteredFor(t, c, tenant, reason); n != 1 || got != actor {
			t.Fatalf("%d entries for %s by %q, want 1 by %q", n, reason, got, actor)
		}
	}

	t.Run("unwatch", func(t *testing.T) {
		c := startGrok(t, nil)
		f := newFakeCursor(t, c)
		app := noteApp(t, c, "ci")
		bot := &grokBot{c: c, vm: newGrokVM(t), id: grokBotID}
		sub, call, _ := grokSetUp(t, c, f, app, bot, "gina", "ci/grok", "auditHookU")
		tenant, _ := topicSubRow(t, c, sub.ID)
		if _, err := unsubscribe(app, call, "ci/grok"); err != nil {
			t.Fatal(err)
		}
		gone(t, c, tenant, "unwatched", "agent")
	})

	t.Run("grant revoked", func(t *testing.T) {
		c, ids := setUp(t, "auditHookG")
		tenant, _, grant := ids()
		if grant == "" {
			t.Skip("no grant on this destination")
		}
		if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			_, err := grants.Revoke(ctx, tx, grant, "user:test")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		gone(t, c, tenant, "grant_ended", "user:test")
	})

	t.Run("grant expired", func(t *testing.T) {
		c, ids := setUp(t, "auditHookE")
		tenant, _, grant := ids()
		if grant == "" {
			t.Skip("no grant on this destination")
		}
		if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE grants SET expires_at = now() - interval '1 second' WHERE id = $1`, grant)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		c.Core.ExpireNow(ctx)
		time.Sleep(200 * time.Millisecond)
		waitFor(t, "the destination to go", func() bool { st, _, _ := hookState(t, c, tenant); return st == "" })
		if n, _ := unregisteredFor(t, c, tenant, "grant_ended"); n != 1 {
			t.Fatalf("%d entries for the expired grant, want 1", n)
		}
	})

	t.Run("agent's links revoked", func(t *testing.T) {
		c, ids := setUp(t, "auditHookL")
		tenant, agent, _ := ids()
		if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			_, err := streams.RevokeLinks(ctx, tx, agent)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		gone(t, c, tenant, "links_revoked", "user")
	})

	t.Run("app deleted", func(t *testing.T) {
		c, ids := setUp(t, "auditHookD")
		tenant, _, _ := ids()
		// A workspace the app doesn't own (a person's): deleting the app
		// deletes its rows there, and the workspace, with its audit log,
		// stays. (An app-owned workspace goes whole.)
		if _, err := c.DB.Pool.Exec(ctx, `UPDATE tenants SET kind = 'personal', app_id = NULL, external_id = NULL WHERE id = $1`, tenant); err != nil {
			t.Fatal(err)
		}
		if _, err := c.DB.Pool.Exec(ctx, `WITH o AS (INSERT INTO organizations (name) VALUES ('Acme') RETURNING id)
			UPDATE apps SET deleted_at = now(), org_id = (SELECT id FROM o) WHERE id = 'ci'`); err != nil {
			t.Fatal(err)
		}
		if _, err := appapi.FinishAppDeletions(ctx, c.DB, 100, 100); err != nil {
			t.Fatal(err)
		}
		var workspace int
		c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE id = $1`, tenant).Scan(&workspace)
		if workspace == 0 {
			t.Fatal("the workspace went with the app")
		}
		// Deleting the app revokes its grants first, which removes the
		// destinations made under them (recorded as the grant's end, by the
		// hub); one with no grant goes as app_deleted.
		gone(t, c, tenant, "grant_ended", audit.Hub)
	})
}
