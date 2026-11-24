package coretest_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/jws"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// Who may subscribe: the manifest's per-event subscribe setting,
// "tools" or "any".

// manifestResult is a manifest publish's answer: the events as stored and
// the subscribe settings it changed.
type manifestResult struct {
	Events []struct {
		Name      string `json:"name"`
		Subscribe string `json:"subscribe"`
	} `json:"events"`
	Changed []apps.SubscribeChange `json:"changed"`
}

// subscribeOf is the event's stored setting in a manifest result.
func (m manifestResult) subscribeOf(event string) string {
	for _, e := range m.Events {
		if e.Name == event {
			return e.Subscribe
		}
	}
	return "(missing)"
}

// publishManifest sends events (chat.message as chatEvents has it, with
// subscribe set unless it's "") with PUT /manifest (put) or POST
// /manifest/merge, as the app.
func publishManifest(t *testing.T, c *coretest.Core, app *wakeshift.Client, put bool, subscribe map[string]string) (manifestResult, int) {
	t.Helper()
	var events []map[string]any
	for name, setting := range subscribe {
		e := chatEvents().Events[0]
		e.Name, e.SubscribePolicy = name, setting
		if name != "chat.message" {
			// A topic of its own: two events on one topic are refused
			// (ambiguous_topic, #912).
			e.Topic = strings.ReplaceAll(name, ".", "-") + "/{channel}"
		}
		b, _ := json.Marshal(e)
		var m map[string]any
		json.Unmarshal(b, &m)
		events = append(events, m)
	}
	method, path := "POST", "/api/apps/v1/manifest/merge"
	if put {
		method, path = "PUT", "/api/apps/v1/manifest"
	}
	jti := make([]byte, 16)
	rand.Read(jti)
	now := time.Now()
	tok, err := jws.Sign(coretest.CredsOf(app).Key, "", "app+jwt", map[string]any{"iss": coretest.CredsOf(app).ID, "sub": coretest.CredsOf(app).ID, "aud": c.URL + "/api/apps",
		"iat": now.Unix(), "exp": now.Add(time.Minute).Unix(), "jti": base64.RawURLEncoding.EncodeToString(jti)})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"events": events})
	req, _ := http.NewRequest(method, c.URL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out manifestResult
	json.NewDecoder(resp.Body).Decode(&out)
	return out, resp.StatusCode
}

