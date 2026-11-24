package deliveries_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/subscriptions"
)

var ctx = context.Background()

// setup creates an agent with a subscription and n pending deliveries.
func setup(t *testing.T, n int) (*db.DB, string) {
	t.Helper()
	d := dbtest.New(t)
	tenant := dbtest.NewTenant(t, d)
	err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		agents.Create(ctx, tx, "codex", "")
		sub, err := subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "codex", EventType: "test.*", CreatedBy: "test"})
		if err != nil {
			return err
		}
		for range n {
			e, _, err := events.Append(ctx, tx, events.Event{Type: "test.ping", Source: "test", From: events.Party{Kind: "hub", ID: "hub"}, Trust: events.Internal})
			if err != nil {
				return err
			}
			if _, err := deliveries.Insert(ctx, tx, deliveries.New{AgentID: "codex", SubscriptionID: sub.ID, Event: e}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return d, tenant
}

func in(t *testing.T, d *db.DB, tenant string, fn func(tx pgx.Tx) error) {
	t.Helper()
	if err := d.InTenant(ctx, tenant, fn); err != nil {
		t.Fatal(err)
	}
}

func TestIDIsDeterministic(t *testing.T) {
	a, b := deliveries.ID("codex", "e1", ""), deliveries.ID("codex", "e1", "")
	if a != b || a == deliveries.ID("codex", "e1", "ses_1") || len(a) != 36 {
		t.Fatalf("%s %s", a, b)
	}
}

func TestClaimLeaseAndFinish(t *testing.T) {
	d, tenant := setup(t, 3)
	var first []deliveries.Delivery
	in(t, d, tenant, func(tx pgx.Tx) (err error) {
		first, err = deliveries.Claim(ctx, tx, "codex", "", "stream-a", time.Minute, 2)
		return err
	})
	if len(first) != 2 || first[0].Event.Seq >= first[1].Event.Seq || first[0].Attempts != 1 {
		t.Fatalf("claimed %+v", first)
	}
	// Another stream gets only what's left.
	in(t, d, tenant, func(tx pgx.Tx) error {
		rest, err := deliveries.Claim(ctx, tx, "codex", "", "stream-b", time.Minute, 10)
		if len(rest) != 1 {
			t.Errorf("second claim got %d", len(rest))
		}
		return err
	})
	in(t, d, tenant, func(tx pgx.Tx) error {
		// Only the holder can mark its deliveries delivered.
		deliveries.Delivered(ctx, tx, "stream-b", []string{first[0].ID})
		if got, _ := deliveries.Get(ctx, tx, first[0].ID); got.State != "in_flight" {
			t.Errorf("another holder finished it: %s", got.State)
		}
		deliveries.Delivered(ctx, tx, "stream-a", []string{first[0].ID})
		if got, _ := deliveries.Get(ctx, tx, first[0].ID); got.State != "delivered" {
			t.Errorf("state %s", got.State)
		}
		if other, _ := deliveries.Ack(ctx, tx, "claude-code", "", first[1].ID); other {
			t.Error("another agent acked it")
		}
		ok, err := deliveries.Ack(ctx, tx, "codex", "", first[1].ID)
		if !ok {
			t.Error("ack")
		}
		if again, _ := deliveries.Ack(ctx, tx, "codex", "", first[1].ID); !again {
			t.Error("a repeated ack isn't confirmed")
		}
		if delivered, _ := deliveries.Ack(ctx, tx, "codex", "", first[0].ID); delivered {
			t.Error("acked a delivery that was already finished as delivered")
		}
		return err
	})
}

func TestReleaseAndReap(t *testing.T) {
	d, tenant := setup(t, 2)
	in(t, d, tenant, func(tx pgx.Tx) error {
		_, err := deliveries.Claim(ctx, tx, "codex", "", "dead-stream", time.Minute, 1)
		return err
	})
	// The stream closed: its lease goes back.
	in(t, d, tenant, func(tx pgx.Tx) error { return deliveries.Release(ctx, tx, "dead-stream") })
	in(t, d, tenant, func(tx pgx.Tx) error {
		got, err := deliveries.Claim(ctx, tx, "codex", "", "s", -time.Second, 10) // already expired
		if len(got) != 2 || got[0].Attempts != 2 {
			t.Errorf("after release: %+v", got)
		}
		return err
	})
	// The stream vanished without releasing: the lease expires and is reaped.
	in(t, d, tenant, func(tx pgx.Tx) error {
		n, err := deliveries.Reap(ctx, tx)
		if n != 2 {
			t.Errorf("reaped %d", n)
		}
		return err
	})
}

func TestTooManyAttemptsIsDead(t *testing.T) {
	d, tenant := setup(t, 1)
	for range deliveries.MaxAttempts {
		in(t, d, tenant, func(tx pgx.Tx) error {
			if _, err := deliveries.Claim(ctx, tx, "codex", "", "s", time.Minute, 1); err != nil {
				return err
			}
			return deliveries.Release(ctx, tx, "s")
		})
	}
	in(t, d, tenant, func(tx pgx.Tx) error {
		left, err := deliveries.Claim(ctx, tx, "codex", "", "s", time.Minute, 1)
		if len(left) != 0 {
			t.Errorf("a dead delivery was claimed again: %+v", left)
		}
		return err
	})
}

// A connector session whose connector then redeems a ticket holds its own
// stream (runtime stream): what was still left for the agent's connector
// stream (in flight there, or queued by a runner step that began before the
// switch) goes to the session's stream from then on, and it can ack it.
func TestConnectorDeliveriesFollowTheSession(t *testing.T) {
	d, tenant := setup(t, 0)
	var s sessions.Session
	var ids []string
	in(t, d, tenant, func(tx pgx.Tx) (err error) {
		s, err = sessions.Register(ctx, tx, sessions.New{Agent: "codex", Conversation: "thread-1", Runtime: sessions.Connector, CreatedBy: "test"})
		if err != nil {
			return err
		}
		var sub string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM subscriptions WHERE agent_id = 'codex'`).Scan(&sub); err != nil {
			return err
		}
		for range 2 {
			e, _, err := events.Append(ctx, tx, events.Event{Type: "test.ping", Source: "test", From: events.Party{Kind: "hub", ID: "hub"}, Trust: events.Internal})
			if err != nil {
				return err
			}
			if _, err := deliveries.Insert(ctx, tx, deliveries.New{AgentID: "codex", Target: s.ID, DeliverTo: s.ID, SubscriptionID: sub, Event: e}); err != nil {
				return err
			}
			ids = append(ids, deliveries.ID("codex", e.ID, s.ID))
		}
		return nil
	})
	// The agent's own stream (target "") holds the first when the
	// session switches; the second is still queued for it.
	in(t, d, tenant, func(tx pgx.Tx) error {
		got, err := deliveries.Claim(ctx, tx, "codex", "", "mac", time.Minute, 1)
		if len(got) != 1 || got[0].DeliverTo == nil {
			t.Errorf("connector stream claimed %+v", got)
		}
		return err
	})
	in(t, d, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sessions SET runtime = 'stream' WHERE id = $1`, s.ID)
		return err
	})
	in(t, d, tenant, func(tx pgx.Tx) error { return deliveries.Release(ctx, tx, "mac") })
	in(t, d, tenant, func(tx pgx.Tx) error {
		if got, err := deliveries.Claim(ctx, tx, "codex", "", "mac", time.Minute, 10); err != nil || len(got) != 0 {
			t.Errorf("the connector stream still claims the session's deliveries: %+v %v", got, err)
		}
		got, err := deliveries.Claim(ctx, tx, "codex", s.ID, "codex-connector", time.Minute, 10)
		if err != nil {
			return err
		}
		if len(got) != 2 || got[0].DeliverTo != nil {
			t.Fatalf("the session's own stream claimed %+v", got)
		}
		for _, id := range ids {
			if ok, err := deliveries.Ack(ctx, tx, "codex", s.ID, id); err != nil || !ok {
				t.Errorf("the session couldn't ack %s: %v", id, err)
			}
		}
		return nil
	})
}

