package coretest_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/mcpevents"
	"github.com/wakeshift/server/internal/mcpevents/event"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// The current SEP-3415 draft (event.Revision), as a client other than
// ChatGPT speaks it.

const draftUA = "acme-agent/2"

// draftParams are subscribeParams from a client that speaks the draft.
func draftParams(channel, url, secret string) map[string]any {
	p := subscribeParams(channel, url, secret)
	p["_meta"] = map[string]any{"io.modelcontextprotocol/clientInfo": map[string]any{"name": "acme-agent", "version": "2"}}
	return p
}

func draft(t *testing.T, endpoint, user, method string, params any) rpcResponse {
	t.Helper()
	return rpcAs(t, endpoint, user, draftUA, nil, method, params)
}

func unsubscribeParams(channel, url string) map[string]any {
	return map[string]any{"name": "chat.message", "arguments": map[string]any{"channel": channel},
		"delivery": map[string]any{"mode": "webhook", "url": url},
		"_meta":    map[string]any{"io.modelcontextprotocol/clientInfo": map[string]any{"name": "acme-agent"}}}
}

// tenantOf is the workspace an app keeps for one of its users.
func tenantOf(t *testing.T, c *coretest.Core, app, user string) string {
	t.Helper()
	var tenant string
	if err := c.DB.Pool.QueryRow(ctx, `SELECT id::text FROM tenants WHERE app_id = $1 AND external_id = $2`, app, user).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	return tenant
}

// eventsConversation subscribes a callback to chat.message {channel:
// eng} for u1, through the app's endpoint (as a draft client unless
// chatgpt is set). The last result is the channel.
func eventsConversation(t *testing.T, chatgpt bool) (*coretest.Core, *wakeshift.Client, string, *fakeChatGPT, string) {
	t.Helper()
	eventsTestSetup(t)
	c := coretest.Start(t)
	app, endpoint := chatEndpoint(t, c, "chat")
	cb := newFakeChatGPT(t)
	var res rpcResponse
	if chatgpt {
		res = rpc(t, endpoint, "u1", "events/subscribe", subscribeParams("eng", cb.url(), cb.secret()))
	} else {
		res = draft(t, endpoint, "u1", "events/subscribe", draftParams("eng", cb.url(), cb.secret()))
	}
	if res.Error != nil {
		t.Fatalf("subscribe: %+v", res.Error)
	}
	return c, app, endpoint, cb, "eng"
}

// texts are which of want each event's text contains, in order.
func texts(evs []webhookEvent, want ...string) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, which(e.Body.Data["text"], want))
	}
	return out
}

// which is the token (zq…, so it appears nowhere else in a framed text)
// that text carries.
func which(text string, want []string) string {
	for _, w := range want {
		if strings.Contains(text, "zq"+w+"zq") {
			return w
		}
	}
	return "?"
}