// subscribeAuditDetails are the workspace's event.subscribe_changed
// entries' details, oldest first.
func subscribeAuditDetails(t *testing.T, c *coretest.Core, tenant string) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT detail FROM audit_log WHERE action = 'event.subscribe_changed' ORDER BY id`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[map[string]any])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// bridgedSub subscribes user's conversation to chat.message #channel with
// POST /event-subscriptions, as tool (call.tool; "" for none).
func bridgedSub(t *testing.T, c *coretest.Core, app *wakeshift.Client, user, tool, channel string) (int, string) {
	t.Helper()
	call := map[string]any{"user": user, "client": "claude-code"}
	if tool != "" {
		call["tool"] = tool
	}
	return appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": call, "name": "chat.message",
		"arguments": map[string]any{"channel": channel}}, nil)
}

// Old SDKs' manifests (no subscribe) keep "any"; a setting, once given, is
// sticky across manifests that leave it out (a mixed fleet: new, old, new),
// a full PUT and the event leaving the manifest and coming back; opening
// it again is explicit, audited in each of the app's workspaces, and
// reported. A new event's first setting changes nothing.
func TestSubscribeSettingSticky(t *testing.T) {
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	if code, e := bridgedSub(t, c, app, "u1", "watch", "eng"); code != 200 {
		t.Fatalf("subscribe: %d %s", code, e)
	}
	tenant := userTenant(t, c, "chat", "u1")

	// An old SDK republishing: nothing changes, and the event stays open.
	got, code := publishManifest(t, c, app, false, map[string]string{"chat.message": ""})
	if code != 200 || got.subscribeOf("chat.message") != "" || len(got.Changed) != 0 || got.Changed == nil {
		t.Fatalf("old SDK: %d %+v", code, got)
	}
	if code, e := bridgedSub(t, c, app, "u1", "subscribe_events", "ops"); code != 200 {
		t.Fatalf("subscribe_events on an event without a setting: %d %s", code, e)
	}

	// New: closed.
	got, code = publishManifest(t, c, app, false, map[string]string{"chat.message": "tools"})
	if code != 200 || got.subscribeOf("chat.message") != "tools" ||
		fmt.Sprint(got.Changed) != "[{chat.message any tools 1}]" {
		t.Fatalf("new SDK: %d %+v", code, got)
	}
	// Old again (merge, then a full PUT), and the event leaving and coming
	// back: still closed, nothing changed.
	for name, put := range map[string]bool{"merge": false, "put": true} {
		got, code = publishManifest(t, c, app, put, map[string]string{"chat.message": ""})
		if code != 200 || got.subscribeOf("chat.message") != "tools" || len(got.Changed) != 0 {
			t.Fatalf("old SDK after new (%s): %d %+v", name, code, got)
		}
	}
	if got, code = publishManifest(t, c, app, true, map[string]string{"chat.other": ""}); code != 200 || got.subscribeOf("chat.message") != "(missing)" {
		t.Fatalf("removing it: %d %+v", code, got)
	}
	if got, code = publishManifest(t, c, app, false, map[string]string{"chat.message": ""}); code != 200 || got.subscribeOf("chat.message") != "tools" {
		t.Fatalf("back again: %d %+v", code, got)
	}
	if code, e := bridgedSub(t, c, app, "u1", "subscribe_events", "dev"); code != 403 || e != "tools_only" {
		t.Fatalf("subscribe_events, still closed: %d %s", code, e)
	}
	// Opening it takes a manifest that says so.
	got, code = publishManifest(t, c, app, false, map[string]string{"chat.message": "any"})
	if code != 200 || got.subscribeOf("chat.message") != "any" || fmt.Sprint(got.Changed) != "[{chat.message tools any 0}]" {
		t.Fatalf("opening: %d %+v", code, got)
	}
	if code, e := bridgedSub(t, c, app, "u1", "subscribe_events", "dev"); code != 200 {
		t.Fatalf("subscribe_events, opened: %d %s", code, e)
	}
	details := subscribeAuditDetails(t, c, tenant)
	if len(details) != 2 || details[0]["to"] != "tools" || details[0]["ended"] != float64(1) || details[0]["kept_without_via"] != float64(0) ||
		details[1]["from"] != "tools" || details[1]["to"] != "any" {
		t.Fatalf("audit: %+v", details)
	}

	// A new event's first setting: nothing to change.
	if got, code = publishManifest(t, c, app, false, map[string]string{"chat.fresh": "tools"}); code != 200 || len(got.Changed) != 0 || got.subscribeOf("chat.fresh") != "tools" {
		t.Fatalf("a new event: %d %+v", code, got)
	}
	if _, code = publishManifest(t, c, app, false, map[string]string{"chat.message": "everyone"}); code != 400 {
		t.Fatalf("an unknown setting: %d", code)
	}
	var logged int
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM app_log WHERE app_id = 'chat' AND detail::text LIKE '%subscribe any -> tools%'`).Scan(&logged)
	if logged != 1 {
		t.Fatalf("the app's log: %d", logged)
	}
}

