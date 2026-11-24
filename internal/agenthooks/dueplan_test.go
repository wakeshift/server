package agenthooks

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/dbtest"
)

// The deliverer reads each destination's conversation's deliveries by
// index, on every Postgres it runs on (CI's "Postgres plans" job runs this
// on several minor versions): never through every delivery of the agent's.
func TestDueWebhooksReadByIndex(t *testing.T) {
	d := dbtest.New(t)
	tenant := dbtest.NewTenant(t, d)
	if err := d.InTenant(context.Background(), tenant, func(tx pgx.Tx) error {
		dbtest.ReadsByTarget(t, tx, "deliveries", dueWebhooksSQL, "", true)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
