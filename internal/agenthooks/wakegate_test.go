package agenthooks

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/wakeshift/server/internal/dbtest"
)

// A Wake for an agent in a workspace with no agent-hosted destination is
// never handed to a claim; one in a workspace with one is.
func TestWakeSkipsWorkspacesWithoutDestinations(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	with, without := dbtest.NewTenant(t, d), dbtest.NewTenant(t, d)
	if _, err := d.Pool.Exec(ctx, `INSERT INTO agent_webhook_targets (tenant_id, session_id) VALUES ($1, $2)`, with, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	dv := &Deliverer{DB: d}
	got, err := dv.hasTargets(ctx, []string{with, without})
	if err != nil {
		t.Fatal(err)
	}
	if !got[with] || got[without] || len(got) != 1 {
		t.Fatalf("workspaces with destinations: %v; want only %s", got, with)
	}
	dv.Wake(without, "codex")
	dv.Wake(with, "codex")
	var claimed []string
	dv.gate.Check(ctx, dv.hasTargets, func(tenant, agent string) { claimed = append(claimed, tenant+"/"+agent) })
	if len(claimed) != 1 || claimed[0] != with+"/codex" {
		t.Fatalf("handed to a claim: %v; want only %s/codex", claimed, with)
	}
}
