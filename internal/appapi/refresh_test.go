package appapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/subscriptions"
)

// A refresh that gets its subscription's row only after the expiry pass
// ended it (which doesn't lock the event subscription record) refreshes
// nothing and says so, so the subscribe makes a new one instead of
// answering "renewed" for routing that's over.
func TestRefreshingEndedRouting(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	if _, err := d.Pool.Exec(ctx, `INSERT INTO apps (id, name) VALUES ('quizzer', 'Quizzer')`); err != nil {
		t.Fatal(err)
	}
	tenant := dbtest.NewTenant(t, d)
	err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) VALUES (current_tenant(), 'claude-code')`); err != nil {
			return err
		}
		grant := dbtest.Grant(t, tx, "quizzer", "claude-code", 24*time.Hour)
		s, err := sessions.Register(ctx, tx, sessions.New{Agent: "claude-code", Label: "a", CreatedBy: "app:quizzer"})
		if err != nil {
			return err
		}
		live := func() (string, error) {
			ns, err := subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "claude-code", EventType: "topics.quizzer", CreatedBy: "app:quizzer",
				GrantID: grant, SessionID: s.ID})
			if err != nil {
				return "", err
			}
			_, err = tx.Exec(ctx, `INSERT INTO topic_subscriptions (tenant_id, subscription_id, app_id, topic, session_id, ttl_seconds, expires_at)
				VALUES (current_tenant(), $1, 'quizzer', 'rounds/1', $2, 60, now() + interval '1 minute')`, ns.ID, s.ID)
			return ns.ID, err
		}
		ended, err := live()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE topic_subscriptions SET ended_at = now(), ended_reason = 'ttl' WHERE subscription_id = $1`, ended); err != nil {
			return err
		}
		if err := subscriptions.Finish(ctx, tx, ended); err != nil {
			return err
		}
		if err := refreshTopicSub(ctx, tx, ended, "rounds/1", map[string]string{}, false, time.Hour); !errors.Is(err, errRoutingEnded) {
			t.Errorf("refreshing ended routing: %v, not errRoutingEnded", err)
		}
		var expires time.Time
		if err := tx.QueryRow(ctx, `SELECT expires_at FROM topic_subscriptions WHERE subscription_id = $1`, ended).Scan(&expires); err != nil {
			return err
		}
		if time.Until(expires) > 2*time.Minute {
			t.Errorf("the ended routing's ttl was started again: %v", expires)
		}
		ok, err := live()
		if err != nil {
			return err
		}
		if err := refreshTopicSub(ctx, tx, ok, "rounds/1", map[string]string{}, false, time.Hour); err != nil {
			t.Errorf("refreshing live routing: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
