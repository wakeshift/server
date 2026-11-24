package coretest_test

import (
	"encoding/json"
	"maps"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/coretest"
)

// A bridged event subscription lasts 30 days without use: each delivery
// starts the 30 days again, and once they run out the subscription ends
// (its routing first, then the event subscription itself).
func TestEventSubscriptionLapses(t *testing.T) {
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	var out eventSubResult
	if code, _ := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": map[string]any{"user": "u1", "client": "claude-code"},
		"name": "chat.message", "arguments": json.RawMessage(`{"channel":"eng"}`)}, &out); code != 200 {
		t.Fatalf("subscribe: %d", code)
	}
	tenant := userTenant(t, c, "chat", "u1")
	var routing string
	expires := func() time.Duration {
		t.Helper()
		var at time.Time
		if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT subscription_id::text FROM event_subscriptions WHERE id = $1::uuid`, out.EventSubscription.ID).Scan(&routing); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT expires_at FROM topic_subscriptions WHERE subscription_id = $1::uuid`, routing).Scan(&at)
		}); err != nil {
			t.Fatal(err)
		}
		return time.Until(at)
	}
	const month = 30 * 24 * time.Hour
	if d := expires(); d < month-time.Hour || d > month {
		t.Fatalf("a new subscription expires in %v, not 30 days", d)
	}

	// Ten days in, a delivery starts the 30 days again.
	setExpiry := func(in string) {
		t.Helper()
		if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE topic_subscriptions SET expires_at = now() + $2::interval WHERE subscription_id = $1::uuid`, routing, in)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	setExpiry("20 days")
	if code, _ := appAPI(t, c, app, "/api/apps/v1/events/emit", map[string]any{"name": "chat.message", "arguments": map[string]any{"channel": "eng"},
		"payload": map[string]any{"text": "hi", "from": "bob"}, "text": "bob: hi"}, nil); code != 200 {
		t.Fatalf("emit: %d", code)
	}
	waitFor(t, "the delivery", func() bool { return deliveryCount(t, c, tenant) == 1 })
	if d := expires(); d < month-time.Hour {
		t.Fatalf("after a delivery it expires in %v, not 30 days", d)
	}

	// Unused past the 30 days: the expiry pass ends it, and it's no longer
	// listed or delivered to.
	setExpiry("-1 second")
	var n int
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		n, err = appapi.ExpireTopics(ctx, tx)
		return err
	}); err != nil || n != 1 {
		t.Fatalf("expired %d %v", n, err)
	}
	var list struct {
		Subscriptions []appapi.EventSubscription `json:"subscriptions"`
	}
	if code, _ := appAPI(t, c, app, "/api/apps/v1/event-subscriptions/list", map[string]any{"user": "u1"}, &list); code != 200 || len(list.Subscriptions) != 0 {
		t.Fatalf("listed after lapsing: %d %+v", code, list.Subscriptions)
	}
	var reason *string
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT ended_reason FROM event_subscriptions WHERE id = $1::uuid`, out.EventSubscription.ID).Scan(&reason)
	}); err != nil || reason == nil || *reason != "expired" {
		t.Fatalf("ended reason %v %v", reason, err)
	}
	appAPI(t, c, app, "/api/apps/v1/events/emit", map[string]any{"name": "chat.message", "arguments": map[string]any{"channel": "eng"},
		"payload": map[string]any{"text": "late", "from": "bob"}, "text": "bob: late"}, nil)
	time.Sleep(300 * time.Millisecond)
	// What's delivered after the 30 days is the notice that it ended, never
	// the late message.
	var late, notices int
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE e.subject LIKE '%late%'), count(*) FILTER (WHERE e.subject NOT LIKE '%bob:%')
			FROM deliveries d JOIN events e ON e.tenant_id = d.tenant_id AND e.id = d.event_id`).Scan(&late, &notices)
	}); err != nil {
		t.Fatal(err)
	}
	if late != 0 || notices != 1 {
		t.Fatalf("after lapsing: %d late messages delivered, %d notices (want 0, 1)", late, notices)
	}
}

// A webhook subscription's routing lapses with its webhook: what's
// published doesn't keep it alive (an MCP Events subscription never
// outlives its refresh_before without a refresh). Once the webhook is gone,
// what waits for its session can never be sent, so it's canceled when the
// routing ends, rather than waiting for good; and the expiry pass cancels
// what routing that ended earlier left waiting. A house agent that left its
// game table without unsubscribing from its hand results left a large
// backlog of deliveries waiting, because the table played on.
func TestWebhookRoutingLapsesWithItsWebhook(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	chatgpt := newFakeChatGPT(t)
	secret := chatgpt.secret()
	subscribe := func(channel string) {
		t.Helper()
		if code, e := appAPI(t, c, app, "/api/apps/v1/events/subscribe", map[string]any{"call": map[string]any{"user": "u1", "client": "openai-mcp"},
			"name": "chat.message", "arguments": map[string]any{"channel": channel},
			"delivery": map[string]any{"mode": "webhook", "url": chatgpt.url(), "secret": secret}, "ttl_ms": 3600000}, nil); code != 200 {
			t.Fatalf("subscribe %s: %d %s", channel, code, e)
		}
	}
	emit := func(channel, text string) {
		t.Helper()
		if code, _ := appAPI(t, c, app, "/api/apps/v1/events/emit", map[string]any{"name": "chat.message", "arguments": map[string]any{"channel": channel},
			"payload": map[string]any{"text": text, "from": "bob"}, "text": text}, nil); code != 200 {
			t.Fatalf("emit: %d", code)
		}
	}
	subscribe("eng")
	tenant := userTenant(t, c, "chat", "u1")
	exec := func(sql string, args ...any) {
		t.Helper()
		if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, args...)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	routingOf := func(channel string) string {
		t.Helper()
		var id string
		if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT subscription_id::text FROM event_subscriptions WHERE destination = 'webhook' AND arguments LIKE '%' || $1 || '%'`, channel).Scan(&id)
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	eng := routingOf("eng")
	states := func(routing string) map[string]int {
		t.Helper()
		out := map[string]int{}
		if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			rows, _ := tx.Query(ctx, `SELECT state, count(*) FROM deliveries WHERE subscription_id = $1::uuid GROUP BY state`, routing)
			for rows.Next() {
				var s string
				var n int
				if err := rows.Scan(&s, &n); err != nil {
					return err
				}
				out[s] = n
			}
			return rows.Err()
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Delivered, and its routing still ends when the webhook would.
	exec(`UPDATE topic_subscriptions SET expires_at = now() + interval '10 minutes' WHERE subscription_id = $1::uuid`, eng)
	emit("eng", "hi")
	waitFor(t, "the event at the callback", func() bool { return len(chatgpt.got()) == 1 })
	var left time.Duration
	c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var at time.Time
		err := tx.QueryRow(ctx, `SELECT expires_at FROM topic_subscriptions WHERE subscription_id = $1::uuid`, eng).Scan(&at)
		left = time.Until(at)
		return err
	})
	if left > 11*time.Minute {
		t.Fatalf("a delivery moved a webhook's routing on: it expires in %v", left)
	}

	// The webhook lapses unrefreshed (its client went away): what's
	// published now waits, and when the routing lapses too it's canceled,
	// with no notice queued for a session nobody reads.
	exec(`UPDATE mcp_event_webhooks SET expires_at = now() - interval '1 second'`)
	emit("eng", "one")
	emit("eng", "two")
	waitFor(t, "two waiting", func() bool { return states(eng)["pending"] == 2 })
	exec(`UPDATE topic_subscriptions SET expires_at = now() - interval '1 second' WHERE subscription_id = $1::uuid`, eng)
	var n int
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		n, err = appapi.ExpireTopics(ctx, tx)
		return err
	}); err != nil || n != 1 {
		t.Fatalf("expired %d %v", n, err)
	}
	if got := states(eng); got["pending"] != 0 || got["cancelled"] != 2 || got["acked"] != 1 || len(got) != 2 {
		t.Fatalf("after the routing lapsed: %v", got)
	}

	// Routing that ended before this (left waiting, as it used to be): the
	// expiry pass cancels what waits once its webhook is gone.
	subscribe("ops")
	ops := routingOf("ops")
	exec(`UPDATE mcp_event_webhooks SET expires_at = now() - interval '1 second'`)
	emit("ops", "three")
	waitFor(t, "one waiting", func() bool { return states(ops)["pending"] == 1 })
	// As the release before migration 116 still writes it while both serve.
	exec(`UPDATE subscriptions SET status = 'cancelled', cancelled_at = now() WHERE id = $1::uuid`, ops)
	exec(`UPDATE topic_subscriptions SET ended_at = now(), ended_reason = 'ttl' WHERE subscription_id = $1::uuid`, ops)
	var cancelled int64
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		cancelled, err = appapi.CancelStranded(ctx, tx, 100, false)
		return err
	}); err != nil || cancelled != 1 {
		t.Fatalf("cancelled %d stranded %v", cancelled, err)
	}
	if got := states(ops); got["cancelled"] != 1 || len(got) != 1 {
		t.Fatalf("stranded: %v", got)
	}

	// Routing that ended days ago, its webhook gone since: every pass's
	// check (recently ended subscriptions only) leaves it; the daily check
	// of every ended subscription cancels it.
	subscribe("dev")
	dev := routingOf("dev")
	exec(`UPDATE mcp_event_webhooks SET expires_at = now() - interval '1 second'`)
	emit("dev", "four")
	waitFor(t, "one waiting", func() bool { return states(dev)["pending"] == 1 })
	exec(`UPDATE subscriptions SET status = 'canceled', cancelled_at = now() - interval '3 days' WHERE id = $1::uuid`, dev)
	exec(`UPDATE topic_subscriptions SET ended_at = now(), ended_reason = 'ttl' WHERE subscription_id = $1::uuid`, dev)
	for _, all := range []bool{false, true} {
		if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
			cancelled, err = appapi.CancelStranded(ctx, tx, 100, all)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		want := map[string]int{"pending": 1}
		if all {
			want = map[string]int{"cancelled": 1}
		}
		if got := states(dev); !maps.Equal(got, want) || cancelled != int64(want["cancelled"]) {
			t.Fatalf("all %v: cancelled %d, states %v", all, cancelled, got)
		}
	}
}
