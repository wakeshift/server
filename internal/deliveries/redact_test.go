package deliveries_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/subscriptions"
)

// redactSetup appends one event with content (flagged to be deleted on
// delivery, or not), queues it for each of agents, and moves every cursor
// past it, as the pipeline would have.
func redactSetup(t *testing.T, flagged bool, agentIDs ...string) (*db.DB, string, events.Event) {
	t.Helper()
	d := dbtest.New(t)
	tenant := dbtest.NewTenant(t, d)
	var e events.Event
	in(t, d, tenant, func(tx pgx.Tx) error {
		var subs []string
		for _, a := range agentIDs {
			agents.Create(ctx, tx, a, "")
			sub, err := subscriptions.Create(ctx, tx, subscriptions.New{AgentID: a, EventType: "test.*", CreatedBy: "test"})
			if err != nil {
				return err
			}
			subs = append(subs, sub.ID)
		}
		var err error
		e, _, err = events.Append(ctx, tx, events.Event{Type: "test.message", Source: "test", From: events.Party{Kind: "app", ID: "chat"},
			Subject: "ada: the launch code is 1234", Body: "a body", Data: []byte(`{"text":"the launch code is 1234"}`),
			Trust: events.External, SourceEventID: "msg-1", DeleteOnDelivery: flagged})
		if err != nil {
			return err
		}
		for i, a := range agentIDs {
			if _, err := deliveries.Insert(ctx, tx, deliveries.New{AgentID: a, SubscriptionID: subs[i], Event: e}); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE subscription_cursors SET last_seq = $1`, e.Seq)
		return err
	})
	return d, tenant, e
}

// content is what's left of an event's content, in every column that held
// it, and whether it's marked redacted.
func content(t *testing.T, d *db.DB, tenant, id string) (subject, body string, data *string, redacted bool) {
	t.Helper()
	in(t, d, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT subject, body, data::text, redacted_at IS NOT NULL FROM events WHERE id = $1`, id).Scan(&subject, &body, &data, &redacted)
	})
	return
}

