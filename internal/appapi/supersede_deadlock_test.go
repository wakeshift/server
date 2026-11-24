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

// A subscribe (or unsubscribe) in a conversation locks its session's row
// (lockWatching), then its subscriptions' (refreshing or ending them). A
// redeem that supersedes that session, as the conversation's hook connects
// a newer one, must take the rows in the same order: locking the
// subscriptions first (lockEnding) and the session's row only when it stops
// watching, the two each waited on the other.
func TestSupersedeAndASubscribeInTheOldSession(t *testing.T) {
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
	const thread = "thread-1"
	var session, sub, link string
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) VALUES (current_tenant(), 'codex')`); err != nil {
			return err
		}
		grant := dbtest.Grant(t, tx, "quizzer", "codex", 24*time.Hour)
		s, err := sessions.Register(ctx, tx, sessions.New{Agent: "codex", Conversation: thread, CreatedBy: "app:quizzer"})
		if err != nil {
			return err
		}
		session = s.ID
		if err := sessions.SetWatching(ctx, tx, s.ID, true); err != nil {
			return err
		}
		ns, err := subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "codex", EventType: "topics.quizzer", CreatedBy: "app:quizzer",
			GrantID: grant, SessionID: s.ID})
		if err != nil {
			return err
		}
		sub = ns.ID
		if _, err := tx.Exec(ctx, `INSERT INTO topic_subscriptions (tenant_id, subscription_id, app_id, topic, session_id, ttl_seconds, expires_at)
			VALUES (current_tenant(), $1, 'quizzer', 'rounds/1', $2, 3600, now() + interval '1 hour')`, sub, s.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO topic_members (app_id, topic, tenant_id, subscription_id) VALUES ('quizzer', 'rounds/1', current_tenant(), $1)`, sub); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO stream_links (tenant_id, agent_id, target, ack, created_by, grant_id, expires_at)
			VALUES (current_tenant(), 'codex', $1, true, 'app:quizzer', $2, now() + interval '1 hour') RETURNING id`, s.ID, grant).Scan(&link)
	}))

	// waiting returns once n transactions wait on a lock (an error, not
	// t.Fatal: it runs off the test's goroutine).
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

	locked, superseded := make(chan struct{}), make(chan error, 1)
	subscribed := make(chan error, 1)
	go func() {
		// The subscribe in the old session: its row, then (once the redeem
		// is under way) its subscription.
		subscribed <- d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			if err := lockWatching(ctx, tx, session); err != nil {
				return err
			}
			close(locked)
			if err := waiting(1); err != nil { // the redeem, on what this holds
				return err
			}
			return refreshTopicSub(ctx, tx, sub, "rounds/1", map[string]string{}, false, time.Hour)
		})
	}()
	<-locked
	go func() {
		superseded <- d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			return supersede(ctx, tx, "quizzer", link, "00000000-0000-0000-0000-000000000000", thread)
		})
	}()
	if err := <-subscribed; err != nil {
		t.Errorf("subscribing: %v", err)
	}
	if err := <-superseded; err != nil {
		t.Errorf("superseding: %v", err)
	}
}
