package appapi_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/subscriptions"
)

// A publish's copy into a workspace where the topic has a lapsed
// subscriber (a) before a live one (b), while b is ended (the console's
// revoke, event.end): the copy ended a, appending its notice (the
// workspace's event counter), then started b's ttl again (b's
// topic_subscriptions row); the end held b's row and waited on the counter
// for its own notice. Every live subscriber's ttl starts again before the
// copy appends anything, as #528 did for direct recipients.
func TestTopicCopyEndingOneSubscriberWhileAnotherIsRevoked(t *testing.T) {
	d, tenant := quizzerUser(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var a, b, es string
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) VALUES (current_tenant(), 'claude-code')`); err != nil {
			return err
		}
		grant := dbtest.Grant(t, tx, "quizzer", "claude-code", 24*time.Hour)
		mk := func(label, created, expires string) (sub, session string, err error) {
			s, err := sessions.Register(ctx, tx, sessions.New{Agent: "claude-code", Label: label, CreatedBy: "app:quizzer"})
			if err != nil {
				return "", "", err
			}
			ns, err := subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "claude-code", EventType: "topics.quizzer", CreatedBy: "app:quizzer",
				GrantID: grant, SessionID: s.ID})
			if err != nil {
				return "", "", err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO topic_subscriptions (tenant_id, subscription_id, app_id, topic, session_id, ttl_seconds, expires_at, created_at)
				VALUES (current_tenant(), $1, 'quizzer', 'rounds/1', $2, 3600, now() + $4::interval, now() - $3::interval)`, ns.ID, s.ID, created, expires); err != nil {
				return "", "", err
			}
			_, err = tx.Exec(ctx, `INSERT INTO topic_members (app_id, topic, tenant_id, subscription_id) VALUES ('quizzer', 'rounds/1', current_tenant(), $1)`, ns.ID)
			return ns.ID, s.ID, err
		}
		var err error
		if a, _, err = mk("a", "2 hours", "-1 minute"); err != nil { // lapsed
			return err
		}
		var session string
		if b, session, err = mk("b", "1 hour", "1 hour"); err != nil { // live
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO event_subscriptions (tenant_id, source_kind, source_id, event, arguments, arguments_key, destination, agent_id, session_id, subscription_id, created_by, principal)
			VALUES (current_tenant(), 'app', 'quizzer', 'ping', '{}', 'k', 'conversation', 'claude-code', $1, $2, 'app:quizzer', 'bob') RETURNING id::text`, session, b).Scan(&es)
	}))
	// The publish, recorded, its copy here not made yet.
	if _, err := d.Pool.Exec(ctx, `INSERT INTO topic_events (app_id, source_id, event_id, topics, text) VALUES ('quizzer', 'p1', gen_random_uuid(), '{rounds/1}', 'hi')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO topic_fanout (app_id, source_id, tenant_id, subscriptions, created_at)
		VALUES ('quizzer', 'p1', $1, $2, now() - interval '2 minutes')`, tenant, []string{a, b}); err != nil {
		t.Fatal(err)
	}
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO event_counters (tenant_id, last_seq) VALUES (current_tenant(), 0) ON CONFLICT DO NOTHING`)
		return err
	}))
	// waiting returns once n transactions wait on a lock.
	waiting := func(n int) error {
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			var got int
			if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&got); err != nil {
				return err
			}
			if got >= n {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%d transactions wait on a lock, not %d", got, n)
			}
		}
	}
	copied, revoked := make(chan error, 1), make(chan error, 1)
	// The workspace's event counter, held while both get to it.
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM event_counters WHERE tenant_id = current_tenant() FOR UPDATE`); err != nil {
			return err
		}
		go func() {
			_, err := appapi.FinishFanouts(ctx, d)
			copied <- err
		}()
		if err := waiting(1); err != nil { // the copy, at the counter (before the fix: having ended a)
			return err
		}
		go func() {
			revoked <- d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
				_, err := appapi.RevokeEventSubscription(ctx, tx, es, audit.Hub)
				return err
			})
		}()
		return waiting(2) // the revoke too: having ended b's routing, at the counter for its notice
	}))
	if err := <-copied; err != nil {
		t.Errorf("making the publish's copy: %v", err)
	}
	if err := <-revoked; err != nil {
		t.Errorf("revoking: %v", err)
	}
}