// A tools-only event refuses subscribe_events, a call naming no tool,
// events/subscribe and every events/poll, a forged cursor's included,
// before anything is made; the app's own tool subscribes, recorded as
// tool:<name>; unsubscribing and listing still work on every path.
func TestToolsOnlyRefusals(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	chatgpt := newFakeChatGPT(t)
	if _, code := publishManifest(t, c, app, false, map[string]string{"chat.message": "tools"}); code != 200 {
		t.Fatalf("manifest: %d", code)
	}
	for _, tool := range []string{"subscribe_events", ""} {
		if code, e := bridgedSub(t, c, app, "u1", tool, "eng"); code != 403 || e != "tools_only" {
			t.Fatalf("tool %q: %d %s", tool, code, e)
		}
	}
	native := map[string]any{"call": map[string]any{"user": "u1", "client": "openai-mcp"}, "name": "chat.message",
		"arguments": map[string]any{"channel": "eng"}, "delivery": map[string]any{"mode": "webhook", "url": chatgpt.url(), "secret": chatgpt.secret()}}
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/subscribe", native, nil); code != 403 || e != "tools_only" || len(chatgpt.verified) != 0 {
		t.Fatalf("events/subscribe: %d %s, verified %v", code, e, chatgpt.verified)
	}
	for _, cursor := range []any{nil, "c1.0", "c1.5"} {
		poll := map[string]any{"call": map[string]any{"user": "u1", "client": "mastra"}, "name": "chat.message",
			"arguments": map[string]any{"channel": "eng"}, "cursor": cursor}
		if code, e := appAPI(t, c, app, "/api/apps/v1/events/poll", poll, nil); code != 403 || e != "tools_only" {
			t.Fatalf("events/poll from %v: %d %s", cursor, code, e)
		}
	}
	var made eventSubResult
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": map[string]any{"user": "u1", "client": "claude-code",
		"tool": "watch_channel"}, "name": "chat.message", "arguments": map[string]any{"channel": "eng"}}, &made); code != 200 {
		t.Fatalf("the app's tool: %d %s", code, e)
	}
	tenant := userTenant(t, c, "chat", "u1")
	var via string
	var n int
	c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*), coalesce(min(via), '') FROM event_subscriptions`).Scan(&n, &via)
	})
	if n != 1 || via != "tool:watch_channel" {
		t.Fatalf("recorded: %d %q", n, via)
	}

	var list struct{ Subscriptions []map[string]any }
	if code, _ := appAPI(t, c, app, "/api/apps/v1/event-subscriptions/list", map[string]any{"user": "u1"}, &list); code != 200 || len(list.Subscriptions) != 1 {
		t.Fatalf("list: %d %+v", code, list)
	}
	if code, _ := appAPI(t, c, app, "/api/apps/v1/events/unsubscribe", native, nil); code != 200 {
		t.Fatalf("events/unsubscribe: %d", code)
	}
	var ended struct{ Ended bool }
	if code, _ := appAPI(t, c, app, "/api/apps/v1/event-subscriptions/unsubscribe", map[string]any{"call": map[string]any{"user": "u1",
		"client": "claude-code", "tool": "unsubscribe_events", "session": made.Session}, "name": "chat.message", "arguments": map[string]any{"channel": "eng"}}, &ended); code != 200 || !ended.Ended {
		t.Fatalf("unsubscribe_events: %d %+v", code, ended)
	}
}

// Closing an event ends, in the same transaction, what wasn't made by the
// app's tools: subscribe_events', native and poll subscriptions, and
// native ones from before via; it keeps the tools' and conversations'
// without via (counted in each workspace's audit entry). A conversation is
// told; a webhook's receiver gets a terminated envelope; emits still reach
// what's kept.
func TestCloseEndsWhatToolsDidntMake(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	chatgpt := newFakeChatGPT(t)
	// A spec client: its webhooks take terminated envelopes (ChatGPT's,
	// openai-mcp in the client table, take none).
	nativeSub := func(user string) map[string]any {
		var out map[string]any
		if code, e := appAPI(t, c, app, "/api/apps/v1/events/subscribe", map[string]any{"call": map[string]any{"user": user, "client": "acme-agent"},
			"name": "chat.message", "arguments": map[string]any{"channel": "eng"},
			"delivery": map[string]any{"mode": "webhook", "url": chatgpt.url(), "secret": chatgpt.secret()}, "ttl_ms": 3600000}, &out); code != 200 {
			t.Fatalf("events/subscribe for %s: %d %s", user, code, e)
		}
		return out
	}
	for user, tool := range map[string]string{"tool": "watch", "builtin": "subscribe_events", "notool": ""} {
		if code, e := bridgedSub(t, c, app, user, tool, "eng"); code != 200 {
			t.Fatalf("subscribe %s: %d %s", user, code, e)
		}
	}
	nativeSub("native")
	nativeSub("oldnative")
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/poll", map[string]any{"call": map[string]any{"user": "poller", "client": "mastra"},
		"name": "chat.message", "arguments": map[string]any{"channel": "eng"}, "cursor": nil}, nil); code != 200 {
		t.Fatalf("poll: %d %s", code, e)
	}
	// A webhook from before via.
	old := userTenant(t, c, "chat", "oldnative")
	if err := c.DB.InTenant(ctx, old, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE event_subscriptions SET via = NULL`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	vias := map[string]string{}
	for _, user := range []string{"tool", "builtin", "notool", "native", "poller"} {
		c.DB.InTenant(ctx, userTenant(t, c, "chat", user), func(tx pgx.Tx) error {
			var v string
			err := tx.QueryRow(ctx, `SELECT coalesce(via, '') FROM event_subscriptions`).Scan(&v)
			vias[user] = v
			return err
		})
	}
	if fmt.Sprint(vias) != "map[builtin:subscribe_events native:native notool: poller:poll tool:tool:watch]" {
		t.Fatalf("via: %v", vias)
	}

	got, code := publishManifest(t, c, app, false, map[string]string{"chat.message": "tools"})
	if code != 200 || fmt.Sprint(got.Changed) != "[{chat.message any tools 4}]" {
		t.Fatalf("close: %d %+v", code, got)
	}
	state := func(user string) (live bool, reason string) {
		c.DB.InTenant(ctx, userTenant(t, c, "chat", user), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT ended_at IS NULL, coalesce(ended_reason, '') FROM event_subscriptions`).Scan(&live, &reason)
		})
		return live, reason
	}
	for user, want := range map[string]string{"tool": "live", "notool": "live", "builtin": "policy_changed", "native": "policy_changed",
		"oldnative": "policy_changed", "poller": "policy_changed"} {
		live, reason := state(user)
		if (want == "live") != live || !live && reason != want {
			t.Errorf("%s: live %t, %q", user, live, reason)
		}
	}
	for user, kept := range map[string]float64{"notool": 1, "tool": 0, "builtin": 0} {
		d := subscribeAuditDetails(t, c, userTenant(t, c, "chat", user))
		if len(d) != 1 || d[0]["kept_without_via"] != kept || d[0]["from"] != "any" || d[0]["to"] != "tools" {
			t.Errorf("%s's audit: %+v", user, d)
		}
	}
	if got := auditActions(t, c, userTenant(t, c, "chat", "builtin")); fmt.Sprint(got) != "[event_subscription.created event_subscription.policy_changed]" {
		t.Errorf("builtin's audit: %v", got)
	}
	// The subscribe_events conversation is told its subscription ended.
	var notices int
	c.DB.InTenant(ctx, userTenant(t, c, "chat", "builtin"), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM topic_subscriptions WHERE ended_reason = 'removed'`).Scan(&notices)
	})
	if notices != 1 {
		t.Errorf("the conversation's routing: %d ended as removed", notices)
	}
	// Both webhooks' receivers are told.
	waitFor(t, "terminated envelopes", func() bool {
		n := 0
		for _, e := range chatgpt.envelopes() {
			if e.Type == "terminated" {
				n++
			}
		}
		return n == 2
	})
	// The poller's next poll is refused, its cursor whatever.
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/poll", map[string]any{"call": map[string]any{"user": "poller", "client": "mastra"},
		"name": "chat.message", "arguments": map[string]any{"channel": "eng"}, "cursor": "c1.0"}, nil); code != 403 || e != "tools_only" {
		t.Fatalf("poll after closing: %d %s", code, e)
	}
	// What's kept still hears the event.
	emitChat(t, c, app, "after")
	for _, user := range []string{"tool", "notool"} {
		tenant := userTenant(t, c, "chat", user)
		waitFor(t, user+"'s delivery", func() bool { return deliveryCount(t, c, tenant) >= 1 })
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(chatgpt.got()); n != 0 {
		t.Fatalf("a closed webhook got %d events", n)
	}
}

