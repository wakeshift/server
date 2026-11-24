package appapi

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/dbtest"
)

// StrandedCodex reads each session's stream lease by index, on every
// Postgres it runs on (CI's "Postgres plans" job runs this on several
// minor versions): never through every lease of the agent's. (Its
// deliveries are a semi-join the planner may run over the pending ones,
// by deliveries_queue, whichever way it likes.)
func TestStrandedCodexReadsByIndex(t *testing.T) {
	d := dbtest.New(t)
	tenant := dbtest.NewTenant(t, d)
	if err := d.InTenant(context.Background(), tenant, func(tx pgx.Tx) error {
		dbtest.ReadsByTarget(t, tx, "stream_leases", strandedCodexSQL, StrandedAfter.String())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
