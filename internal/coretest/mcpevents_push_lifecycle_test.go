package coretest_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
)

// More of a push stream's lifecycle: what ends it with close (the
// client reconnects with its cursor), gaps, the replay window, and
// concurrent voucher redemptions.

// A deploy's drain and the age limit end a stream with close.
func TestPushDrainAndMaxAge(t *testing.T) {
	c := coretest.StartWith(t, func(o *core.Options) { o.StreamMaxAge = 500 * time.Millisecond })
	app := eventsApp(t, c, "chat")
	s := c.OpenPush(t, app, pushBody("u1", nil, nil))
	mustActive(t, s)
	if f := s.NextOf("close", 3*time.Second); f == nil || f["type"] != "close" {
		t.Fatalf("at the age limit: %+v", f)
	}

	d := coretest.Start(t)
	dapp := eventsApp(t, d, "chat")
	ds := d.OpenPush(t, dapp, pushBody("u1", nil, nil))
	mustActive(t, ds)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go d.Streams.Drain(ctx)
	if f := ds.NextOf("close", 3*time.Second); f == nil || f["type"] != "close" {
		t.Fatalf("on drain: %+v", f)
	}
	if s := d.OpenPush(t, dapp, pushBody("u1", nil, nil)); s.Status != 503 || s.Header.Get("Retry-After") == "" {
		t.Fatalf("while draining: %d", s.Status)
	}
}

// A suspended app, or a revoked key, ends its streams with close at the
// next renewal; a reconnect meets the refusal at open.
func TestPushAppSuspendedOrKeyRevoked(t *testing.T) {
	c := coretest.StartWith(t, func(o *core.Options) { o.StreamRenew = 200 * time.Millisecond })
	app := eventsApp(t, c, "chat")
	s := c.OpenPush(t, app, pushBody("u1", nil, nil))
	mustActive(t, s)
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE apps SET suspended_at = now() WHERE id = 'chat'`); err != nil {
		t.Fatal(err)
	}
	if f := s.NextOf("close", 3*time.Second); f == nil || f["type"] != "close" {
		t.Fatalf("suspended: %+v", f)
	}
	if r := c.OpenPush(t, app, pushBody("u1", nil, nil)); r.Status != 403 {
		t.Fatalf("reconnect while suspended: %d", r.Status)
	}
	c.DB.Pool.Exec(ctx, `UPDATE apps SET suspended_at = NULL WHERE id = 'chat'`)

	k := c.OpenPush(t, app, pushBody("u1", nil, nil))
	mustActive(t, k)
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE app_keys SET revoked_at = now() WHERE app_id = 'chat'`); err != nil {
		t.Fatal(err)
	}
	if f := k.NextOf("close", 3*time.Second); f == nil || f["type"] != "close" {
		t.Fatalf("key revoked: %+v", f)
	}
}

