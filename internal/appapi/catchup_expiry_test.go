package appapi

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/subscriptions"
)

// A grant past its end but not yet revoked (the expiry pass revokes it
// within minutes) no longer covers deliveries: live delivery's pacing makes
// none, and neither may an ending subscription's catch-up. Unexpired, the
// same catch-up delivers, so the test can't pass by delivering nothing at
// all.
func TestCatchUpDeliversNothingUnderAnExpiredGrant(t *testing.T) {
	for _, tc := range []struct {
		name    string
		expired bool
		want    int
	}{{"unexpired", false, 1}, {"expired", true, 0}} {
		t.Run(tc.name, func(t *testing.T) {
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
			const topic = "rounds/1"
			var grant, sub, event string
			must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) VALUES (current_tenant(), 'claude')`); err != nil {
					return err
				}
				grant = dbtest.Grant(t, tx, "quizzer", "claude", 24*time.Hour)
				s, err := sessions.Register(ctx, tx, sessions.New{Agent: "claude", Conversation: "thread-1", CreatedBy: "app:quizzer"})
				if err != nil {
					return err
				}
				ns, err := subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "claude", EventType: topicsType("quizzer"), CreatedBy: "app:quizzer",
					GrantID: grant, SessionID: s.ID, Filters: map[string]string{topicAttr(topic): "1"}})
				sub = ns.ID
				return err
			}))
			// Published, and not yet read by the pipeline (none runs here).
			must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
				e, _, err := events.Append(ctx, tx, events.Event{Type: topicsType("quizzer"), Source: "app:quizzer", From: events.Party{Kind: "app", ID: "quizzer"},
					Subject: "after consent ended", Trust: events.External, Attrs: map[string]string{"app": "quizzer", "topic": topic, topicAttr(topic): "1"}})
				event = e.ID
				return err
			}))
			if tc.expired {
				// Past its end; the expiry pass hasn't revoked it yet.
				must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
					_, err := tx.Exec(ctx, `UPDATE grants SET expires_at = now() - interval '1 second' WHERE id = $1`, grant)
					return err
				}))
			}
			must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error { return catchUp(ctx, tx, sub) }))
			must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
				var n int
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE event_id = $1`, event).Scan(&n); err != nil {
					return err
				}
				if n != tc.want {
					t.Errorf("catch-up made %d deliveries, want %d", n, tc.want)
				}
				return nil
			}))
		})
	}
}
