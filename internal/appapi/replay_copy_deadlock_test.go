package appapi_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/subscriptions"
)

// publishWithACopyToMake is a delete-on-delivery publish to rounds/1 whose
// copy into tenant, for subscription a, is still to be made, and a second
// subscription c to the topic, made since (as a subscribe that replays it
// would).
func publishWithACopyToMake(t *testing.T) (d *db.DB, tenant, a, c string) {
	t.Helper()
	d, tenant = quizzerUser(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) VALUES (current_tenant(), 'claude-code')`); err != nil {
			return err
		}
		grant := dbtest.Grant(t, tx, "quizzer", "claude-code", 24*time.Hour)
		mk := func(label string) (string, error) {
			s, err := sessions.Register(ctx, tx, sessions.New{Agent: "claude-code", Label: label, CreatedBy: "app:quizzer"})
			if err != nil {
				return "", err
			}
			sub, err := subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "claude-code", EventType: "topics.quizzer", CreatedBy: "app:quizzer",
				GrantID: grant, SessionID: s.ID})
			if err != nil {
				return "", err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO topic_subscriptions (tenant_id, subscription_id, app_id, topic, session_id, ttl_seconds, expires_at)
				VALUES (current_tenant(), $1, 'quizzer', 'rounds/1', $2, 3600, now() + interval '1 hour')`, sub.ID, s.ID); err != nil {
				return "", err
			}
			_, err = tx.Exec(ctx, `INSERT INTO topic_members (app_id, topic, tenant_id, subscription_id) VALUES ('quizzer', 'rounds/1', current_tenant(), $1)`, sub.ID)
			return sub.ID, err
		}
		var err error
		if a, err = mk("a"); err != nil {
			return err
		}
		c, err = mk("c")
		return err
	}))
	if _, err := d.Pool.Exec(ctx, `INSERT INTO topic_events (app_id, source_id, event_id, topics, text, delete_on_delivery, published_at)
		VALUES ('quizzer', 'p1', gen_random_uuid(), '{rounds/1}', 'hi', true, now() - interval '2 minutes')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO topic_fanout (app_id, source_id, tenant_id, subscriptions, created_at) VALUES ('quizzer', 'p1', $1, $2, now() - interval '2 minutes')`,
		tenant, []string{a}); err != nil {
		t.Fatal(err)
	}
	return d, tenant, a, c
}

// A subscribe replaying a delete-on-delivery publish appended its copy (the
// workspace's event counter), then redacted the publish (its topic_events
// row); the publish's own copy into the same workspace locks that row, then
// appends: each waited on the other. A replay now leaves a publish whose
// copies are still to be made alone (TestReplayLeavesAPublishsCopiesTheirContent),
// so it finishes without the counter, and the copy is made after it.
func TestReplayAndAPublishCopyIntoTheSameWorkspace(t *testing.T) {
	d, tenant, _, c := publishWithACopyToMake(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	replayed, copied := make(chan error, 1), make(chan error, 1)
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO event_counters (tenant_id, last_seq) VALUES (current_tenant(), 0) ON CONFLICT DO NOTHING`)
		return err
	}))
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM event_counters WHERE tenant_id = current_tenant() FOR UPDATE`); err != nil {
			return err
		}
		go func() {
			replayed <- d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
				return appapi.Replay(ctx, tx, "quizzer", "rounds/1", c, time.Now(), time.Hour)
			})
		}()
		select {
		case err := <-replayed:
			if err != nil {
				t.Errorf("replaying: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("the replay waits on the workspace's event counter while a copy is still to be made")
		}
		go func() {
			_, err := appapi.FinishFanouts(ctx, d)
			copied <- err
		}()
		lockWaiters(t, d, 1) // the publish's own copy waits to append
		return nil
	}))
	if err := <-copied; err != nil {
		t.Errorf("making the publish's copy: %v", err)
	}
	copiesMade(t, d)
}

// A subscribe's replay doesn't take a delete-on-delivery publish from the
// subscribers it was published to: while a copy is still to be made (a
// workspace's copy that failed, or the fan-out still under way), its
// content is theirs, and the last copy deletes it. Redacted by the replay,
// that copy had nothing to make, and its subscriber never heard it.
func TestReplayLeavesAPublishsCopiesTheirContent(t *testing.T) {
	d, tenant, a, c := publishWithACopyToMake(t)
	ctx := context.Background()
	if err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return appapi.Replay(ctx, tx, "quizzer", "rounds/1", c, time.Now(), time.Hour)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := appapi.FinishFanouts(ctx, d); err != nil {
		t.Fatal(err)
	}
	var copies int
	if err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM events WHERE source_event_id LIKE 'topic:%'`).Scan(&copies)
	}); err != nil {
		t.Fatal(err)
	}
	if copies != 1 {
		t.Errorf("subscription %s's copy of the publish: %d made, want 1", a, copies)
	}
	var redacted bool
	if err := d.Pool.QueryRow(ctx, `SELECT redacted_at IS NOT NULL FROM topic_events WHERE app_id = 'quizzer' AND source_id = 'p1'`).Scan(&redacted); err != nil {
		t.Fatal(err)
	}
	if !redacted {
		t.Error("the publish kept its content after its last copy")
	}
}
