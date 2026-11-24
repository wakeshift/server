package coretest_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/mcpevents"
)

// A webhook's retries belong to the delivery being retried: one canceled
// while it was backing off (its subscription ended) doesn't pass its count
// or retry window on to the next, which gets its own MaxAttempts.
func TestMCPEventsRetryStateIsPerDelivery(t *testing.T) {
	c, app, _, cb, _ := eventsConversation(t, false)
	base := mcpevents.BackoffBase
	mcpevents.BackoffBase = 200 * time.Millisecond // room to cancel between attempts
	t.Cleanup(func() { mcpevents.BackoffBase = base })
	tenant := tenantOf(t, c, "chat", "u1")
	cb.status = func(n int) int {
		if n <= 5 {
			return 503 // a's four attempts and b's first
		}
		return 200
	}
	publish(t, c, app, "a")
	waitFor(t, "a's fourth attempt", func() bool { return len(cb.tried()) == 4 })
	// Canceled as ending its subscription would: the webhook locked before
	// its deliveries, the order the deliverer records an attempt in (its
	// fourth may still be being recorded). The other way round, the two
	// deadlock.
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE mcp_event_webhooks SET next_attempt_at = NULL`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'cancelled', finished_at = now(), lease_until = NULL, lease_holder = NULL
			WHERE state IN ('pending', 'in_flight')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	publish(t, c, app, "b")
	waitFor(t, "b delivered or abandoned", func() bool { return len(cb.got()) == 1 || len(cb.envelopes()) > 0 })
	want := []string{"a", "b"}
	order := []string{}
	for _, x := range cb.tried() {
		order = append(order, which(x, want))
	}
	if fmt.Sprint(order) != "[a a a a b b]" || len(cb.envelopes()) != 0 {
		t.Fatalf("attempts %v, envelopes %+v", order, cb.envelopes())
	}
}
