package coretest_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// A delivery an instance claimed and never finished (it stopped mid-send:
// a deploy) still comes first: nothing after it goes while its lease
// holds, and once its lease lapses and it's waiting again, it goes, then
// what came after it.
func TestMCPEventsOrderAfterInterruptedSend(t *testing.T) {
	c, app, _, cb, _ := eventsConversation(t, false)
	tenant := tenantOf(t, c, "chat", "u1")
	exec := func(sql string) {
		t.Helper()
		if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	// The stopped instance held the webhook while it claimed "first".
	exec(`UPDATE mcp_event_webhooks SET busy_holder = 'webhook_dead', busy_until = now() + interval '1 hour'`)
	publish(t, c, app, "first")
	waitFor(t, "first's delivery", func() bool { return deliveryStates(t, c, "chat", "u1")["pending"] == 1 })
	exec(`UPDATE deliveries SET state = 'in_flight', attempts = attempts + 1, lease_holder = 'webhook_dead', lease_until = now() + interval '90 seconds'`)
	exec(`UPDATE mcp_event_webhooks SET busy_holder = NULL, busy_until = NULL`)

	publish(t, c, app, "second")
	time.Sleep(time.Second) // many passes (Poll is 50ms)
	if got := texts(cb.got(), "first", "second"); len(got) != 0 {
		t.Fatalf("sent while an earlier delivery was in flight: %v", got)
	}

	// The lease lapses and the sweep returns first to waiting (as
	// deliveries.Reap does).
	exec(`UPDATE deliveries SET state = 'pending', lease_until = NULL, lease_holder = NULL WHERE state = 'in_flight'`)
	waitFor(t, "both events", func() bool { return len(cb.got()) == 2 })
	if got := texts(cb.got(), "first", "second"); fmt.Sprint(got) != "[first second]" {
		t.Fatalf("delivered: %v", got)
	}
}
