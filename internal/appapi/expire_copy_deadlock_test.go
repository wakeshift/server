package appapi_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/subscriptions"
)

// The expiry pass ending lapsed topic subscriptions (ExpireTopics, longest
// lapsed first) and a publish's copy ending the same ones as it finds them
// (copyInto, oldest created first) each tell a conversation per ended
// subscription, taking the workspace's event counter after the first: in
// opposite orders, each then waits on the other.
func TestExpireTopicsAndAPublishCopyEndingTheSameSubscriptions(t *testing.T) {
	d, tenant := quizzerUser(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var a, b string
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) VALUES (current_tenant(), 'claude-code')`); err != nil {
			return err
		}
		grant := dbtest.Grant(t, tx, "quizzer", "claude-code", 24*time.Hour)
		mk := func(label, created, expires string) (string, error) {
			s, err := sessions.Register(ctx, tx, sessions.New{Agent: "claude-code", Label: label, CreatedBy: "app:quizzer"})
			if err != nil {
				return "", err
			}
			sub, err := subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "claude-code", EventType: "topics.quizzer", CreatedBy: "app:quizzer",
				GrantID: grant, SessionID: s.ID})
			if err != nil {
				return "", err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO topic_subscriptions (tenant_id, subscription_id, app_id, topic, session_id, ttl_seconds, expires_at, created_at)
				VALUES (current_tenant(), $1, 'quizzer', 'rounds/1', $2, 3600, now() - $3::interval, now() - $4::interval)`, sub.ID, s.ID, expires, created); err != nil {
				return "", err
			}
			_, err = tx.Exec(ctx, `INSERT INTO topic_members (app_id, topic, tenant_id, subscription_id) VALUES ('quizzer', 'rounds/1', current_tenant(), $1)`, sub.ID)
			return sub.ID, err
		}
		var err error
		// a: created first, lapsed last; b: created last, lapsed first.
		if a, err = mk("a", "2 hours", "1 minute"); err != nil {
			return err
		}
		b, err = mk("b", "1 hour", "30 minutes")
		return err
	}))
	// A publish to the topic, recorded with both as subscribers, its copy
	// not made yet (as one interrupted, for FinishFanouts).
	if _, err := d.Pool.Exec(ctx, `INSERT INTO topic_events (app_id, source_id, event_id, topics, text) VALUES ('quizzer', 'p1', gen_random_uuid(), '{rounds/1}', 'hi')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO topic_fanout (app_id, source_id, tenant_id, subscriptions, created_at) VALUES ('quizzer', 'p1', $1, $2, now() - interval '5 minutes')`,
		tenant, []string{a, b}); err != nil {
		t.Fatal(err)
	}
	expired, copied := make(chan error, 1), make(chan error, 1)
	// The workspace's event counter, held while both get to it.
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO event_counters (tenant_id, last_seq) VALUES (current_tenant(), 0)
			ON CONFLICT (tenant_id) DO UPDATE SET last_seq = event_counters.last_seq`); err != nil {
			return err
		}
		go func() {
			expired <- d.InTenant(ctx, tenant, func(tx pgx.Tx) error { _, err := appapi.ExpireTopics(ctx, tx); return err })
		}()
		lockWaiters(t, d, 1) // ExpireTopics waits to tell b's conversation
		go func() {
			_, err := appapi.FinishFanouts(ctx, d)
			copied <- err
		}()
		lockWaiters(t, d, 2) // the copy waits too
		return nil
	}))
	if err := <-expired; err != nil {
		t.Errorf("expiring topic subscriptions: %v", err)
	}
	if err := <-copied; err != nil {
		t.Errorf("making the publish's copy: %v", err)
	}
	copiesMade(t, d)
}
