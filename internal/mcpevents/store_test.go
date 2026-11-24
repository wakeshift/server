package mcpevents

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/dbtest"
)

// A Subscribe that replaces a session's webhook while a delivery pass is
// ending that same webhook waits for the pass, rather than deadlocking
// with it: both delete the webhook before its target. The pass locks the
// webhook, then removes it (Unsubscribe); it's held here just before it
// removes the target, by a trigger that waits on an advisory lock the test
// holds, while the Subscribe runs.
func TestSubscribeWaitsForAPassEndingTheWebhook(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	tenant := dbtest.NewTenant(t, d)
	owner := dbtest.Owner(t, d)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := owner.Exec(ctx, `INSERT INTO apps (id, name, mcp_url) VALUES ('zzapp', 'zz', 'https://zz.example/mcp')`)
	must(err)
	seal, err := NewSealer(make([]byte, 32))
	must(err)
	var session string
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) VALUES (current_tenant(), 'ag')`); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO sessions (tenant_id, agent_id, handle, created_by) VALUES (current_tenant(), 'ag', 'ag#1', 'user:u') RETURNING id::text`).Scan(&session); err != nil {
			return err
		}
		// Ended (access revoked): the next pass removes it.
		if _, err := tx.Exec(ctx, `INSERT INTO mcp_event_webhooks (tenant_id, id, app_id, agent_id, session_id, session_handle, name, url, secret, expires_at, ending_reason)
			VALUES (current_tenant(), 'sub_w', 'zzapp', 'ag', $1, 'ag#1', 'e', 'https://cb.example/', $2, now() + interval '1 hour', 'Access revoked')`, session, seal.seal("sub_w", []byte("s"))); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO mcp_event_targets (tenant_id, webhook_id, expires_at) VALUES (current_tenant(), 'sub_w', now() + interval '1 hour')`)
		return err
	}))

	// A transaction's second statement deleting targets (the pass's
	// Unsubscribe; its first is the pass's sweep of expired webhooks)
	// waits, before it locks anything, until the test lets it go.
	_, err = owner.Exec(ctx, `CREATE FUNCTION zz_hold() RETURNS trigger LANGUAGE plpgsql AS $$
		DECLARE n int := coalesce(nullif(current_setting('zz.target_deletes', true), ''), '0')::int + 1;
		BEGIN
			PERFORM set_config('zz.target_deletes', n::text, true);
			IF n = 2 THEN PERFORM pg_advisory_xact_lock_shared(4242); END IF;
			RETURN NULL;
		END $$`)
	must(err)
	_, err = owner.Exec(ctx, `CREATE TRIGGER zz_hold BEFORE DELETE ON mcp_event_targets FOR EACH STATEMENT EXECUTE FUNCTION zz_hold()`)
	must(err)
	_, err = owner.Exec(ctx, `SELECT pg_advisory_lock(4242)`)
	must(err)
	released := false
	release := func() {
		if !released {
			released = true
			owner.Exec(ctx, `SELECT pg_advisory_unlock(4242)`)
		}
	}
	defer release()

	dl := &Deliverer{DB: d, Seal: seal}
	dl.init()
	claimErr := make(chan error, 1)
	go func() { _, _, err := dl.claim(ctx, tenant, ""); claimErr <- err }()
	for deadline := time.Now().Add(10 * time.Second); ; {
		var waiting int
		must(owner.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&waiting))
		if waiting > 0 {
			break // the pass holds the webhook and waits
		}
		if time.Now().After(deadline) {
			t.Fatal("the pass never reached Unsubscribe")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// A new webhook for the session replaces the old one.
	subErr := make(chan error, 1)
	go func() {
		subErr <- d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			_, err := Subscribe(ctx, tx, seal, Webhook{ID: "sub_new", App: "zzapp", Agent: "ag", Session: session, SessionHandle: "ag#1",
				Name: "e", URL: "https://cb.example/new", Secret: []byte("t"), TTL: time.Hour}, time.Now())
			return err
		})
	}()
	time.Sleep(300 * time.Millisecond) // the Subscribe waits on the webhook now
	release()
	e1, e2 := <-claimErr, <-subErr
	if e1 != nil {
		t.Fatalf("the pass: %v", e1)
	}
	if e2 != nil {
		t.Fatalf("the Subscribe: %v", e2)
	}
	var hooks, targets []string
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT id FROM mcp_event_webhooks ORDER BY id`)
		var err error
		if hooks, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		rows, _ = tx.Query(ctx, `SELECT webhook_id FROM mcp_event_targets WHERE tenant_id = current_tenant() ORDER BY webhook_id`)
		targets, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	}))
	if len(hooks) != 1 || hooks[0] != "sub_new" || len(targets) != 1 || targets[0] != "sub_new" {
		t.Fatalf("webhooks %v, targets %v: want only the new one", hooks, targets)
	}
}
