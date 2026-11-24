package coretest_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/mcpevents/event"
)

// MCP Events' poll delivery (events/poll), as a draft client (Mastra)
// speaks it through an app's MCP endpoint.

func pollParams(channel string, cursor any, extra map[string]any) map[string]any {
	p := map[string]any{"name": "chat.message", "arguments": map[string]any{"channel": channel}, "cursor": cursor,
		"_meta": map[string]any{"io.modelcontextprotocol/clientInfo": map[string]any{"name": "acme-agent", "version": "2"}}}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

// pollUntil polls from cursor until the result has n events (a publish
// reaches the subscription's session a moment after it's emitted).
func pollUntil(t *testing.T, endpoint, cursor string, n int, extra map[string]any) map[string]any {
	t.Helper()
	var last map[string]any
	waitFor(t, fmt.Sprintf("%d polled events", n), func() bool {
		res := draft(t, endpoint, "u1", "events/poll", pollParams("eng", cursor, extra))
		if res.Error != nil {
			t.Fatalf("poll: %+v", res.Error)
		}
		last = res.Result
		evs, _ := last["events"].([]any)
		return len(evs) >= n
	})
	return last
}

func polledTexts(res map[string]any) []string {
	var out []string
	for _, e := range res["events"].([]any) {
		occ := e.(map[string]any)
		data := occ["data"].(map[string]any)
		out = append(out, fmt.Sprint(data["text"]))
	}
	return out
}

// A poll subscribes on its first call (from now), then each poll returns
// what was emitted after its cursor, in order, as occurrences (name,
// eventId, the app's payload untouched, the rendered line in _meta); a
// poll that doesn't move its cursor gets the same events again (at least
// once), one that does gets nothing more.
func TestMCPEventsPoll(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app, endpoint := chatEndpoint(t, c, "chat")

	list := draft(t, endpoint, "u1", "events/list", map[string]any{})
	evs, _ := list.Result["events"].([]any)
	for _, e := range evs {
		if d := e.(map[string]any)["delivery"]; fmt.Sprint(d) != "[webhook poll push]" {
			t.Fatalf("events/list: %+v", e)
		}
	}
	if len(evs) == 0 {
		t.Fatalf("events/list: %+v", list.Result)
	}

	first := draft(t, endpoint, "u1", "events/poll", pollParams("eng", nil, nil))
	if first.Error != nil {
		t.Fatalf("first poll: %+v", first.Error)
	}
	r := first.Result
	if r["resultType"] != "complete" || fmt.Sprint(r["events"]) != "[]" || r["cursor"] != "c1.0" || r["truncated"] != false ||
		r["hasMore"] != false || r["nextPollMs"] != float64(10000) {
		t.Fatalf("first poll: %+v", r)
	}
	emitChat(t, c, app, "a", "b")
	got := pollUntil(t, endpoint, "c1.0", 2, nil)
	if txt := polledTexts(got); fmt.Sprint(txt) != "[zqazq zqbzq]" {
		t.Fatalf("polled: %v", txt)
	}
	occ := got["events"].([]any)[0].(map[string]any)
	meta, _ := occ["_meta"].(map[string]any)
	if occ["name"] != "chat.message" || occ["eventId"] == "" || occ["timestamp"] == "" || occ["cursor"] != nil ||
		!strings.Contains(fmt.Sprint(meta["ai.wakeshift/text"]), "message zqazq") {
		t.Fatalf("occurrence: %+v", occ)
	}
	cursor := got["cursor"].(string)
	if cursor == "c1.0" {
		t.Fatalf("cursor didn't move: %+v", got)
	}

	// The same cursor again: the same events, the same IDs.
	again := draft(t, endpoint, "u1", "events/poll", pollParams("eng", "c1.0", nil)).Result
	if fmt.Sprint(polledTexts(again)) != "[zqazq zqbzq]" || again["events"].([]any)[0].(map[string]any)["eventId"] != occ["eventId"] {
		t.Fatalf("polled again: %+v", again)
	}
	// Moved on: nothing more, and both acknowledged.
	next := draft(t, endpoint, "u1", "events/poll", pollParams("eng", cursor, nil)).Result
	if fmt.Sprint(next["events"]) != "[]" || next["cursor"] != cursor {
		t.Fatalf("after the cursor: %+v", next)
	}
	tenant := userTenant(t, c, "chat", "u1")
	var acked int
	c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE state = 'acked'`).Scan(&acked)
	})
	if acked != 2 {
		t.Fatalf("acknowledged: %d", acked)
	}

	// A batch past maxEvents says there's more, and to poll again now.
	emitChat(t, c, app, "c", "d")
	part := pollUntil(t, endpoint, cursor, 1, map[string]any{"maxEvents": 1})
	if fmt.Sprint(polledTexts(part)) != "[zqczq]" || part["hasMore"] != true || part["nextPollMs"] != float64(0) {
		waitFor(t, "both waiting", func() bool {
			res := draft(t, endpoint, "u1", "events/poll", pollParams("eng", cursor, map[string]any{"maxEvents": 1})).Result
			part = res
			return res["hasMore"] == true
		})
		if fmt.Sprint(polledTexts(part)) != "[zqczq]" || part["nextPollMs"] != float64(0) {
			t.Fatalf("partial batch: %+v", part)
		}
	}
	rest := pollUntil(t, endpoint, part["cursor"].(string), 1, nil)
	if fmt.Sprint(polledTexts(rest)) != "[zqdzq]" || rest["hasMore"] != false {
		t.Fatalf("the rest: %+v", rest)
	}

	// One subscription, listed as a poll's, and the consent record has its
	// audit entry.
	var dest string
	var n, audited int
	c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*), min(destination) FROM event_subscriptions WHERE ended_at IS NULL`).Scan(&n, &dest); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'event_subscription.created'`).Scan(&audited)
	})
	if n != 1 || dest != "poll" || audited != 1 {
		t.Fatalf("subscriptions: %d %q, audited %d", n, dest, audited)
	}
}

// A poll subscription the person stops in the console refuses the next
// poll that carries its cursor (Forbidden: poll's termination), so the
// client's loop stops; a poll from null starts a new one, as a webhook
// refresh after termination does. Authorize runs on every poll, and a poll
// with no user, or a push request, is refused.
func TestMCPEventsPollStopped(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	_, endpoint := chatEndpoint(t, c, "chat")
	first := draft(t, endpoint, "u1", "events/poll", pollParams("eng", nil, nil))
	if first.Error != nil {
		t.Fatalf("first poll: %+v", first.Error)
	}
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
	for range 2 { // and it stays stopped
		res := draft(t, endpoint, "u1", "events/poll", pollParams("eng", "c1.0", nil))
		if res.Error == nil || res.Error.Code != -32024 {
			t.Fatalf("poll after revoking: %+v", res)
		}
	}
	again := draft(t, endpoint, "u1", "events/poll", pollParams("eng", nil, nil))
	if again.Error != nil || again.Result["cursor"] != "c1.0" {
		t.Fatalf("poll from null after revoking: %+v", again)
	}
	// A cursor past anything the subscription had acknowledges nothing.
	if res := draft(t, endpoint, "u1", "events/poll", pollParams("eng", "c1.999999", nil)); res.Error == nil || res.Error.Code != -32602 {
		t.Fatalf("a cursor from the future: %+v", res)
	}

	if res := draft(t, endpoint, "u1", "events/poll", pollParams("secret", nil, nil)); res.Error == nil || res.Error.Code != -32024 {
		t.Fatalf("authorize: %+v", res)
	}
	if res := draft(t, endpoint, "", "events/poll", pollParams("eng", nil, nil)); res.Error == nil || res.Error.Code != -32024 {
		t.Fatalf("no user: %+v", res)
	}
	if res := draft(t, endpoint, "u1", "events/poll", pollParams("eng", "nope", nil)); res.Error == nil || res.Error.Code != -32602 {
		t.Fatalf("a cursor Wakeshift didn't make: %+v", res)
	}
}

// Where the core doesn't serve push (WAKESHIFT_MCP_EVENTS_PUSH off), the
// SDK doesn't either: events/list leaves it out of delivery and
// events/stream answers Unsupported, value "push", in both dialects. (This
// package's cores serve push, so an events/stream there opens a stream:
// mcpevents_push_test.go.)
func TestMCPEventsStreamPushOff(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.StartWith(t, func(o *core.Options) { o.MCPEventsPush = false })
	_, endpoint := chatEndpoint(t, c, "chat")
	list := draft(t, endpoint, "u1", "events/list", map[string]any{})
	for _, e := range list.Result["events"].([]any) {
		if d := e.(map[string]any)["delivery"]; fmt.Sprint(d) != "[webhook poll]" {
			t.Fatalf("events/list with push off: %+v", e)
		}
	}
	if res := draft(t, endpoint, "u1", "events/stream", pollParams("eng", nil, nil)); res.Error == nil || res.Error.Code != -32026 || res.Error.Data["value"] != "push" {
		t.Fatalf("push, draft: %+v", res)
	}
	if res := rpcAs(t, endpoint, "u1", "openai-mcp/1.0.0", nil, "events/stream", map[string]any{"name": "chat.message", "arguments": map[string]any{"channel": "eng"}}); res.Error == nil || res.Error.Code != event.CodesFor(event.DialectChatGPT).Unsupported {
		t.Fatalf("push, ChatGPT: %+v", res)
	}
}

// A poll subscription that lapses (no poll for its lease) is a new one on
// the next poll: what was emitted meanwhile went nowhere, so a poll with an
// old cursor says truncated.
func TestMCPEventsPollLapsed(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app, endpoint := chatEndpoint(t, c, "chat")
	if res := draft(t, endpoint, "u1", "events/poll", pollParams("eng", nil, nil)); res.Error != nil {
		t.Fatalf("first poll: %+v", res.Error)
	}
	tenant := userTenant(t, c, "chat", "u1")
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE topic_subscriptions SET expires_at = now() - interval '1 second'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	emitChat(t, c, app, "lost")
	res := draft(t, endpoint, "u1", "events/poll", pollParams("eng", "c1.0", nil))
	if res.Error != nil || res.Result["truncated"] != true || fmt.Sprint(res.Result["events"]) != "[]" {
		t.Fatalf("poll after lapsing: %+v", res)
	}
	var live, ended int
	c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE ended_at IS NULL), count(*) FILTER (WHERE ended_reason = 'expired') FROM event_subscriptions`).Scan(&live, &ended)
	})
	if live != 1 || ended != 1 {
		t.Fatalf("live %d, expired %d", live, ended)
	}
}