// ToOwnStream hands a connector session's pending deliveries to the session
// (it waits, or holds a stream, itself now): the session claims and acks
// them, and the agent's connector stream no longer does. It changes only
// pending ones: what's in flight keeps deliver_to, and follows the session
// by claimable once the session is no longer a connector session (as
// before ToOwnStream named the agent; TestConnectorDeliveriesFollowTheSession).
func TestToOwnStream(t *testing.T) {
	d, tenant := setup(t, 0)
	var s sessions.Session
	in(t, d, tenant, func(tx pgx.Tx) (err error) {
		s, err = sessions.Register(ctx, tx, sessions.New{Agent: "codex", Conversation: "thread-1", Runtime: sessions.Connector, CreatedBy: "test"})
		if err != nil {
			return err
		}
		var sub string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM subscriptions WHERE agent_id = 'codex'`).Scan(&sub); err != nil {
			return err
		}
		for range 2 {
			e, _, err := events.Append(ctx, tx, events.Event{Type: "test.ping", Source: "test", From: events.Party{Kind: "hub", ID: "hub"}, Trust: events.Internal})
			if err != nil {
				return err
			}
			if _, err := deliveries.Insert(ctx, tx, deliveries.New{AgentID: "codex", Target: s.ID, DeliverTo: s.ID, SubscriptionID: sub, Event: e}); err != nil {
				return err
			}
		}
		return nil
	})
	in(t, d, tenant, func(tx pgx.Tx) error {
		got, err := deliveries.Claim(ctx, tx, "codex", "", "mac", time.Minute, 1)
		if len(got) != 1 {
			t.Errorf("connector stream claimed %+v", got)
		}
		return err
	})
	in(t, d, tenant, func(tx pgx.Tx) error {
		// Another agent's name changes nothing.
		if err := deliveries.ToOwnStream(ctx, tx, "claude", s.ID); err != nil {
			return err
		}
		if ok, err := deliveries.Pending(ctx, tx, "codex", s.ID); err != nil || ok {
			t.Errorf("another agent's ToOwnStream moved the session's deliveries: %v %v", ok, err)
		}
		return deliveries.ToOwnStream(ctx, tx, "codex", s.ID)
	})
	in(t, d, tenant, func(tx pgx.Tx) error {
		if ok, err := deliveries.Pending(ctx, tx, "codex", ""); err != nil || ok {
			t.Errorf("the connector stream still has the session's pending delivery: %v %v", ok, err)
		}
		got, err := deliveries.Claim(ctx, tx, "codex", s.ID, "own", time.Minute, 10)
		if err != nil {
			return err
		}
		if len(got) != 1 || got[0].DeliverTo != nil {
			t.Fatalf("the session's own stream claimed %+v", got)
		}
		if ok, err := deliveries.Ack(ctx, tx, "codex", s.ID, got[0].ID); err != nil || !ok {
			t.Errorf("the session couldn't ack it: %v %v", ok, err)
		}
		var held int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE lease_holder = 'mac' AND state = 'in_flight' AND deliver_to IS NOT NULL`).Scan(&held); err != nil {
			return err
		}
		if held != 1 {
			t.Errorf("%d still in flight on the connector stream, want 1", held)
		}
		return nil
	})
}