func requireRedacted(t *testing.T, d *db.DB, tenant string, e events.Event) {
	t.Helper()
	subject, body, data, redacted := content(t, d, tenant, e.ID)
	if subject != "" || body != "" || data != nil || !redacted {
		t.Fatalf("content left after delivery: subject %q body %q data %v redacted %v", subject, body, data, redacted)
	}
	// What's kept: the event's identity (dedupe, cursors) and its deliveries.
	in(t, d, tenant, func(tx pgx.Tx) error {
		dup, inserted, err := events.Append(ctx, tx, events.Event{Type: "test.message", Source: "test", From: events.Party{Kind: "app", ID: "chat"},
			Subject: "again", Trust: events.External, SourceEventID: "msg-1", DeleteOnDelivery: true})
		if err != nil || inserted || dup.ID != e.ID || !dup.Redacted || dup.Subject != "" {
			t.Fatalf("a repeat of the event: %+v inserted %v %v", dup, inserted, err)
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE event_id = $1`, e.ID).Scan(&n); err != nil || n == 0 {
			t.Fatalf("deliveries: %d %v", n, err)
		}
		return nil
	})
}

func requireKept(t *testing.T, d *db.DB, tenant string, e events.Event) {
	t.Helper()
	if subject, _, data, redacted := content(t, d, tenant, e.ID); subject != e.Subject || data == nil || redacted {
		t.Fatalf("content went: subject %q data %v redacted %v", subject, data, redacted)
	}
}

// A flagged event's content goes when its last delivery is acknowledged
// (a stream's ack, a webhook's 2xx), not before; after that it's never
// queued again.
func TestRedactOnceEveryDeliveryIsAcked(t *testing.T) {
	d, tenant, e := redactSetup(t, true, "codex", "claude")
	var ds []deliveries.Delivery
	in(t, d, tenant, func(tx pgx.Tx) (err error) {
		ds, err = deliveries.Claim(ctx, tx, "codex", "", "stream-a", time.Minute, 10)
		return err
	})
	if len(ds) != 1 || ds[0].Event.Subject != e.Subject {
		t.Fatalf("claimed %+v", ds)
	}
	in(t, d, tenant, func(tx pgx.Tx) error {
		ok, err := deliveries.Ack(ctx, tx, "codex", "", ds[0].ID)
		if !ok {
			t.Error("not acked")
		}
		return err
	})
	requireKept(t, d, tenant, e) // claude's is still owed
	in(t, d, tenant, func(tx pgx.Tx) error {
		_, err := deliveries.RedactDelivered(ctx, tx, 100)
		return err
	})
	requireKept(t, d, tenant, e)

	in(t, d, tenant, func(tx pgx.Tx) (err error) {
		ds, err = deliveries.Claim(ctx, tx, "claude", "", "wait-b", time.Minute, 10)
		return err
	})
	in(t, d, tenant, func(tx pgx.Tx) error { return deliveries.AckHolder(ctx, tx, "claude", "", "wait-b") })
	requireRedacted(t, d, tenant, e)

	// Never queued again, and a repeated ack is still fine.
	in(t, d, tenant, func(tx pgx.Tx) error {
		got, err := events.Get(ctx, tx, e.ID)
		if err != nil {
			return err
		}
		if ok, err := deliveries.Insert(ctx, tx, deliveries.New{AgentID: "claude", Target: "other", SubscriptionID: ds[0].SubscriptionID, Event: got}); ok || err != nil {
			t.Fatalf("a redacted event queued: %v %v", ok, err)
		}
		ok, err := deliveries.Ack(ctx, tx, "codex", "", deliveries.ID("codex", e.ID, ""))
		if !ok {
			t.Error("a repeated ack failed")
		}
		return err
	})
}

// An event that doesn't ask keeps its content after delivery.
func TestUnflaggedKeptAfterDelivery(t *testing.T) {
	d, tenant, e := redactSetup(t, false, "codex")
	in(t, d, tenant, func(tx pgx.Tx) error {
		ds, err := deliveries.Claim(ctx, tx, "codex", "", "stream-a", time.Minute, 10)
		if err != nil {
			return err
		}
		if _, err := deliveries.Ack(ctx, tx, "codex", "", ds[0].ID); err != nil {
			return err
		}
		_, err = deliveries.RedactDelivered(ctx, tx, 100)
		return err
	})
	requireKept(t, d, tenant, e)
}

// A flagged event that wasn't delivered keeps its content (to normal
// expiry): pending, in flight, dead or canceled, or not yet read by every
// subscription.
func TestUndeliveredKeepsContent(t *testing.T) {
	d, tenant, e := redactSetup(t, true, "codex")
	redactAll := func() {
		in(t, d, tenant, func(tx pgx.Tx) error {
			_, err := deliveries.RedactDelivered(ctx, tx, 100)
			return err
		})
	}
	redactAll()
	requireKept(t, d, tenant, e) // pending
	in(t, d, tenant, func(tx pgx.Tx) error {
		_, err := deliveries.Claim(ctx, tx, "codex", "", "stream-a", time.Minute, 10)
		return err
	})
	redactAll()
	requireKept(t, d, tenant, e) // in flight
	in(t, d, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'dead', finished_at = now(), lease_until = NULL`)
		return err
	})
	redactAll()
	requireKept(t, d, tenant, e) // dead: it never got there

	// Delivered, but a subscription hasn't read past it yet: the pipeline
	// may still owe it to someone.
	in(t, d, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'acked'`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE subscription_cursors SET last_seq = $1`, e.Seq-1)
		return err
	})
	redactAll()
	requireKept(t, d, tenant, e)
	// A subscription to other types, however far behind, doesn't hold it.
	in(t, d, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE subscription_cursors SET last_seq = $1`, e.Seq); err != nil {
			return err
		}
		other, err := subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "codex", EventType: "other.*", CreatedBy: "test"})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE subscription_cursors SET last_seq = 0 WHERE subscription_id = $1`, other.ID)
		return err
	})
	redactAll()
	requireRedacted(t, d, tenant, e)
}

// Written to a consumer that can't acknowledge, a flagged event keeps its
// content while it may be resent (RedeliverWindow), then the expiry pass
// takes it; a resend after that sends nothing (never an empty wake-up).
func TestDeliveredWithoutAckWaitsForTheResendWindow(t *testing.T) {
	d, tenant, e := redactSetup(t, true, "codex")
	var id string
	in(t, d, tenant, func(tx pgx.Tx) error {
		ds, err := deliveries.Claim(ctx, tx, "codex", "", "sse-a", time.Minute, 10)
		if err != nil {
			return err
		}
		id = ds[0].ID
		if err := deliveries.Delivered(ctx, tx, "sse-a", []string{id}); err != nil {
			return err
		}
		_, err = deliveries.RedactDelivered(ctx, tx, 100)
		return err
	})
	requireKept(t, d, tenant, e)
	in(t, d, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET finished_at = now() - $1::interval`, (deliveries.RedeliverWindow + time.Minute).String()); err != nil {
			return err
		}
		n, err := deliveries.RedactDelivered(ctx, tx, 100)
		if n != 1 {
			t.Errorf("redacted %d", n)
		}
		return err
	})
	requireRedacted(t, d, tenant, e)
	// Asked for again (a lost connection), even within a window: it doesn't
	// go back to pending.
	in(t, d, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET finished_at = now()`); err != nil {
			return err
		}
		if n, err := deliveries.ResendLost(ctx, tx, "codex", "sse-a", time.Minute); n != 0 || err != nil {
			t.Fatalf("ResendLost: %d %v", n, err)
		}
		return nil
	})
}