// Discovery and the subscribe result: both capability keys, resultType on
// every result, the grant capped at a day (never no expiry, clamped up to
// the floor), and deliveryStatus on a refresh.
func TestMCPEventsDraftProtocol(t *testing.T) {
	c, app, endpoint, cb, channel := eventsConversation(t, false)
	res := draft(t, endpoint, "u1", "server/discover", map[string]any{})
	caps, _ := res.Result["capabilities"].(map[string]any)
	ext, _ := caps["extensions"].(map[string]any)
	if fmt.Sprint(caps[event.TopLevelKey]) != "map[listChanged:false]" || fmt.Sprint(ext[event.ExtensionKey]) != "map[listChanged:false]" || caps["tools"] == nil {
		t.Fatalf("capabilities: %v", caps)
	}
	if res := draft(t, endpoint, "u1", "events/list", map[string]any{}); res.Result["resultType"] != "complete" {
		t.Fatalf("events/list: %+v", res.Result)
	}

	secret := cb.secret()
	for _, c := range []struct {
		ttl      any
		min, max time.Duration
	}{
		{int64(7 * 24 * time.Hour / time.Millisecond), 23 * time.Hour, 24*time.Hour + time.Minute}, // a week: a day
		{nil, 23 * time.Hour, 24*time.Hour + time.Minute},                                          // ttlMs: null: still a day, never no expiry
		{int64(1000), 4 * time.Minute, 6 * time.Minute},                                            // a second: the 5-minute floor
	} {
		p := draftParams(channel, cb.url(), secret)
		p["ttlMs"] = c.ttl
		res := draft(t, endpoint, "u1", "events/subscribe", p)
		refresh, err := time.Parse(time.RFC3339, fmt.Sprint(res.Result["refreshBefore"]))
		if res.Error != nil || err != nil || time.Until(refresh) < c.min || time.Until(refresh) > c.max {
			t.Errorf("ttlMs %v: %+v %+v", c.ttl, res.Error, res.Result)
		}
	}

	publish(t, c, app, "first")
	waitFor(t, "the event", func() bool { return len(cb.got()) == 1 })
	e := cb.got()[0]
	if e.Body.Cursor == nil || !strings.HasPrefix(*e.Body.Cursor, "c1.") {
		t.Fatalf("occurrence cursor: %+v", e.Body)
	}
	waitFor(t, "lastDeliveryAt", func() bool {
		res := draft(t, endpoint, "u1", "events/subscribe", draftParams(channel, cb.url(), secret))
		st, _ := res.Result["deliveryStatus"].(map[string]any)
		return st["active"] == true && st["lastDeliveryAt"] != nil && st["lastError"] == nil
	})
	// The refresh's cursor is the watermark: at or past the event's.
	res = draft(t, endpoint, "u1", "events/subscribe", draftParams(channel, cb.url(), secret))
	got, _ := mcpevents.ParseCursor(fmt.Sprint(res.Result["cursor"]))
	at, _ := mcpevents.ParseCursor(*e.Body.Cursor)
	if got < at || res.Result["resultType"] != "complete" {
		t.Fatalf("refresh: %+v, event at %d", res.Result, at)
	}
	// Unsubscribing returns the empty result, with resultType.
	if res := draft(t, endpoint, "u1", "events/unsubscribe", unsubscribeParams(channel, cb.url())); res.Error != nil || res.Result["resultType"] != "complete" {
		t.Fatalf("unsubscribe: %+v %+v", res.Error, res.Result)
	}
}

// Mcp-Name, when sent, must match params.name (plain or SEP-2243's Base64
// sentinel): a mismatch is HTTP 400 and HeaderMismatch.
func TestMCPEventsMcpName(t *testing.T) {
	_, _, endpoint, cb, channel := eventsConversation(t, false)
	p := draftParams(channel, cb.url(), cb.secret())
	res, status := rpcStatus(t, endpoint, "u1", draftUA, map[string]string{"Mcp-Name": "other.event"}, "events/subscribe", p)
	if status != http.StatusBadRequest || res.Error == nil || res.Error.Code != event.HeaderMismatch {
		t.Fatalf("mismatch: %d %+v", status, res.Error)
	}
	if res, status := rpcStatus(t, endpoint, "u1", draftUA, map[string]string{"Mcp-Name": "=?base64?***?="}, "events/subscribe", p); status != 400 || res.Error.Code != -32020 {
		t.Fatalf("a bad sentinel: %d %+v", status, res.Error)
	}
	for _, h := range []string{"chat.message", "=?base64?Y2hhdC5tZXNzYWdl?="} {
		if res, status := rpcStatus(t, endpoint, "u1", draftUA, map[string]string{"Mcp-Name": h}, "events/subscribe", p); status != 200 || res.Error != nil {
			t.Fatalf("%q: %d %+v", h, status, res.Error)
		}
	}
}

// Strict order: nothing after a delivery that's being retried goes until
// it's acknowledged; then the rest follow, in order.
func TestMCPEventsStrictOrder(t *testing.T) {
	c, app, _, cb, _ := eventsConversation(t, false)
	cb.status = func(n int) int {
		if n <= 2 {
			return 503 // the first event fails twice
		}
		return 200
	}
	publish(t, c, app, "one", "two", "three")
	waitFor(t, "three events", func() bool { return len(cb.got()) == 3 })
	want := []string{"one", "two", "three"}
	if got := texts(cb.got(), want...); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("delivered: %v", got)
	}
	tried := cb.tried()
	order := make([]string, 0, len(tried))
	for _, x := range tried {
		order = append(order, which(x, want))
	}
	if fmt.Sprint(order) != "[one one one two three]" {
		t.Fatalf("attempts: %v", order)
	}
}