// Resend finds the delivery a client last saw by its ID (compared as a
// uuid); a malformed one names none, and isn't an error.
func TestResendByLastSeenID(t *testing.T) {
	d, tenant := setup(t, 2)
	in(t, d, tenant, func(tx pgx.Tx) error {
		got, err := deliveries.Claim(ctx, tx, "codex", "", "stream-a", time.Minute, 2)
		if err != nil || len(got) != 2 {
			t.Fatalf("claimed %d: %v", len(got), err)
		}
		if err := deliveries.Delivered(ctx, tx, "stream-a", []string{got[0].ID, got[1].ID}); err != nil {
			return err
		}
		if n, err := deliveries.Resend(ctx, tx, "codex", "", "not-a-uuid", "stream-a", time.Minute); n != 0 || err != nil {
			t.Fatalf("malformed ID: %d, %v", n, err)
		}
		n, err := deliveries.Resend(ctx, tx, "codex", "", got[0].ID, "", time.Minute)
		if n != 1 {
			t.Fatalf("resent %d after the first", n)
		}
		if again, _ := deliveries.Get(ctx, tx, got[1].ID); again.State != "pending" {
			t.Fatalf("the second is %s", again.State)
		}
		return err
	})
}

// Resend from a known connection sends again only what that connection
// wrote: what another one (or a tool result) handed over stays delivered.
func TestResendOnlyWhatThePreviousConnectionWrote(t *testing.T) {
	d, tenant := setup(t, 3)
	in(t, d, tenant, func(tx pgx.Tx) error {
		mine, err := deliveries.Claim(ctx, tx, "codex", "", "stream-a", time.Minute, 2)
		if err != nil || len(mine) != 2 {
			t.Fatalf("claimed %d: %v", len(mine), err)
		}
		theirs, err := deliveries.Claim(ctx, tx, "codex", "", "stream-b", time.Minute, 1)
		if err != nil || len(theirs) != 1 {
			t.Fatalf("claimed %d: %v", len(theirs), err)
		}
		deliveries.Delivered(ctx, tx, "stream-a", []string{mine[0].ID, mine[1].ID})
		deliveries.Delivered(ctx, tx, "stream-b", []string{theirs[0].ID})
		n, err := deliveries.Resend(ctx, tx, "codex", "", mine[0].ID, "stream-a", time.Minute)
		if err != nil || n != 1 {
			t.Fatalf("resent %d: %v", n, err)
		}
		if g, _ := deliveries.Get(ctx, tx, theirs[0].ID); g.State != "delivered" {
			t.Fatalf("another connection's delivery is %s", g.State)
		}
		return nil
	})
}