// A reconnecting client's Last-Event-ID gets again what was written after
// it, but not an event whose content went.
func TestResendSkipsRedacted(t *testing.T) {
	d := dbtest.New(t)
	tenant := dbtest.NewTenant(t, d)
	var ids []string
	in(t, d, tenant, func(tx pgx.Tx) error {
		agents.Create(ctx, tx, "codex", "")
		sub, err := subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "codex", EventType: "test.*", CreatedBy: "test"})
		if err != nil {
			return err
		}
		for _, flagged := range []bool{false, true, false} {
			e, _, err := events.Append(ctx, tx, events.Event{Type: "test.message", Source: "test", From: events.Party{Kind: "app", ID: "chat"},
				Subject: "text", Trust: events.External, DeleteOnDelivery: flagged})
			if err != nil {
				return err
			}
			if _, err := deliveries.Insert(ctx, tx, deliveries.New{AgentID: "codex", SubscriptionID: sub.ID, Event: e}); err != nil {
				return err
			}
			ids = append(ids, deliveries.ID("codex", e.ID, ""))
		}
		if _, err := tx.Exec(ctx, `UPDATE subscription_cursors SET last_seq = (SELECT max(seq) FROM events)`); err != nil {
			return err
		}
		if _, err := deliveries.Claim(ctx, tx, "codex", "", "sse-a", time.Minute, 10); err != nil {
			return err
		}
		if err := deliveries.Delivered(ctx, tx, "sse-a", ids); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET finished_at = now() - $1::interval`, (deliveries.RedeliverWindow + time.Minute).String()); err != nil {
			return err
		}
		if n, err := deliveries.RedactDelivered(ctx, tx, 100); n != 1 || err != nil {
			t.Fatalf("redacted %d %v", n, err)
		}
		_, err = tx.Exec(ctx, `UPDATE deliveries SET finished_at = now()`)
		return err
	})
	in(t, d, tenant, func(tx pgx.Tx) error {
		n, err := deliveries.Resend(ctx, tx, "codex", "", ids[0], "", time.Minute)
		if n != 1 {
			t.Fatalf("resent %d, want only the third", n)
		}
		var state string
		if err := tx.QueryRow(ctx, `SELECT state FROM deliveries WHERE id = $1`, ids[1]).Scan(&state); err != nil || state != "delivered" {
			t.Fatalf("the redacted one: %s %v", state, err)
		}
		return err
	})
}

// Handed over in a tool result (Drain), a flagged event's content goes at
// once.
func TestDrainRedacts(t *testing.T) {
	d := dbtest.New(t)
	tenant := dbtest.NewTenant(t, d)
	var e events.Event
	in(t, d, tenant, func(tx pgx.Tx) error {
		agents.Create(ctx, tx, "claude", "")
		sub, err := subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "claude", EventType: "test.*", CreatedBy: "test"})
		if err != nil {
			return err
		}
		if e, _, err = events.Append(ctx, tx, events.Event{Type: "test.message", Source: "test", From: events.Party{Kind: "app", ID: "chat"},
			Subject: "secret", Data: []byte(`{"a":1}`), Trust: events.External, DeleteOnDelivery: true}); err != nil {
			return err
		}
		if _, err := deliveries.Insert(ctx, tx, deliveries.New{AgentID: "claude", Target: "ses-1", SubscriptionID: sub.ID, Event: e}); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE subscription_cursors SET last_seq = $1`, e.Seq)
		return err
	})
	in(t, d, tenant, func(tx pgx.Tx) error {
		list, err := deliveries.Drain(ctx, tx, "claude", "ses-1", 10)
		if len(list) != 1 || list[0].Event.Subject != "secret" {
			t.Fatalf("drained %+v", list)
		}
		return err
	})
	if subject, _, data, redacted := content(t, d, tenant, e.ID); subject != "" || data != nil || !redacted {
		t.Fatalf("after Drain: %q %v %v", subject, data, redacted)
	}
}

// A delivery of an event whose content went meanwhile (only a race with a
// resend gets one queued) is finished when claimed or drained, never sent
// empty.
func TestRedactedNeverClaimed(t *testing.T) {
	d, tenant, e := redactSetup(t, true, "codex")
	in(t, d, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE events SET subject = '', body = '', data = NULL, redacted_at = now() WHERE id = $1`, e.ID)
		return err
	})
	in(t, d, tenant, func(tx pgx.Tx) error {
		ds, err := deliveries.Claim(ctx, tx, "codex", "", "stream-a", time.Minute, 10)
		if len(ds) != 0 {
			t.Fatalf("claimed a redacted event: %+v", ds)
		}
		if err != nil {
			return err
		}
		var state, why string
		if err := tx.QueryRow(ctx, `SELECT state, coalesce(last_error, '') FROM deliveries`).Scan(&state, &why); err != nil {
			return err
		}
		if state != "delivered" || why != "content deleted after delivery" {
			t.Fatalf("left %s %q", state, why)
		}
		if ok, err := deliveries.Pending(ctx, tx, "codex", ""); ok || err != nil {
			t.Fatalf("still pending: %v %v", ok, err)
		}
		return nil
	})
}