// A delivery abandoned after MaxAttempts is a gap: the draft client gets a
// gap envelope with a fresh cursor, its next refresh says truncated (once),
// and the next delivery goes on.
func TestMCPEventsGap(t *testing.T) {
	c, app, endpoint, cb, channel := eventsConversation(t, false)
	cb.status = func(n int) int {
		if n <= mcpevents.MaxAttempts {
			return 500
		}
		return 200
	}
	publish(t, c, app, "lost")
	waitFor(t, "the gap envelope", func() bool { return len(cb.envelopes()) == 1 })
	env := cb.envelopes()[0]
	if env.Type != "gap" || !strings.HasPrefix(env.Cursor, "c1.") || !strings.HasPrefix(env.ID, "msg_gap_") {
		t.Fatalf("gap: %+v", env)
	}
	if n := len(cb.tried()); n != mcpevents.MaxAttempts {
		t.Fatalf("%d attempts", n)
	}
	publish(t, c, app, "next")
	waitFor(t, "the next event", func() bool { return len(cb.got()) == 1 })
	p := draftParams(channel, cb.url(), cb.secret())
	if res := draft(t, endpoint, "u1", "events/subscribe", p); res.Result["truncated"] != true {
		t.Fatalf("refresh after a gap: %+v", res.Result)
	}
	if res := draft(t, endpoint, "u1", "events/subscribe", p); res.Result["truncated"] != false {
		t.Fatalf("the next refresh: %+v", res.Result)
	}
}

// The retry window ends retries too, before MaxAttempts.
func TestMCPEventsRetryWindow(t *testing.T) {
	c, app, _, cb, _ := eventsConversation(t, false)
	t.Cleanup(mcpevents.SetRetryWindow(time.Millisecond))
	cb.status = func(n int) int { return 503 }
	publish(t, c, app, "late")
	waitFor(t, "the gap envelope", func() bool { return len(cb.envelopes()) == 1 })
	if n := len(cb.tried()); n != 2 {
		t.Fatalf("%d attempts within a 1ms window (want 2: the first failure starts it)", n)
	}
}

// ChatGPT takes neither gap nor terminated envelopes: it gets none, and
// still learns of the gap from its next refresh.
func TestMCPEventsChatGPTGetsNoEnvelopes(t *testing.T) {
	c, app, endpoint, cb, channel := eventsConversation(t, true)
	cb.status = func(n int) int {
		if n <= mcpevents.MaxAttempts {
			return 500
		}
		return 200
	}
	publish(t, c, app, "lost", "next")
	waitFor(t, "the next event", func() bool { return len(cb.got()) == 1 })
	if res := rpc(t, endpoint, "u1", "events/subscribe", subscribeParams(channel, cb.url(), cb.secret())); res.Result["truncated"] != true {
		t.Fatalf("refresh: %+v", res.Result)
	}
	if env := cb.envelopes(); len(env) != 0 {
		t.Fatalf("envelopes to ChatGPT: %+v", env)
	}
	// Another client (a draft one) can't refresh ChatGPT's subscription:
	// it's ChatGPT's, under its own grant.
	if res := draft(t, endpoint, "u1", "events/subscribe", draftParams(channel, cb.url(), cb.secret())); res.Error == nil || res.Error.Code != -32024 {
		t.Fatalf("another client's refresh: %+v", res.Error)
	}
}

