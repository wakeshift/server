package appapi_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/subscriptions"
)

// A multi-recipient emit's direct copy to a subscription, made while the
// subscription is revoked in the console: the copy appends its event (the
// workspace's event counter), then starts the subscription's ttl again
// (its topic_subscriptions row); the revoke ends that row, then appends its
// notice. Neither may wait on the other.
func TestDirectEmitCopyAndARevokeOfItsSubscription(t *testing.T) {
	d, tenant := quizzerUser(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var es string
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) VALUES (current_tenant(), 'claude-code')`); err != nil {
			return err
		}
		grant := dbtest.Grant(t, tx, "quizzer", "claude-code", 24*time.Hour)
		s, err := sessions.Register(ctx, tx, sessions.New{Agent: "claude-code", Label: "a", CreatedBy: "app:quizzer"})
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
		if _, err := tx.Exec(ctx, `INSERT INTO topic_members (app_id, topic, tenant_id, subscription_id) VALUES ('quizzer', 'rounds/1', current_tenant(), $1)`, sub.ID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO event_subscriptions (tenant_id, source_kind, source_id, event, arguments, arguments_key, destination, agent_id, session_id, subscription_id, created_by, principal)
			VALUES (current_tenant(), 'app', 'quizzer', 'ping', '{}', 'k', 'conversation', 'claude-code', $1, $2, 'app:quizzer', 'bob') RETURNING id::text`, s.ID, sub.ID).Scan(&es)
	}))
	// The emit, recorded, its direct copy to es not made yet.
	to := `[{"user": "bob", "subscription": "` + es + `"}]`
	if _, err := d.Pool.Exec(ctx, `INSERT INTO topic_events (app_id, source_id, event_id, topics, text, recipients, results)
		VALUES ('quizzer', 'e1', gen_random_uuid(), '{}', 'hi', $1::jsonb, '{"found": []}')`, `{"event": "ping", "to": `+to+`}`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO topic_fanout (app_id, source_id, tenant_id, subscriptions, direct, created_at)
		VALUES ('quizzer', 'e1', $1, '{}', $2::jsonb, now() - interval '2 minutes')`, tenant, to); err != nil {
		t.Fatal(err)
	}
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO event_counters (tenant_id, last_seq) VALUES (current_tenant(), 0) ON CONFLICT DO NOTHING`)
		return err
	}))
	copied, revoked := make(chan error, 1), make(chan error, 1)
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM event_counters WHERE tenant_id = current_tenant() FOR UPDATE`); err != nil {
			return err
		}
		go func() {
			_, err := appapi.FinishFanouts(ctx, d)
			copied <- err
		}()
		lockWaiters(t, d, 1) // the copy waits to append
		go func() {
			revoked <- d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
				_, err := appapi.RevokeEventSubscription(ctx, tx, es, audit.Hub)
				return err
			})
		}()
		lockWaiters(t, d, 2) // the revoke waits too: to end the routing, or to append its notice
		return nil
	}))
	if err := <-copied; err != nil {
		t.Errorf("making the emit's copy: %v", err)
	}
	copiesMade(t, d)
	if err := <-revoked; err != nil {
		t.Errorf("revoking: %v", err)
	}
}