// Resend without a known previous connection (prev empty: the stream it
// reconnects from left no lease) sends again only what the connection that
// wrote the last-seen delivery wrote after it: what a tool result drained
// meanwhile, or another connection handed over, was received there, and
// sending it again would wake the agent twice.
func TestResendWithoutPrevSkipsWhatWasDrained(t *testing.T) {
	d, tenant := setup(t, 4)
	in(t, d, tenant, func(tx pgx.Tx) error {
		mine, err := deliveries.Claim(ctx, tx, "codex", "", "stream-a", time.Minute, 2)
		if err != nil || len(mine) != 2 {
			t.Fatalf("claimed %d: %v", len(mine), err)
		}
		if err := deliveries.Delivered(ctx, tx, "stream-a", []string{mine[0].ID, mine[1].ID}); err != nil {
			return err
		}
		theirs, err := deliveries.Claim(ctx, tx, "codex", "", "stream-b", time.Minute, 1)
		if err != nil || len(theirs) != 1 {
			t.Fatalf("claimed %d: %v", len(theirs), err)
		}
		if err := deliveries.Delivered(ctx, tx, "stream-b", []string{theirs[0].ID}); err != nil {
			return err
		}
		drained, err := deliveries.Drain(ctx, tx, "codex", "", 10)
		if err != nil || len(drained) != 1 {
			t.Fatalf("drained %d: %v", len(drained), err)
		}
		n, err := deliveries.Resend(ctx, tx, "codex", "", mine[0].ID, "", time.Minute)
		if err != nil || n != 1 {
			t.Fatalf("resent %d, want 1 (the one stream-a wrote after): %v", n, err)
		}
		if g, _ := deliveries.Get(ctx, tx, mine[1].ID); g.State != "pending" {
			t.Errorf("stream-a's later delivery is %s", g.State)
		}
		if g, _ := deliveries.Get(ctx, tx, drained[0].ID); g.State != "delivered" {
			t.Errorf("the drained delivery is %s", g.State)
		}
		if g, _ := deliveries.Get(ctx, tx, theirs[0].ID); g.State != "delivered" {
			t.Errorf("another connection's delivery is %s", g.State)
		}
		return nil
	})
}