// A cursor on a live subscription changes nothing. A fresh subscription
// (after the old one ended) replays from it: what the old one delivered
// after the cursor (the client hadn't persisted it), what was published
// while there was none, then what's live. Not truncated: all of it was
// kept.
func TestMCPEventsReplay(t *testing.T) {
	c, app, endpoint, cb, channel := eventsConversation(t, false)
	publish(t, c, app, "e1", "e2", "e3")
	waitFor(t, "three events", func() bool { return len(cb.got()) == 3 })
	first := cb.got()
	waitFor(t, "acked", func() bool { return deliveryStates(t, c, "chat", "u1")["acked"] == 3 })
	after1 := *first[0].Body.Cursor

	p := draftParams(channel, cb.url(), cb.secret())
	p["cursor"] = after1
	if res := draft(t, endpoint, "u1", "events/subscribe", p); res.Error != nil || res.Result["truncated"] != false {
		t.Fatalf("live refresh with a cursor: %+v %+v", res.Error, res.Result)
	}
	time.Sleep(150 * time.Millisecond)
	if n := len(cb.got()); n != 3 {
		t.Fatalf("a live refresh replayed: %d events", n)
	}

	if res := draft(t, endpoint, "u1", "events/unsubscribe", unsubscribeParams(channel, cb.url())); res.Error != nil {
		t.Fatal(res.Error)
	}
	publish(t, c, app, "e4")
	if res := draft(t, endpoint, "u1", "events/subscribe", p); res.Error != nil || res.Result["truncated"] != false {
		t.Fatalf("fresh subscribe with a cursor: %+v %+v", res.Error, res.Result)
	}
	publish(t, c, app, "e5")
	waitFor(t, "e5", func() bool { return len(cb.got()) == 7 })
	time.Sleep(150 * time.Millisecond)
	if got := texts(cb.got(), "e1", "e2", "e3", "e4", "e5"); fmt.Sprint(got) != "[e1 e2 e3 e2 e3 e4 e5]" {
		t.Fatalf("delivered: %v", got)
	}
}

// lapsedSubscription delivers e1 and e2 to a draft client's webhook, ends
// it, and publishes e3 while there's none. It returns the cursor after e1.
func lapsedSubscription(t *testing.T) (*coretest.Core, *wakeshift.Client, string, *fakeChatGPT, string, string) {
	t.Helper()
	c, app, endpoint, cb, channel := eventsConversation(t, false)
	publish(t, c, app, "e1", "e2")
	waitFor(t, "two events", func() bool { return len(cb.got()) == 2 })
	waitFor(t, "acked", func() bool { return deliveryStates(t, c, "chat", "u1")["acked"] == 2 })
	if res := draft(t, endpoint, "u1", "events/unsubscribe", unsubscribeParams(channel, cb.url())); res.Error != nil {
		t.Fatal(res.Error)
	}
	publish(t, c, app, "e3")
	return c, app, endpoint, cb, channel, *cb.got()[0].Body.Cursor
}

// A fresh subscribe after a lapse replays from the client's cursor, not
// from before it: a publish the client never had because it was made
// before the client's subscription (here, 30 seconds before the one the
// cursor follows) isn't sent, while those after the cursor are.
func TestMCPEventsReplayStartsAtTheCursor(t *testing.T) {
	c, _, endpoint, cb, channel, after1 := lapsedSubscription(t)
	owner := dbtest.Owner(t, c.DB)
	tag, err := owner.Exec(context.Background(), `INSERT INTO topic_events (app_id, source_id, event_id, topics, text, data, published_at)
		SELECT app_id, source_id || '-early', gen_random_uuid(), topics, replace(text, 'zqe1zq', 'zqearlyzq'),
			replace(data::text, 'zqe1zq', 'zqearlyzq')::jsonb, published_at - interval '30 seconds'
		FROM topic_events WHERE app_id = 'chat' AND text LIKE '%zqe1zq%'`)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("an early publish: %v, %d rows", err, tag.RowsAffected())
	}
	p := draftParams(channel, cb.url(), cb.secret())
	p["cursor"] = after1
	if res := draft(t, endpoint, "u1", "events/subscribe", p); res.Error != nil || res.Result["truncated"] != false {
		t.Fatalf("fresh subscribe with a cursor: %+v %+v", res.Error, res.Result)
	}
	waitFor(t, "e2 and e3 again", func() bool { return len(cb.got()) == 4 })
	time.Sleep(150 * time.Millisecond)
	if got := texts(cb.got(), "e1", "e2", "e3", "early"); fmt.Sprint(got) != "[e1 e2 e2 e3]" {
		t.Fatalf("delivered: %v", got)
	}
}

