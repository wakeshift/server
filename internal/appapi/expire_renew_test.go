package appapi

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/subscriptions"
)

// The expiry pass finds a lapsed topic subscription, then locks it
// (lockEnding). A subscribe renewing it in between (its ttl started again,
// committed) must not see it ended a moment later with a "ttl" notice: the
// pass ends only what's still lapsed once it holds it.
func TestExpiryPassSparesASubscriptionRenewedMeanwhile(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO apps (id, name) VALUES ('quizzer', 'Quizzer')`); err != nil {
		t.Fatal(err)
	}
	tenant := dbtest.NewTenant(t, d)
	var sub string
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) VALUES (current_tenant(), 'claude-code')`); err != nil {
			return err
		}
		grant := dbtest.Grant(t, tx, "quizzer", "claude-code", 24*time.Hour)
		s, err := sessions.Register(ctx, tx, sessions.New{Agent: "claude-code", Label: "a", CreatedBy: "app:quizzer"})
		if err != nil {
			return err
		}
		ns, err := subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "claude-code", EventType: "topics.quizzer", CreatedBy: "app:quizzer",
			GrantID: grant, SessionID: s.ID})
		if err != nil {
			return err
		}
		sub = ns.ID
		_, err = tx.Exec(ctx, `INSERT INTO topic_subscriptions (tenant_id, subscription_id, app_id, topic, session_id, ttl_seconds, expires_at)
			VALUES (current_tenant(), $1, 'quizzer', 'rounds/1', $2, 60, now() - interval '1 minute')`, sub, s.ID)
		return err
	}))
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
	expired := make(chan error, 1)
	// The subscription's cursor held, so the pass stops between finding it
	// and locking it.
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM subscription_cursors WHERE subscription_id = $1 FOR UPDATE`, sub); err != nil {
			return err
		}
		go func() {
			expired <- d.InTenant(ctx, tenant, func(tx pgx.Tx) error { _, err := ExpireTopics(ctx, tx); return err })
		}()
		if err := waiting(1); err != nil {
			return err
		}
		// The renewal, committed meanwhile.
		return d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			return refreshTopicSub(ctx, tx, sub, "rounds/1", map[string]string{}, false, time.Hour)
		})
	}))
	must(<-expired)
	var ended bool
	var status string
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT ts.ended_at IS NOT NULL, s.status FROM topic_subscriptions ts JOIN subscriptions s ON s.id = ts.subscription_id
			WHERE ts.subscription_id = $1`, sub).Scan(&ended, &status)
	}))
	if ended || status != subscriptions.Active {
		t.Fatalf("the renewed subscription was ended by the expiry pass (ended %v, status %s)", ended, status)
	}
}

// The same for a publish's copy: it found a subscriber lapsed, then locked
// it; renewed in between, the subscriber gets the copy and isn't ended.
func TestCopySparesASubscriptionRenewedMeanwhile(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO apps (id, name) VALUES ('quizzer', 'Quizzer')`); err != nil {
		t.Fatal(err)
	}
	tenant := dbtest.NewTenant(t, d)
	var sub string
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) VALUES (current_tenant(), 'claude-code')`); err != nil {
			return err
		}
		grant := dbtest.Grant(t, tx, "quizzer", "claude-code", 24*time.Hour)
		s, err := sessions.Register(ctx, tx, sessions.New{Agent: "claude-code", Label: "a", CreatedBy: "app:quizzer"})
		if err != nil {
			return err
		}
		ns, err := subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "claude-code", EventType: "topics.quizzer", CreatedBy: "app:quizzer",
			GrantID: grant, SessionID: s.ID})
		if err != nil {
			return err
		}
		sub = ns.ID
		_, err = tx.Exec(ctx, `INSERT INTO topic_subscriptions (tenant_id, subscription_id, app_id, topic, session_id, ttl_seconds, expires_at)
			VALUES (current_tenant(), $1, 'quizzer', 'rounds/1', $2, 60, now() - interval '1 minute')`, sub, s.ID)
		return err
	}))
	if _, err := d.Pool.Exec(ctx, `INSERT INTO topic_events (app_id, source_id, event_id, topics, text) VALUES ('quizzer', 'p1', gen_random_uuid(), '{rounds/1}', 'hi')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO topic_fanout (app_id, source_id, tenant_id, subscriptions, created_at)
		VALUES ('quizzer', 'p1', $1, $2, now() - interval '2 minutes')`, tenant, []string{sub}); err != nil {
		t.Fatal(err)
	}
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
	copied := make(chan error, 1)
	// The subscriber's cursor held, so the copy stops between finding it
	// lapsed and locking it.
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM subscription_cursors WHERE subscription_id = $1 FOR UPDATE`, sub); err != nil {
			return err
		}
		go func() {
			_, err := FinishFanouts(ctx, d)
			copied <- err
		}()
		if err := waiting(1); err != nil {
			return err
		}
		return d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			return refreshTopicSub(ctx, tx, sub, "rounds/1", map[string]string{}, false, time.Hour)
		})
	}))
	must(<-copied)
	var ended bool
	var copies int
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT ended_at IS NOT NULL FROM topic_subscriptions WHERE subscription_id = $1`, sub).Scan(&ended); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM events WHERE source_event_id LIKE 'topic:%'`).Scan(&copies)
	}))
	if ended || copies != 1 {
		t.Fatalf("the renewed subscriber: ended %v, %d copies (want live, 1)", ended, copies)
	}
}
