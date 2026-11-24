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

// The console's revoke ends an event subscription as the app's event.end
// does (endEventSubs): a conversation that stopped listening (the app's
// unwatch) asked to stop, so it gets no notice, which would only wait for
// its next subscribe and arrive after it.
func TestConsoleRevokeAfterAnUnwatchLeavesNoNotice(t *testing.T) {
	d, tenant := quizzerUser(t)
	ctx := context.Background()
	err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
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
		var es string
		if err := tx.QueryRow(ctx, `INSERT INTO event_subscriptions (tenant_id, source_kind, source_id, event, arguments, arguments_key, destination, agent_id, session_id, subscription_id, created_by, principal)
			VALUES (current_tenant(), 'app', 'quizzer', 'ping', '{}', 'k', 'conversation', 'claude-code', $1, $2, 'app:quizzer', 'bob') RETURNING id::text`, s.ID, sub.ID).Scan(&es); err != nil {
			return err
		}
		// The app's unwatch: the session stopped listening.
		if err := sessions.SetWatching(ctx, tx, s.ID, false); err != nil {
			return err
		}
		if _, err := appapi.RevokeEventSubscription(ctx, tx, es, audit.User("someone@example.com")); err != nil {
			return err
		}
		var waiting int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE target = $1 AND state = 'pending'`, s.ID).Scan(&waiting); err != nil {
			return err
		}
		if waiting != 0 {
			t.Errorf("%d deliveries wait for a session that stopped listening (the revoke's notice)", waiting)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