// A fresh subscribe after a lapse says truncated when it can't replay
// everything since the cursor: one it can't place, one older than the
// publishes kept, or a maxAgeMs that cuts what was published since.
func TestMCPEventsReplayTruncated(t *testing.T) {
	resubscribe := func(t *testing.T, endpoint string, cb *fakeChatGPT, channel string, cursor any, maxAgeMs any) map[string]any {
		t.Helper()
		p := draftParams(channel, cb.url(), cb.secret())
		p["cursor"] = cursor
		if maxAgeMs != nil {
			p["maxAgeMs"] = maxAgeMs
		}
		res := draft(t, endpoint, "u1", "events/subscribe", p)
		if res.Error != nil {
			t.Fatalf("fresh subscribe: %+v %+v", res.Error, res.Result)
		}
		return res.Result
	}
	settled := func(t *testing.T, cb *fakeChatGPT, want string) {
		t.Helper()
		time.Sleep(300 * time.Millisecond)
		if got := texts(cb.got(), "e1", "e2", "e3"); fmt.Sprint(got) != want {
			t.Fatalf("delivered: %v, want %s", got, want)
		}
	}

	t.Run("a cursor it can't place", func(t *testing.T) {
		_, _, endpoint, cb, channel, _ := lapsedSubscription(t)
		if res := resubscribe(t, endpoint, cb, channel, "c1.0", nil); res["truncated"] != true {
			t.Fatalf("c1.0: %+v", res)
		}
		settled(t, cb, "[e1 e2]")
	})

	t.Run("older than the publishes kept", func(t *testing.T) {
		c, _, endpoint, cb, channel, cursor := lapsedSubscription(t)
		// The cursor's event (e1, and everything before it) was published
		// and arrived over a day ago; e2 and e3 were published since, and
		// are kept.
		if _, err := c.DB.Pool.Exec(ctx, `UPDATE topic_events SET published_at = now() - interval '25 hours'
			WHERE app_id = 'chat' AND (text || coalesce(data::text, '')) LIKE '%zqe1zq%'`); err != nil {
			t.Fatal(err)
		}
		tenant := tenantOf(t, c, "chat", "u1")
		seq, _ := mcpevents.ParseCursor(cursor)
		if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE events SET received_at = now() - interval '25 hours' WHERE seq <= $1`, seq)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if res := resubscribe(t, endpoint, cb, channel, cursor, nil); res["truncated"] != true {
			t.Fatalf("old cursor: %+v", res)
		}
		waitFor(t, "the replay", func() bool { return len(cb.got()) == 4 })
		settled(t, cb, "[e1 e2 e2 e3]")
	})

	t.Run("maxAgeMs cuts a publish", func(t *testing.T) {
		c, _, endpoint, cb, channel, cursor := lapsedSubscription(t)
		// e1 was published 11 minutes ago, e2 (also delivered) 10 minutes
		// ago, e3 just now.
		if _, err := c.DB.Pool.Exec(ctx, `UPDATE topic_events SET published_at = now() - interval '10 minutes' WHERE app_id = 'chat' AND (text || coalesce(data::text, '')) LIKE '%zqe2zq%'`); err != nil {
			t.Fatal(err)
		}
		if _, err := c.DB.Pool.Exec(ctx, `UPDATE topic_events SET published_at = now() - interval '11 minutes' WHERE app_id = 'chat' AND (text || coalesce(data::text, '')) LIKE '%zqe1zq%'`); err != nil {
			t.Fatal(err)
		}
		tenant := tenantOf(t, c, "chat", "u1")
		seq, _ := mcpevents.ParseCursor(cursor)
		if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE events SET received_at = now() - interval '11 minutes' WHERE seq <= $1`, seq)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if res := resubscribe(t, endpoint, cb, channel, cursor, 60000); res["truncated"] != true {
			t.Fatalf("maxAgeMs: %+v", res)
		}
		waitFor(t, "the replay", func() bool { return len(cb.got()) == 3 })
		settled(t, cb, "[e1 e2 e3]")
	})

	t.Run("a cursor after everything", func(t *testing.T) {
		_, _, endpoint, cb, channel, _ := lapsedSubscription(t)
		// The newest cursor the client got, then nothing published but e3.
		last := *cb.got()[1].Body.Cursor
		if res := resubscribe(t, endpoint, cb, channel, last, nil); res["truncated"] != false {
			t.Fatalf("latest cursor: %+v", res)
		}
		waitFor(t, "e3", func() bool { return len(cb.got()) == 3 })
		settled(t, cb, "[e1 e2 e3]")
	})
}

