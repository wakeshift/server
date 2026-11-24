package subscriptions_test

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/subscriptions"
)

// The cursors of subscriptions that ended over an hour ago are deleted;
// those of live subscriptions, and of ones that just ended (a step or an
// ending may still hold them), stay.
func TestPruneEndedCursors(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	tenant := dbtest.NewTenant(t, d)
	var live, ended, old, oldUnknown string
	err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := agents.Create(ctx, tx, "codex", ""); err != nil {
			return err
		}
		ids := make([]string, 4)
		for i := range ids {
			s, err := subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "codex", EventType: "app.x.ping", CreatedBy: "me@example.com"})
			if err != nil {
				return err
			}
			ids[i] = s.ID
		}
		live, ended, old, oldUnknown = ids[0], ids[1], ids[2], ids[3]
		for _, id := range ids[1:] {
			if _, err := subscriptions.Cancel(ctx, tx, id); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE subscriptions SET cancelled_at = now() - interval '2 hours' WHERE id = $1`, old); err != nil {
			return err
		}
		// Ended before cancelled_at was always set.
		_, err := tx.Exec(ctx, `UPDATE subscriptions SET cancelled_at = NULL WHERE id = $1`, oldUnknown)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var pruned int64
	var left []string
	err = d.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		if pruned, err = subscriptions.PruneEndedCursors(ctx, tx, 100); err != nil {
			return err
		}
		rows, _ := tx.Query(ctx, `SELECT subscription_id::text FROM subscription_cursors ORDER BY subscription_id`)
		left, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 2 || len(left) != 2 || !slices.Contains(left, live) || !slices.Contains(left, ended) {
		t.Fatalf("pruned %d, left %v; want %s and %s left (not %s, %s)", pruned, left, live, ended, old, oldUnknown)
	}
}