// An app on 0.10.x (manifests without subscribe) with live native and
// subscribe_events subscriptions: republishing ends nothing, new native
// subscriptions work, and emits deliver.
func TestOldAppUnaffected(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	chatgpt := newFakeChatGPT(t)
	subscribe := func(user string) {
		t.Helper()
		if code, e := appAPI(t, c, app, "/api/apps/v1/events/subscribe", map[string]any{"call": map[string]any{"user": user, "client": "openai-mcp"},
			"name": "chat.message", "arguments": map[string]any{"channel": "eng"},
			"delivery": map[string]any{"mode": "webhook", "url": chatgpt.url(), "secret": chatgpt.secret()}, "ttl_ms": 3600000}, nil); code != 200 {
			t.Fatalf("events/subscribe for %s: %d %s", user, code, e)
		}
	}
	subscribe("u1")
	if code, e := bridgedSub(t, c, app, "u2", "subscribe_events", "eng"); code != 200 {
		t.Fatalf("subscribe_events: %d %s", code, e)
	}
	for _, put := range []bool{false, true} {
		if got, code := publishManifest(t, c, app, put, map[string]string{"chat.message": ""}); code != 200 || len(got.Changed) != 0 {
			t.Fatalf("republish: %d %+v", code, got)
		}
	}
	subscribe("u3")
	for _, user := range []string{"u1", "u2", "u3"} {
		var live int
		c.DB.InTenant(ctx, userTenant(t, c, "chat", user), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM event_subscriptions WHERE ended_at IS NULL`).Scan(&live)
		})
		if live != 1 {
			t.Fatalf("%s: %d live", user, live)
		}
		if d := subscribeAuditDetails(t, c, userTenant(t, c, "chat", user)); len(d) != 0 {
			t.Fatalf("%s's audit: %+v", user, d)
		}
	}
	emitChat(t, c, app, "hi")
	waitFor(t, "both webhooks' events", func() bool { return len(chatgpt.got()) == 2 })
	tenant := userTenant(t, c, "chat", "u2")
	waitFor(t, "the conversation's delivery", func() bool { return deliveryCount(t, c, tenant) >= 1 })
}