// When access ends (the app's grant revoked), the draft client is told: a
// terminated envelope with Forbidden, and the webhook is gone.
func TestMCPEventsTerminated(t *testing.T) {
	c, app, _, cb, _ := eventsConversation(t, false)
	tenant := tenantOf(t, c, "chat", "u1")
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE grants SET revoked_at = now() WHERE app_id = 'chat'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	publish(t, c, app, "after revocation")
	waitFor(t, "the terminated envelope", func() bool { return len(cb.envelopes()) == 1 })
	env := cb.envelopes()[0]
	if env.Type != "terminated" || env.Error.Code != -32024 || env.Error.Message != "Forbidden" || env.Error.Data["reason"] != "Access revoked" ||
		!strings.HasPrefix(env.ID, "msg_terminated_") {
		t.Fatalf("terminated: %+v", env)
	}
	var n int
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM mcp_event_targets`).Scan(&n)
	if n != 0 || len(cb.got()) != 0 {
		t.Fatalf("%d webhooks, %d events", n, len(cb.got()))
	}
}

// Access is rechecked at least hourly: a webhook last checked two hours ago
// is checked again (and marked), and one whose conversation is no longer
// its agent's ends with a terminated envelope.
func TestMCPEventsReauthorize(t *testing.T) {
	c, app, _, cb, _ := eventsConversation(t, false)
	tenant := tenantOf(t, c, "chat", "u1")
	stale := func() {
		t.Helper()
		if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE mcp_event_webhooks SET authorized_at = now() - interval '2 hours'`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	checkedLately := func() bool {
		var ok bool
		c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT coalesce(bool_and(authorized_at > now() - interval '1 minute'), false) FROM mcp_event_webhooks`).Scan(&ok)
		})
		return ok
	}
	stale()
	waitFor(t, "the recheck", checkedLately)
	publish(t, c, app, "still fine")
	waitFor(t, "the event", func() bool { return len(cb.got()) == 1 })

	// The subscription's session is now another agent's (a second
	// conversation's agent, in the same workspace).
	if _, err := subscribe(app, caller{User: "u1", Client: "claude-code"}, "u/u1/ci", subOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sessions SET agent_id = 'claude-code' WHERE id IN (SELECT session_id FROM mcp_event_webhooks)`)
		return err
	}); err != nil {
		t.Fatalf("moving the session to another agent: %v", err)
	}
	stale()
	waitFor(t, "the terminated envelope", func() bool { return len(cb.envelopes()) == 1 })
	if env := cb.envelopes()[0]; env.Type != "terminated" || env.Error.Data["reason"] != "The conversation is gone" {
		t.Fatalf("terminated: %+v", env)
	}
}

// nativeChat subscribes a draft client's callback to chat.message
// {channel: eng} of an app with chatEvents.
func nativeChat(t *testing.T) (*coretest.Core, *wakeshift.Client, *fakeChatGPT, map[string]any) {
	t.Helper()
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	cb := newFakeChatGPT(t)
	var res map[string]any
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/subscribe", map[string]any{
		"call": map[string]any{"user": "u1", "client": "acme-agent", "user_agent": draftUA}, "name": "chat.message",
		"arguments": map[string]any{"channel": "eng"}, "dialect": event.DialectDraft,
		"delivery": map[string]any{"mode": "webhook", "url": cb.url(), "secret": cb.secret()}, "ttl_ms": 3600000}, &res); code != 200 {
		t.Fatalf("subscribe: %d %s", code, e)
	}
	return c, app, cb, res
}