// NextAvailable is how long until a paced delivery comes due by the
// database's clock, put on this instance's.
func TestNextAvailableIsTheDatabasesWait(t *testing.T) {
	d, tenant := setup(t, 1)
	in(t, d, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET available_at = clock_timestamp() + interval '2 seconds'`); err != nil {
			return err
		}
		next, err := deliveries.NextAvailable(ctx, tx, "codex", "")
		if in := time.Until(next); err != nil || in < 1500*time.Millisecond || in > 2*time.Second {
			t.Fatalf("due in %s: %v", in, err)
		}
		return nil
	})
}

// Prune deletes finished deliveries past the retention window, and events
// past it that no delivery names, even one no subscription has read yet:
// past retention it's never delivered.
func TestPrune(t *testing.T) {
	d, tenant := setup(t, 3)
	in(t, d, tenant, func(tx pgx.Tx) error {
		got, err := deliveries.Claim(ctx, tx, "codex", "", "stream-a", time.Minute, 3)
		if err != nil || len(got) != 3 {
			t.Fatalf("claimed %d: %v", len(got), err)
		}
		// One finished long ago, one recently, one still in flight.
		deliveries.Delivered(ctx, tx, "stream-a", []string{got[0].ID, got[1].ID})
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET finished_at = now() - interval '40 days' WHERE id = $1`, got[0].ID); err != nil {
			return err
		}
		// Every event is old; the subscription has read past the first two.
		if _, err := tx.Exec(ctx, `UPDATE events SET received_at = now() - interval '40 days'`); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE subscription_cursors SET last_seq = $1`, got[1].Event.Seq)
		return err
	})
	// An event no delivery names, past every cursor: unread, but past
	// retention, so it goes too.
	in(t, d, tenant, func(tx pgx.Tx) error {
		_, _, err := events.Append(ctx, tx, events.Event{Type: "test.ping", Source: "test", From: events.Party{Kind: "hub", ID: "hub"}, Trust: events.Internal})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE events SET received_at = now() - interval '40 days'`)
		return err
	})
	in(t, d, tenant, func(tx pgx.Tx) error {
		nd, ne, err := deliveries.Prune(ctx, tx, deliveries.Retention, 100)
		if err != nil || nd != 1 || ne != 2 {
			t.Fatalf("pruned %d deliveries and %d events: %v", nd, ne, err)
		}
		var left, evs int
		tx.QueryRow(ctx, `SELECT count(*) FROM deliveries`).Scan(&left)
		tx.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&evs)
		if left != 2 || evs != 2 {
			t.Fatalf("%d deliveries and %d events left", left, evs)
		}
		return nil
	})
}

// A message nobody takes doesn't wait forever: deliveries still owed for an
// event past retention go with the event (pending, or in flight with a
// lapsed lease), while one leased right now finishes first, and a recent
// event's are kept.
func TestPruneUndelivered(t *testing.T) {
	d, tenant := setup(t, 4)
	var ids []string
	in(t, d, tenant, func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT id::text FROM deliveries ORDER BY event_seq`)
		var err error
		if ids, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || len(ids) != 4 {
			t.Fatalf("deliveries %v: %v", ids, err)
		}
		// 0: pending; 1: in flight, lease lapsed; 2: in flight, leased now;
		// 3: pending, but its event is recent. Every other event is old.
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'in_flight', lease_holder = 's', lease_until = now() - interval '1 minute' WHERE id = $1`, ids[1]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'in_flight', lease_holder = 's', lease_until = now() + interval '1 minute' WHERE id = $1`, ids[2]); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE events SET received_at = now() - interval '31 days'
			WHERE id IN (SELECT event_id FROM deliveries WHERE id <> $1)`, ids[3])
		return err
	})
	in(t, d, tenant, func(tx pgx.Tx) error {
		nd, ne, err := deliveries.Prune(ctx, tx, deliveries.Retention, 100)
		if err != nil || nd != 2 || ne != 2 {
			t.Fatalf("pruned %d deliveries and %d events: %v", nd, ne, err)
		}
		rows, _ := tx.Query(ctx, `SELECT id::text FROM deliveries ORDER BY event_seq`)
		left, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || len(left) != 2 || left[0] != ids[2] || left[1] != ids[3] {
			t.Fatalf("left %v, want %v: %v", left, ids[2:], err)
		}
		var evs int
		tx.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&evs)
		if evs != 2 {
			t.Fatalf("%d events left", evs)
		}
		return nil
	})
}