// A delivery that can't be sent (over MaxBody) is a gap: a fresh active
// with truncated and the cursor past it, and delivery goes on.
func TestPushGapResendsActive(t *testing.T) {
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	s := c.OpenPush(t, app, pushBody("u1", nil, nil))
	cur := mustActive(t, s)["cursor"]
	s.Close()
	waitFor(t, "released", func() bool {
		var live bool
		c.DB.InTenant(ctx, userTenant(t, c, "chat", "u1"), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM stream_leases WHERE renewed_at IS NOT NULL)`).Scan(&live)
		})
		return !live
	})
	emitChat(t, c, app, "big", "after")
	tenant := userTenant(t, c, "chat", "u1")
	waitFor(t, "queued", func() bool { return deliveryStates(t, c, "chat", "u1")["pending"] == 2 })
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE events SET data = jsonb_build_object('text', repeat('x', 300000), 'from', 'ada') WHERE data->>'text' = 'zqbigzq'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r := c.OpenPush(t, app, pushBody("u1", cur, nil))
	if a := mustActive(t, r); a["truncated"] != false {
		t.Fatalf("first active: %+v", a)
	}
	gap := r.NextOf("active", pushWait)
	if gap == nil || gap["type"] != "active" || gap["truncated"] != true || gap["cursor"] == cur {
		t.Fatalf("the gap: %+v", gap)
	}
	if f := r.NextOf("event", pushWait); f == nil || eventText(f) != "zqafterzq" {
		t.Fatalf("after the gap: %+v", f)
	}
}

// What was delivered more than the replay window ago isn't replayed: the
// resume says truncated.
func TestPushReplayWindow(t *testing.T) {
	fastHeartbeats(t)
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	s := c.OpenPush(t, app, pushBody("u1", nil, nil))
	start := mustActive(t, s)["cursor"]
	emitChat(t, c, app, "a", "b")
	s.NextOf("event", pushWait)
	s.NextOf("event", pushWait)
	waitFor(t, "delivered", func() bool {
		s.Next(300 * time.Millisecond)
		return deliveryStates(t, c, "chat", "u1")["delivered"] == 2
	})
	s.Close()
	tenant := userTenant(t, c, "chat", "u1")
	waitFor(t, "released", func() bool {
		var live bool
		c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM stream_leases WHERE renewed_at IS NOT NULL)`).Scan(&live)
		})
		return !live
	})
	// Both delivered long enough ago that the lost connection's hand-back
	// doesn't take them, and a queued two hours ago: outside the window.
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'delivered', finished_at = now() - interval '10 minutes', lease_until = NULL`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE deliveries d SET created_at = now() - interval '2 hours' FROM events e
			WHERE e.tenant_id = d.tenant_id AND e.id = d.event_id AND e.data->>'text' = 'zqazq'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r := c.OpenPush(t, app, pushBody("u1", start, nil))
	a := mustActive(t, r)
	if a["truncated"] != true || a["cursor"] == start {
		t.Fatalf("active: %+v", a)
	}
	if f := r.NextOf("event", pushWait); f == nil || eventText(f) != "zqbzq" {
		t.Fatalf("replayed: %+v", f)
	}
}

// 50 redemptions of one voucher at once make one subscription.
func TestPushVoucherConcurrentRedemptions(t *testing.T) {
	c := coretest.Start(t)
	app := voucherApp(t, c, "chat")
	_, v := handOut(t, c, app, userCall("ada"), "chat.message", eng, nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := c.OpenPush(t, app, map[string]any{"call": userCall("ada"), "name": "chat.message", "arguments": eng, "voucher": v})
			mu.Lock()
			codes[s.Status]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if codes[200] != 1 || codes[403] != 49 {
		t.Fatalf("statuses: %v", codes)
	}
	var n int
	c.DB.InTenant(ctx, userTenant(t, c, "chat", "ada"), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM event_subscriptions WHERE destination = 'push'`).Scan(&n)
	})
	if n != 1 {
		t.Fatalf("subscriptions: %d", n)
	}
}

// A personal event's multi-recipient emit reaches a recipient's push
// subscription, as it reaches webhook and poll ones.
func TestPushMultiRecipientEmit(t *testing.T) {
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	s := c.OpenPush(t, app, map[string]any{"call": map[string]any{"user": "u1", "client": "opencode"}, "name": "note.mine", "arguments": map[string]any{"a": "x"}})
	mustActive(t, s)
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/emit", map[string]any{"name": "note.mine", "arguments": map[string]any{"a": "x"},
		"payload": map[string]any{"n": 1}, "text": "for two", "to": []any{"u1", "u2"}}, nil); code != 200 {
		t.Fatalf("emit: %d %s", code, e)
	}
	if f := s.NextOf("event", pushWait); f == nil || f["type"] != "event" {
		t.Fatalf("the multi-recipient emit: %+v", f)
	}
}

// A waiting delivery whose content was deleted is a gap too (not finished
// silently): active with truncated, then what follows.
func TestPushGapRedacted(t *testing.T) {
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	s := c.OpenPush(t, app, pushBody("u1", nil, nil))
	cur := mustActive(t, s)["cursor"]
	s.Close()
	tenant := userTenant(t, c, "chat", "u1")
	waitFor(t, "released", func() bool {
		var live bool
		c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM stream_leases WHERE renewed_at IS NOT NULL)`).Scan(&live)
		})
		return !live
	})
	emitChat(t, c, app, "gone", "after")
	waitFor(t, "queued", func() bool { return deliveryStates(t, c, "chat", "u1")["pending"] == 2 })
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE events SET redacted_at = now() WHERE data->>'text' = 'zqgonezq'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r := c.OpenPush(t, app, pushBody("u1", cur, nil))
	mustActive(t, r)
	if gap := r.NextOf("active", pushWait); gap == nil || gap["type"] != "active" || gap["truncated"] != true {
		t.Fatalf("the gap: %+v", gap)
	}
	if f := r.NextOf("event", pushWait); f == nil || eventText(f) != "zqafterzq" {
		t.Fatalf("after the gap: %+v", f)
	}
}