func emitChat(t *testing.T, c *coretest.Core, app *wakeshift.Client, tokens ...string) {
	t.Helper()
	for _, tok := range tokens {
		if code, e := appAPI(t, c, app, "/api/apps/v1/events/emit", map[string]any{"name": "chat.message", "arguments": map[string]any{"channel": "eng"},
			"payload": map[string]any{"text": "zq" + tok + "zq", "from": "ada"}, "text": "message zq" + tok + "zq"}, nil); code != 200 {
			t.Fatalf("emit: %d %s", code, e)
		}
	}
}

// A subscription to one of the app's event types gets that event: its
// name, the app's payload untouched as data, a cursor, and the rendered
// line in _meta; in order, even when one fails first.
func TestNativeEventDelivery(t *testing.T) {
	c, app, cb, res := nativeChat(t)
	if !strings.HasPrefix(fmt.Sprint(res["cursor"]), "c1.") || res["truncated"] != false {
		t.Fatalf("subscribe result: %+v", res)
	}
	cb.status = func(n int) int {
		if n == 1 {
			return 503 // the first fails once
		}
		return 200
	}
	emitChat(t, c, app, "a", "b", "c")
	waitFor(t, "three events", func() bool { return len(cb.got()) == 3 })
	got := cb.got()
	var cursors []int64
	for i, tok := range []string{"a", "b", "c"} {
		e := got[i]
		if e.Body.Name != "chat.message" || len(e.Body.Data) != 2 || e.Body.Data["text"] != "zq"+tok+"zq" || e.Body.Data["from"] != "ada" {
			t.Fatalf("event %d: %+v", i, e.Body)
		}
		line := e.Body.Meta[event.TextMeta]
		if !strings.Contains(line, "message zq"+tok+"zq") || !strings.Contains(line, `trust="external"`) || strings.Contains(line, `"from":"ada"`) {
			t.Fatalf("rendered line %d: %q", i, line)
		}
		if e.Body.Cursor == nil {
			t.Fatalf("event %d: no cursor", i)
		}
		n, err := mcpevents.ParseCursor(*e.Body.Cursor)
		if err != nil {
			t.Fatal(err)
		}
		cursors = append(cursors, n)
		if e.Sub != res["id"] {
			t.Fatalf("event %d to %s, want %s", i, e.Sub, res["id"])
		}
	}
	if !(cursors[0] < cursors[1] && cursors[1] < cursors[2]) {
		t.Fatalf("cursors: %v", cursors)
	}
	// The failed first attempt was retried before anything after it went.
	if n := len(cb.tried()); n != 4 {
		t.Fatalf("%d attempts, want 4", n)
	}
	if order := cb.tried(); fmt.Sprint(order) != "[zqazq zqazq zqbzq zqczq]" {
		t.Fatalf("attempt order: %v", order)
	}
}

// Revoking an event subscription in the console tells a draft receiver: a
// terminated envelope, then the webhook is gone and nothing more arrives.
func TestNativeEventRevoked(t *testing.T) {
	c, app, cb, res := nativeChat(t)
	tenant := userTenant(t, c, "chat", "u1")
	var id string
	c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id::text FROM event_subscriptions WHERE ended_at IS NULL`).Scan(&id)
	})
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := appapi.RevokeEventSubscription(ctx, tx, id, audit.User("ada"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the terminated envelope", func() bool { return len(cb.envelopes()) == 1 })
	env := cb.envelopes()[0]
	if env.Type != "terminated" || env.Error.Code != -32024 || env.Error.Data["reason"] != "Access revoked" {
		t.Fatalf("terminated: %+v", env)
	}
	waitFor(t, "the webhook removed", func() bool {
		var n int
		c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM mcp_event_webhooks WHERE id = $1`, res["id"]).Scan(&n)
		})
		return n == 0
	})
	emitChat(t, c, app, "after")
	time.Sleep(200 * time.Millisecond)
	if n := len(cb.got()); n != 0 {
		t.Fatalf("delivered after revoking: %d", n)
	}
}
