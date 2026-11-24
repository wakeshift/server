package deliveries_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/subscriptions"
)

// An app claims, drains, acknowledges and paces only what its own
// subscriptions delivered to a session; "" (the agent's own channels) takes
// every app's.
func TestAppScopedDeliveries(t *testing.T) {
	d := dbtest.New(t)
	tenant := dbtest.NewTenant(t, d)
	if _, err := d.Pool.Exec(ctx, `INSERT INTO apps (id, name) VALUES ('chat', 'Chat'), ('relay', 'Relay')`); err != nil {
		t.Fatal(err)
	}
	const session = "11111111-1111-4111-8111-111111111111"
	ids := map[string]string{} // delivery IDs by app ("" for the console's)
	in(t, d, tenant, func(tx pgx.Tx) error {
		agents.Create(ctx, tx, "codex", "")
		for _, app := range []string{"chat", "relay", ""} {
			n := subscriptions.New{AgentID: "codex", EventType: "test.*", CreatedBy: "test"}
			if app != "" {
				n.GrantID = dbtest.Grant(t, tx, app, "codex", time.Hour)
			}
			sub, err := subscriptions.Create(ctx, tx, n)
			if err != nil {
				return err
			}
			for i, at := range []time.Time{{}, time.Now().Add(time.Hour)} {
				e, _, err := events.Append(ctx, tx, events.Event{Type: "test.ping", Source: "test", Subject: app, From: events.Party{Kind: "hub", ID: "hub"}, Trust: events.Internal})
				if err != nil {
					return err
				}
				if _, err := deliveries.Insert(ctx, tx, deliveries.New{AgentID: "codex", Target: session, SubscriptionID: sub.ID, Event: e, AvailableAt: at}); err != nil {
					return err
				}
				if i == 0 {
					ids[app] = deliveries.ID("codex", e.ID, session)
				}
			}
		}
		return nil
	})

	subjects := func(list []deliveries.Delivery) []string {
		var out []string
		for _, dl := range list {
			out = append(out, dl.Event.Subject)
		}
		return out
	}
	in(t, d, tenant, func(tx pgx.Tx) error {
		// Paced: only its own next one counts.
		if next, err := deliveries.NextAvailableFor(ctx, tx, "codex", session, "relay"); err != nil || next.IsZero() {
			t.Errorf("relay's next: %v %v", next, err)
		}
		// Acks: not another app's, nor the console's.
		for _, other := range []string{"chat", ""} {
			if ok, err := deliveries.AckFor(ctx, tx, "codex", session, "relay", ids[other]); err != nil || ok {
				t.Errorf("relay acked %q's delivery: %v %v", other, ok, err)
			}
		}
		got, err := deliveries.ClaimFor(ctx, tx, "codex", session, "relay", "relay-wait", time.Minute, 10)
		if err != nil || len(got) != 1 || got[0].Event.Subject != "relay" {
			t.Errorf("relay claimed %v %v", subjects(got), err)
		}
		if ok, err := deliveries.AckFor(ctx, tx, "codex", session, "relay", ids["relay"]); err != nil || !ok {
			t.Errorf("relay's own ack: %v %v", ok, err)
		}
		got, err = deliveries.DrainFor(ctx, tx, "codex", session, "chat", 10)
		if err != nil || len(got) != 1 || got[0].Event.Subject != "chat" {
			t.Errorf("chat drained %v %v", subjects(got), err)
		}
		// The agent's own channel gets what's left: the console's.
		got, err = deliveries.Claim(ctx, tx, "codex", session, "link", time.Minute, 10)
		if err != nil || len(got) != 1 || got[0].Event.Subject != "" {
			t.Errorf("the agent's link claimed %v %v", subjects(got), err)
		}
		return nil
	})
}
