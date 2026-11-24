package appapi_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/grants"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/subscriptions"
)

// Revoking a grant while one of its event subscriptions is being ended (a
// user's console revoke of the subscription, or the app's user ending it)
// waits for the end, rather than deadlocking with it: ending one locks it,
// then its subscription; revoking locks the grant, then the agent's event
// subscriptions, before canceling the grant's subscriptions.
func TestGrantRevokeWaitsForAnEventSubscriptionEnding(t *testing.T) {
	d, tenant := quizzerUser(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var grant, es string
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) VALUES (current_tenant(), 'claude-code')`); err != nil {
			return err
		}
		grant = dbtest.Grant(t, tx, "quizzer", "claude-code", 24*time.Hour)
		s, err := sessions.Register(ctx, tx, sessions.New{Agent: "claude-code", Label: "abc", CreatedBy: "app:quizzer"})
		if err != nil {
			return err
		}
		sub, err := subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "claude-code", EventType: "topics.quizzer", CreatedBy: "app:quizzer",
			GrantID: grant, SessionID: s.ID})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO topic_subscriptions (tenant_id, subscription_id, app_id, topic, session_id, ttl_seconds, expires_at)
			VALUES (current_tenant(), $1, 'quizzer', 'rounds/1', $2, 3600, now() + interval '1 hour')`, sub.ID, s.ID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO event_subscriptions (tenant_id, source_kind, source_id, event, arguments, arguments_key, destination, agent_id, session_id, subscription_id, created_by)
			VALUES (current_tenant(), 'app', 'quizzer', 'ping', '{}', 'k', 'conversation', 'claude-code', $1, $2, 'app:quizzer') RETURNING id::text`, s.ID, sub.ID).Scan(&es)
	}))
	revoked := make(chan error, 1)
	endErr := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		// RevokeEventSubscription's lock on the record, held open here.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM event_subscriptions WHERE id = $1 FOR UPDATE`, es); err != nil {
			return err
		}
		go func() {
			revoked <- d.InTenant(ctx, tenant, func(tx pgx.Tx) error { _, err := grants.Revoke(ctx, tx, grant, audit.Hub); return err })
		}()
		if err := waitForALockWait(ctx, d, tx); err != nil { // the revoke is waiting now
			return err
		}
		_, err := appapi.RevokeEventSubscription(ctx, tx, es, audit.Hub)
		return err
	})
	rerr := <-revoked
	if endErr != nil {
		t.Fatalf("ending the event subscription: %v", endErr)
	}
	if rerr != nil {
		t.Fatalf("revoking the grant: %v", rerr)
	}
	var revokedAt *time.Time
	var reason string
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT revoked_at FROM grants WHERE id = $1`, grant).Scan(&revokedAt); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT ended_reason FROM event_subscriptions WHERE id = $1`, es).Scan(&reason)
	}))
	if revokedAt == nil || reason != "revoked" {
		t.Fatalf("grant revoked at %v, event subscription ended %q", revokedAt, reason)
	}
}
