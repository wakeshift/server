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

// A multi-recipient emit's copy ends the lapsed topic subscriptions it
// finds, then the lapsed direct ones; the expiry pass ends its batch oldest
// first. With a direct recipient's subscription (d) older than the topic's
// (t), each would end one, take the workspace's event counter for its
// notice, and wait on the other's subscription. Every transaction that ends
// several subscriptions locks them all first, in ID order, so neither waits
// holding the counter.
func TestExpireTopicsAndAnEmitCopyEndingTopicAndDirectSubscriptions(t *testing.T) {
	d, tenant := quizzerUser(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var direct, topic, es string
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) VALUES (current_tenant(), 'claude-code')`); err != nil {
			return err
		}
		grant := dbtest.Grant(t, tx, "quizzer", "claude-code", 24*time.Hour)
		mk := func(label, topicName, created, expires string) (sub, session string, err error) {
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
				VALUES (current_tenant(), $1, 'quizzer', $3, $2, 3600, now() - $4::interval, now() - $5::interval)`, ns.ID, s.ID, topicName, expires, created); err != nil {
				return "", "", err
			}
			_, err = tx.Exec(ctx, `INSERT INTO topic_members (app_id, topic, tenant_id, subscription_id) VALUES ('quizzer', $2, current_tenant(), $1)`, ns.ID, topicName)
			return ns.ID, s.ID, err
		}
		// The direct recipient's: created first, lapsed last. The topic's:
		// created last, lapsed first.
		var session string
		var err error
		if direct, session, err = mk("d", "ping/bob", "2 hours", "1 minute"); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO event_subscriptions (tenant_id, source_kind, source_id, event, arguments, arguments_key, destination, agent_id, session_id, subscription_id, created_by, principal)
			VALUES (current_tenant(), 'app', 'quizzer', 'ping', '{}', 'k', 'conversation', 'claude-code', $1, $2, 'app:quizzer', 'bob') RETURNING id::text`, session, direct).Scan(&es); err != nil {
			return err
		}
		topic, _, err = mk("t", "rounds/1", "1 hour", "30 minutes")
		return err
	}))
	// The emit, recorded with t as the topic's subscriber and es as a
	// direct recipient, its copy not made yet (as one interrupted, for
	// FinishFanouts).
	to := `[{"user": "bob", "subscription": "` + es + `"}]`
	if _, err := d.Pool.Exec(ctx, `INSERT INTO topic_events (app_id, source_id, event_id, topics, text, recipients, results)
		VALUES ('quizzer', 'e1', gen_random_uuid(), '{rounds/1}', 'hi', $1::jsonb, '{"found": []}')`, `{"event": "ping", "to": `+to+`}`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO topic_fanout (app_id, source_id, tenant_id, subscriptions, direct, created_at)
		VALUES ('quizzer', 'e1', $1, $2, $3::jsonb, now() - interval '5 minutes')`, tenant, []string{topic}, to); err != nil {
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
		lockWaiters(t, d, 1) // the expiry pass, at the counter (before the fix: having ended d)
		go func() {
			_, err := appapi.FinishFanouts(ctx, d)
			copied <- err
		}()
		lockWaiters(t, d, 2) // the copy too: on the expiry pass's locks (before the fix: at the counter, having ended t)
		return nil
	}))
	if err := <-expired; err != nil {
		t.Errorf("expiring topic subscriptions: %v", err)
	}
	if err := <-copied; err != nil {
		t.Errorf("making the emit's copy: %v", err)
	}
	copiesMade(t, d)
	var left int
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM topic_subscriptions WHERE ended_at IS NULL`).Scan(&left)
	}))
	if left != 0 {
		t.Errorf("%d lapsed subscriptions not ended", left)
	}
}
