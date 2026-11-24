package mcpevents

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/dbtest"
)

// The deliverer reads each webhook's conversation's deliveries by index,
// on every Postgres it runs on (CI's "Postgres plans" job runs this on
// several minor versions): never through every delivery of the agent's.
func TestDelivererReadsByIndex(t *testing.T) {
	d := dbtest.New(t)
	tenant := dbtest.NewTenant(t, d)
	for name, q := range map[string]struct {
		sql  string
		args []any
	}{
		"prune": {pruneWebhooksSQL, nil},
		"due":   {dueWebhooksSQL, []any{ReauthEvery.String(), ""}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := d.InTenant(context.Background(), tenant, func(tx pgx.Tx) error {
				dbtest.ReadsByTarget(t, tx, "deliveries", q.sql, q.args...)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
