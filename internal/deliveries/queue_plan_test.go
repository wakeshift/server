package deliveries

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/subscriptions"
)

// A session's queue is read by its target even under a generic plan, which
// Postgres may cache for pgx's prepared statements after five runs: the
// plan is made without the parameters' values, so a CASE on the target's
// placeholder (as the queue's condition was) isn't folded, and every
// claim, pending check and ack walked all of the agent's deliveries, a
// month of them in a big workspace. Seeded with an agent whose deliveries
// are spread over many sessions, so the plans are those of a real queue.
func TestSessionQueueGenericPlanReadsByTarget(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	tenant := dbtest.NewTenant(t, d)
	owner := dbtest.Owner(t, d)
	const others = 5000
	err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := agents.Create(ctx, tx, "codex", ""); err != nil {
			return err
		}
		sub, err := subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "codex", EventType: "test.*", CreatedBy: "test"})
		if err != nil {
			return err
		}
		e, _, err := events.Append(ctx, tx, events.Event{Type: "test.ping", Source: "test", From: events.Party{Kind: "hub", ID: "hub"}, Trust: events.Internal})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO events SELECT (jsonb_populate_record(NULL::events, to_jsonb(b) || jsonb_build_object('seq', b.seq + g, 'id', gen_random_uuid()))).*
			FROM (SELECT * FROM events WHERE id = $1) b, generate_series(1, $2) g`, e.ID, others); err != nil {
			return err
		}
		// Half pending, half in flight, over 500 sessions and the agent's own.
		_, err = tx.Exec(ctx, `INSERT INTO deliveries (tenant_id, id, agent_id, target, event_id, event_seq, subscription_id, state, lease_holder, lease_until)
			SELECT current_tenant(), gen_random_uuid(), 'codex', CASE WHEN e.seq % 50 = 0 THEN '' ELSE 'ses-' || (e.seq % 500) END, e.id, e.seq, $1,
				CASE WHEN e.seq % 2 = 0 THEN 'pending' ELSE 'in_flight' END,
				CASE WHEN e.seq % 2 = 1 THEN 'h' END, CASE WHEN e.seq % 2 = 1 THEN now() + interval '1 minute' END
			FROM events e`, sub.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `ANALYZE deliveries`); err != nil {
		t.Fatal(err)
	}

	const target = "ses-7"
	queries := map[string]string{
		"claim":           claimSQL(target, false),
		"claim turn-held": claimSQL(target, true),
		"pending":         pendingSQL(target),
		"to own stream":   toOwnStreamSQL,
		// The shape of every ack, resend and next-available read.
		"in queue": `SELECT id FROM deliveries WHERE lease_holder = $3 AND agent_id = $1 AND ` + inQueue(target, "$2") + ` AND state IN ('in_flight', 'pending')`,
	}
	for name, q := range queries {
		err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			conds, plan := dbtest.GenericIndexConds(t, tx, "deliveries", q)
			// Every read of the agent's deliveries narrows to the target
			// (claim's UPDATE also finds each claimed row by its key).
			byAgent := 0
			for _, c := range conds {
				if !strings.Contains(c, "agent_id = ") {
					continue
				}
				byAgent++
				if !strings.Contains(c, "target = ") {
					t.Errorf("%s: the generic plan reads the agent's deliveries without target: %q\nplan: %s", name, c, plan)
				}
			}
			if byAgent == 0 {
				t.Errorf("%s: the generic plan doesn't read deliveries by agent: %q\nplan: %s", name, conds, plan)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
