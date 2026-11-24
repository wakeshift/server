package pipeline

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/subscriptions"
)

// A subscription no session claimed delivers to a session it picks, so its
// copy and that session's own subscription's share one delivery, owned by
// whichever made it. Canceling the owner leaves the delivery to the
// other, whichever it is; a third conversation's subscription keeps
// nothing of it.
func TestCancelKeepsAPickedSessionsCopy(t *testing.T) {
	f := setup(t)
	session := func() string {
		var s sessions.Session
		f.tx(func(tx pgx.Tx) (err error) {
			s, err = sessions.Register(ctx, tx, sessions.New{Agent: "claude-code"})
			return err
		})
		return s.ID
	}
	s1 := session()
	s2 := session() // the more recently active: what's picked
	f.tx(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sessions SET last_active_at = now() + interval '1 minute' WHERE id = $1`, s2)
		return err
	})
	subscribe := func(session string) subscriptions.Subscription {
		var s subscriptions.Subscription
		f.tx(func(tx pgx.Tx) (err error) {
			s, err = subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "claude-code", EventType: "slack.message", SessionID: session, CreatedBy: "test"})
			return err
		})
		return s
	}
	wide := subscribe("")
	subscribe(s1)
	own := subscribe(s2)
	e := f.slack("e1", "C1")
	if _, err := (&Runner{DB: f.d}).Tenant(ctx, f.tenant); err != nil {
		t.Fatal(err)
	}
	get := func(target string) deliveries.Delivery {
		t.Helper()
		var d deliveries.Delivery
		f.tx(func(tx pgx.Tx) (err error) {
			d, err = deliveries.Get(ctx, tx, deliveries.ID("claude-code", e.ID, target))
			return err
		})
		return d
	}
	first := get(s2)
	if first.SubscriptionID != wide.ID && first.SubscriptionID != own.ID {
		t.Fatalf("the picked session's copy is owned by %s", first.SubscriptionID)
	}
	f.cancel(first.SubscriptionID)
	heir := wide.ID
	if first.SubscriptionID == wide.ID {
		heir = own.ID
	}
	if d := get(s2); d.State != "pending" || d.SubscriptionID != heir {
		t.Fatalf("after canceling its owner the picked session's copy is %s, owned by %s (want %s)", d.State, d.SubscriptionID, heir)
	}
	f.cancel(heir)
	if d := get(s2); d.State != "cancelled" {
		t.Fatalf("with only another conversation's subscription left the copy is %s", d.State)
	}
	if d := get(s1); d.State != "pending" {
		t.Fatalf("the other conversation's own copy is %s", d.State)
	}
}
