package coretest_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/jws"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// chatEvents is a manifest with one event type: chat.message {channel,
// from?}, routed to channels/<channel> and filtered on the payload's from.
func chatEvents() apps.Manifest {
	return apps.Manifest{Events: []apps.EventType{{
		Name:        "chat.message",
		Description: "A message in a channel.",
		Arguments:   json.RawMessage(`{"type":"object","properties":{"channel":{"type":"string"},"from":{"type":"string"}},"required":["channel"],"additionalProperties":false}`),
		Payload:     json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"},"from":{"type":"string"}},"required":["text","from"]}`),
		Topic:       "channels/{channel}",
		Filter:      map[string]string{"from": "{from}"},
	}}}
}

// eventsApp is an app with chatEvents published, and the personal note
// (note.mine). Not the other notes: their all-placeholder topics ({a}/{b})
// could make chat.message's (apps.AmbiguousTopics).
func eventsApp(t *testing.T, c *coretest.Core, id string) *wakeshift.Client {
	t.Helper()
	app := bareApp(t, c, id)
	coretest.DefinePersonalNote(t, app)
	if err := app.PublishManifest(ctx, wakeshift.Manifest{Description: "Notes."}); err != nil {
		t.Fatal(err)
	}
	got, err := c.Apps.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	m := chatEvents()
	m.Events = append(m.Events, got.Manifest.Events...)
	if err := c.Apps.SetManifest(ctx, id, m); err != nil {
		t.Fatal(err)
	}
	return app
}

// appAPI calls the app API as the app (a fresh assertion), for calls
// wakeshift doesn't wrap yet, returning the status and the error code.
func appAPI(t *testing.T, c *coretest.Core, app *wakeshift.Client, path string, body, out any) (int, string) {
	t.Helper()
	code, errCode, err := appPost(c, app, path, body, out)
	if err != nil {
		t.Fatal(err)
	}
	return code, errCode
}

// appPost is appAPI for a goroutine of the test's own (which mustn't call
// t.Fatal): what failed is returned instead.
func appPost(c *coretest.Core, app *wakeshift.Client, path string, body, out any) (int, string, error) {
	code, raw, err := appPostRaw(c, app, path, body)
	if err != nil {
		return 0, "", err
	}
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	json.Unmarshal(raw, &e)
	if out != nil && code == 200 {
		if err := json.Unmarshal(raw, out); err != nil {
			return code, e.Error.Code, err
		}
	}
	return code, e.Error.Code, nil
}

// appPostRaw is appPost's request: the status and the body as it came.
func appPostRaw(c *coretest.Core, app *wakeshift.Client, path string, body any) (int, json.RawMessage, error) {
	jti := make([]byte, 16)
	rand.Read(jti)
	now := time.Now()
	tok, err := jws.Sign(coretest.CredsOf(app).Key, "", "app+jwt", map[string]any{"iss": coretest.CredsOf(app).ID, "sub": coretest.CredsOf(app).ID, "aud": c.URL + "/api/apps",
		"iat": now.Unix(), "exp": now.Add(time.Minute).Unix(), "jti": base64.RawURLEncoding.EncodeToString(jti)})
	if err != nil {
		return 0, nil, err
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", c.URL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	var raw json.RawMessage
	json.NewDecoder(resp.Body).Decode(&raw)
	return resp.StatusCode, raw, nil
}

type eventSubResult struct {
	EventSubscription struct {
		ID        string          `json:"id"`
		Event     string          `json:"event"`
		Arguments json.RawMessage `json:"arguments"`
		Created   bool            `json:"created"`
	} `json:"event_subscription"`
	Session  string `json:"session"`
	Addition struct {
		Text string `json:"text"`
	} `json:"addition"`
}

func userTenant(t *testing.T, c *coretest.Core, app, user string) string {
	t.Helper()
	var tenant string
	if err := c.DB.Pool.QueryRow(ctx, `SELECT id::text FROM tenants WHERE app_id = $1 AND external_id = $2`, app, user).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	return tenant
}

func auditActions(t *testing.T, c *coretest.Core, tenant string) []string {
	t.Helper()
	var out []string
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT action FROM audit_log WHERE action LIKE 'event_subscription.%' ORDER BY id`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// A manifest's event types are validated when it's published.
func TestManifestEventTypes(t *testing.T) {
	c := coretest.Start(t)
	noteApp(t, c, "chat")
	if err := c.Apps.SetManifest(ctx, "chat", chatEvents()); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*apps.EventType){
		"a reserved name": func(e *apps.EventType) { e.Name = "wakeshift.message" },
		"a bad schema":    func(e *apps.EventType) { e.Payload = json.RawMessage(`{"type":"nope"}`) },
		"a remote $ref":   func(e *apps.EventType) { e.Arguments = json.RawMessage(`{"$ref":"https://example.com/x.json"}`) },
		"a private topic": func(e *apps.EventType) { e.Topic = "u/{channel}" },
	} {
		m := chatEvents()
		change(&m.Events[0])
		if err := c.Apps.SetManifest(ctx, "chat", m); !errors.Is(err, apps.ErrInvalidManifest) {
			t.Errorf("%s: %v, want an invalid manifest", name, err)
		}
	}
	got, err := c.Apps.Get(ctx, "chat")
	if err != nil || len(got.Manifest.Events) != 1 || got.Manifest.Events[0].Name != "chat.message" {
		t.Fatalf("stored manifest: %+v %v", got.Manifest, err)
	}
}

// A bridged conversation subscribes by event name and arguments: equal
// arguments (in any order or spacing) are one subscription, different ones
// two; each is recorded and audited; an emit is checked against the
// payload schema and routed by topic and filter; unsubscribing ends one.
func TestEventSubscriptionsBridged(t *testing.T) {
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	call := map[string]any{"user": "u1", "client": "claude-code"}
	sub := func(args string) (eventSubResult, int, string) {
		var out eventSubResult
		code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": call, "name": "chat.message", "arguments": json.RawMessage(args)}, &out)
		return out, code, e
	}
	first, code, _ := sub(`{"channel": "eng"}`)
	if code != 200 || !first.EventSubscription.Created || string(first.EventSubscription.Arguments) != `{"channel":"eng"}` {
		t.Fatalf("subscribe: %d %+v", code, first)
	}
	call["session"] = first.Session
	again, _, _ := sub(` {"channel":"eng"} `)
	if again.EventSubscription.Created || again.EventSubscription.ID != first.EventSubscription.ID {
		t.Fatalf("equal arguments made another subscription: %+v", again)
	}
	ada, _, _ := sub(`{"from":"ada","channel":"eng"}`)
	if !ada.EventSubscription.Created || ada.EventSubscription.ID == first.EventSubscription.ID {
		t.Fatalf("different arguments: %+v", ada)
	}
	for args, want := range map[string]string{`{"channel":"Eng"}`: "bad_arguments", `{"room":"eng"}`: "bad_arguments", `{}`: "bad_arguments"} {
		if _, code, e := sub(args); code != 400 || e != want {
			t.Errorf("arguments %s: %d %s", args, code, e)
		}
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": call, "name": "chat.nope", "arguments": map[string]any{}}, nil); code != 404 || e != "unknown_event" {
		t.Fatalf("unknown event: %d %s", code, e)
	}

	// Emitting: the payload must match its schema, and the topic's
	// arguments are required.
	emit := func(args, payload string) (int, string) {
		return appAPI(t, c, app, "/api/apps/v1/events/emit", map[string]any{"name": "chat.message", "arguments": json.RawMessage(args),
			"payload": json.RawMessage(payload), "text": "a message"}, nil)
	}
	if code, e := emit(`{"channel":"eng"}`, `{"text":"hi"}`); code != 400 || e != "bad_arguments" {
		t.Fatalf("a payload missing a required field: %d %s", code, e)
	}
	if code, e := emit(`{}`, `{"text":"hi","from":"bob"}`); code != 400 || e != "bad_arguments" {
		t.Fatalf("an emit without its topic's argument: %d %s", code, e)
	}
	if code, _ := emit(`{"channel":"eng"}`, `{"text":"from bob","from":"bob"}`); code != 200 {
		t.Fatalf("emit: %d", code)
	}
	tenant := userTenant(t, c, "chat", "u1")
	// Bob's message matches the channel subscription, not Ada's: one
	// delivery. Ada's matches both: still one delivery (one per event and
	// conversation).
	waitFor(t, "bob's message delivered", func() bool { return deliveryCount(t, c, tenant) == 1 })
	if code, _ := emit(`{"channel":"eng"}`, `{"text":"from ada","from":"ada"}`); code != 200 {
		t.Fatal("emit")
	}
	waitFor(t, "ada's message delivered", func() bool { return deliveryCount(t, c, tenant) == 2 })
	if code, _ := emit(`{"channel":"ops"}`, `{"text":"elsewhere","from":"ada"}`); code != 200 {
		t.Fatal("emit")
	}
	time.Sleep(300 * time.Millisecond)
	if n := deliveryCount(t, c, tenant); n != 2 {
		t.Fatalf("another channel's message was delivered: %d", n)
	}

	var list struct {
		Subscriptions []appapi.EventSubscription `json:"subscriptions"`
	}
	if code, _ := appAPI(t, c, app, "/api/apps/v1/event-subscriptions/list", map[string]any{"user": "u1"}, &list); code != 200 || len(list.Subscriptions) != 2 {
		t.Fatalf("list: %d %+v", code, list)
	}
	if code, _ := appAPI(t, c, app, "/api/apps/v1/event-subscriptions/list", map[string]any{"user": "nobody"}, &list); code != 200 || len(list.Subscriptions) != 0 {
		t.Fatalf("another user's list: %+v", list)
	}

	var ended struct{ Ended bool }
	if code, _ := appAPI(t, c, app, "/api/apps/v1/event-subscriptions/unsubscribe", map[string]any{"call": call, "name": "chat.message",
		"arguments": map[string]any{"channel": "eng"}}, &ended); code != 200 || !ended.Ended {
		t.Fatalf("unsubscribe: %d %+v", code, ended)
	}
	appAPI(t, c, app, "/api/apps/v1/event-subscriptions/list", map[string]any{"user": "u1"}, &list)
	if len(list.Subscriptions) != 1 || list.Subscriptions[0].ID != ada.EventSubscription.ID {
		t.Fatalf("after unsubscribing: %+v", list.Subscriptions)
	}
	if got := strings.Join(auditActions(t, c, tenant), ","); got != "event_subscription.created,event_subscription.created,event_subscription.unsubscribed" {
		t.Fatalf("audit: %s", got)
	}
}

// A number argument written 1.0 is the same subscription as 1, so it hears
// an emit of 1 (a Python app's dict schema sends floats; the Go SDK's
// bridged path keeps the number as written). One recorded when its topic
// was still .../1.0 moves to the emit's topic at its next refresh.
func TestEventNumberArgumentTopic(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	if err := c.Apps.SetManifest(ctx, "ci", apps.Manifest{Events: []apps.EventType{{
		Name:        "ci.run",
		Description: "A CI run finished.",
		Arguments:   json.RawMessage(`{"type":"object","properties":{"pr":{"type":"integer"}},"required":["pr"],"additionalProperties":false}`),
		Payload:     json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`),
		Topic:       "ci/{pr}",
	}}}); err != nil {
		t.Fatal(err)
	}
	call := map[string]any{"user": "u1", "client": "claude-code"}
	sub := func(args string) eventSubResult {
		t.Helper()
		var out eventSubResult
		if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": call, "name": "ci.run", "arguments": json.RawMessage(args)}, &out); code != 200 {
			t.Fatalf("subscribe %s: %d %s", args, code, e)
		}
		return out
	}
	emit := func() {
		t.Helper()
		if code, e := appAPI(t, c, app, "/api/apps/v1/events/emit", map[string]any{"name": "ci.run", "arguments": json.RawMessage(`{"pr":1}`),
			"payload": json.RawMessage(`{"ok":true}`), "text": "CI passed"}, nil); code != 200 {
			t.Fatalf("emit: %d %s", code, e)
		}
	}
	first := sub(`{"pr":1.0}`)
	call["session"] = first.Session
	tenant := userTenant(t, c, "ci", "u1")
	emit()
	waitFor(t, "the emit of 1 delivered to the subscription made with 1.0", func() bool { return deliveryCount(t, c, tenant) == 1 })

	// As it was recorded before: on ci/1.0, where nothing is emitted.
	var subID string
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `UPDATE topic_subscriptions SET topic = 'ci/1.0' WHERE ended_at IS NULL RETURNING subscription_id::text`).Scan(&subID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE subscriptions SET filters = filters - '$topic:ci/1' || '{"$topic:ci/1.0":"1"}' WHERE id = $1`, subID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE topic_members SET topic = 'ci/1.0' WHERE subscription_id = $1`, subID); err != nil {
		t.Fatal(err)
	}
	if again := sub(`{"pr":1}`); again.EventSubscription.Created || again.EventSubscription.ID != first.EventSubscription.ID {
		t.Fatalf("1 and 1.0 are two subscriptions: %+v %+v", first, again)
	}
	var member string
	if err := c.DB.Pool.QueryRow(ctx, `SELECT topic FROM topic_members WHERE subscription_id = $1`, subID).Scan(&member); err != nil || member != "ci/1" {
		t.Fatalf("after its refresh the subscription is on %q (%v), want ci/1", member, err)
	}
	emit()
	waitFor(t, "the emit delivered after the refresh", func() bool { return deliveryCount(t, c, tenant) == 2 })
}

func deliveryCount(t *testing.T, c *coretest.Core, tenant string) int {
	t.Helper()
	var n int
	c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM deliveries`).Scan(&n)
	})
	return n
}

// Revoking (as the console does) ends delivery at once, tells the
// conversation, and is audited; a workspace can't see or revoke another's.
func TestRevokeEventSubscription(t *testing.T) {
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	var mine, theirs eventSubResult
	appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": map[string]any{"user": "u1", "client": "claude-code"},
		"name": "chat.message", "arguments": map[string]any{"channel": "eng"}}, &mine)
	appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": map[string]any{"user": "u2", "client": "claude-code"},
		"name": "chat.message", "arguments": map[string]any{"channel": "eng"}}, &theirs)
	u1, u2 := userTenant(t, c, "chat", "u1"), userTenant(t, c, "chat", "u2")

	// Another workspace's subscription: not listed, not revocable.
	if err := c.DB.InTenant(ctx, u1, func(tx pgx.Tx) error {
		list, err := appapi.ListEventSubscriptions(ctx, tx)
		if err != nil {
			return err
		}
		if len(list) != 1 || list[0].ID != mine.EventSubscription.ID || list[0].SourceName != "Chat" || list[0].Description != "A message in a channel." {
			t.Fatalf("u1's list: %+v", list)
		}
		_, err = appapi.RevokeEventSubscription(ctx, tx, theirs.EventSubscription.ID, audit.User("x"))
		if !errors.Is(err, appapi.ErrNoEventSubscription) {
			t.Fatalf("revoking another workspace's: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := c.DB.InTenant(ctx, u1, func(tx pgx.Tx) error {
		s, err := appapi.RevokeEventSubscription(ctx, tx, mine.EventSubscription.ID, audit.User("ada"))
		if err != nil || s.EndedReason != "revoked" {
			t.Fatalf("revoke: %+v %v", s, err)
		}
		_, err = appapi.RevokeEventSubscription(ctx, tx, mine.EventSubscription.ID, audit.User("ada"))
		if !errors.Is(err, appapi.ErrNoEventSubscription) {
			t.Fatalf("revoking twice: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Told: the conversation's notice is its one delivery.
	waitFor(t, "the notice", func() bool { return deliveryCount(t, c, u1) == 1 })
	var subject string
	c.DB.InTenant(ctx, u1, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT e.subject FROM deliveries d JOIN events e ON e.id = d.event_id`).Scan(&subject)
	})
	if !strings.Contains(subject, "it was revoked in Wakeshift") {
		t.Fatalf("notice: %q", subject)
	}
	appAPI(t, c, app, "/api/apps/v1/events/emit", map[string]any{"name": "chat.message", "arguments": map[string]any{"channel": "eng"},
		"payload": map[string]any{"text": "after", "from": "bob"}, "text": "after"}, nil)
	waitFor(t, "u2 gets it", func() bool { return deliveryCount(t, c, u2) == 1 })
	if n := deliveryCount(t, c, u1); n != 1 {
		t.Fatalf("a revoked subscription was delivered: %d", n)
	}
	var by string
	c.DB.InTenant(ctx, u1, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT actor FROM audit_log WHERE action = 'event_subscription.revoked'`).Scan(&by)
	})
	if by != "user:ada" {
		t.Fatalf("revocation's actor: %q", by)
	}
	// The app's list for u1 no longer has it.
	var list struct {
		Subscriptions []appapi.EventSubscription `json:"subscriptions"`
	}
	appAPI(t, c, app, "/api/apps/v1/event-subscriptions/list", map[string]any{"user": "u1"}, &list)
	if len(list.Subscriptions) != 0 {
		t.Fatalf("listed after revoking: %+v", list.Subscriptions)
	}
}

// A client that speaks MCP Events subscribes by webhook: one per
// (principal, callback, event, arguments), with a session of its own; a
// refresh keeps it; an emit reaches the callback; unsubscribing ends it.
func TestEventSubscriptionsNative(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	chatgpt := newFakeChatGPT(t)
	secret := chatgpt.secret()
	call := map[string]any{"user": "u1", "client": "openai-mcp", "user_agent": "openai-mcp/1.0.0"}
	subscribe := func(args map[string]any) (map[string]any, int, string) {
		var out map[string]any
		code, e := appAPI(t, c, app, "/api/apps/v1/events/subscribe", map[string]any{"call": call, "name": "chat.message", "arguments": args,
			"delivery": map[string]any{"mode": "webhook", "url": chatgpt.url(), "secret": secret}, "ttl_ms": 3600000}, &out)
		return out, code, e
	}
	res, code, e := subscribe(map[string]any{"channel": "eng"})
	if code != 200 || res["created"] != true || !strings.HasPrefix(res["id"].(string), "sub_") {
		t.Fatalf("subscribe: %d %s %+v", code, e, res)
	}
	again, _, _ := subscribe(map[string]any{"channel": "eng"})
	if again["id"] != res["id"] || again["created"] != false || len(chatgpt.verified) != 1 {
		t.Fatalf("refresh: %+v, verified %v", again, chatgpt.verified)
	}
	// Other arguments to the same callback: another subscription, not
	// verified again (SEP-3415 caches verification per (principal, url)).
	other, _, _ := subscribe(map[string]any{"channel": "ops"})
	if other["id"] == res["id"] || other["created"] != true || len(chatgpt.verified) != 1 {
		t.Fatalf("another subscription: %+v, verified %v", other, chatgpt.verified)
	}
	tenant := userTenant(t, c, "chat", "u1")
	var sessions int
	c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(DISTINCT session_id) FROM event_subscriptions WHERE destination = 'webhook' AND ended_at IS NULL`).Scan(&sessions)
	})
	if sessions != 2 {
		t.Fatalf("webhook subscriptions share a session: %d", sessions)
	}
	// Another client of the same user, with the same callback and
	// arguments, can't refresh (or end) the first client's subscription.
	call["client"], call["user_agent"] = "claude-ai", ""
	if _, code, e := subscribe(map[string]any{"channel": "eng"}); code != 403 || e != "other_client" {
		t.Fatalf("another client refreshing: %d %s", code, e)
	}
	appAPI(t, c, app, "/api/apps/v1/events/unsubscribe", map[string]any{"call": call, "name": "chat.message",
		"arguments": map[string]any{"channel": "eng"}, "delivery": map[string]any{"mode": "webhook", "url": chatgpt.url()}}, nil)
	call["client"], call["user_agent"] = "openai-mcp", "openai-mcp/1.0.0"
	if _, code, e := subscribe(map[string]any{"channel": "eng", "extra": true}); code != 400 || e != "bad_arguments" {
		t.Fatalf("bad arguments: %d %s", code, e)
	}
	delete(call, "user")
	if _, code, _ := subscribe(map[string]any{"channel": "eng"}); code != 400 {
		t.Fatalf("a webhook without a principal: %d", code)
	}
	call["user"] = "u1"

	appAPI(t, c, app, "/api/apps/v1/events/emit", map[string]any{"name": "chat.message", "arguments": map[string]any{"channel": "eng"},
		"payload": map[string]any{"text": "hi", "from": "ada"}, "text": "hi"}, nil)
	waitFor(t, "the event at the callback", func() bool { return len(chatgpt.got()) == 1 })
	if got := chatgpt.got()[0]; got.Sub != res["id"] {
		t.Fatalf("delivered to %s, want %s", got.Sub, res["id"])
	}

	if code, _ := appAPI(t, c, app, "/api/apps/v1/events/unsubscribe", map[string]any{"call": call, "name": "chat.message",
		"arguments": map[string]any{"channel": "eng"}, "delivery": map[string]any{"mode": "webhook", "url": chatgpt.url()}}, nil); code != 200 {
		t.Fatalf("unsubscribe: %d", code)
	}
	var live int
	c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM mcp_event_webhooks WHERE id = $1`, res["id"]).Scan(&live)
	})
	if live != 0 {
		t.Fatal("the webhook outlived its subscription")
	}
	appAPI(t, c, app, "/api/apps/v1/events/emit", map[string]any{"name": "chat.message", "arguments": map[string]any{"channel": "eng"},
		"payload": map[string]any{"text": "after", "from": "ada"}, "text": "after"}, nil)
	time.Sleep(300 * time.Millisecond)
	if n := len(chatgpt.got()); n != 1 {
		t.Fatalf("delivered after unsubscribing: %d", n)
	}
	if got := strings.Join(auditActions(t, c, tenant), ","); got != "event_subscription.created,event_subscription.created,event_subscription.unsubscribed" {
		t.Fatalf("audit: %s", got)
	}
}

// 045 and 047 end every workspace's wakeshift.message webhooks, and only
// those. The core no longer makes them, so they're made here from event
// type webhooks.
func TestMigrationEndsMessageWebhooks(t *testing.T) {
	for _, file := range []string{"045_event_subscriptions.sql", "047_end_message_webhooks.sql"} {
		t.Run(file, func(t *testing.T) {
			eventsTestSetup(t)
			c := coretest.Start(t)
			app := eventsApp(t, c, "chat")
			chatgpt := newFakeChatGPT(t)
			for _, sub := range []struct{ user, channel string }{{"u1", "eng"}, {"u1", "old"}, {"u2", "old"}} {
				if code, e := appAPI(t, c, app, "/api/apps/v1/events/subscribe", map[string]any{"call": map[string]any{"user": sub.user, "client": "openai-mcp"},
					"name": "chat.message", "arguments": map[string]any{"channel": sub.channel},
					"delivery": map[string]any{"mode": "webhook", "url": chatgpt.url() + "/" + sub.channel, "secret": chatgpt.secret()}}, nil); code != 200 {
					t.Fatalf("subscribe: %d %s", code, e)
				}
			}
			count := func(user, where string) (n int) {
				c.DB.InTenant(ctx, userTenant(t, c, "chat", user), func(tx pgx.Tx) error {
					return tx.QueryRow(ctx, `SELECT count(*) FROM mcp_event_webhooks WHERE `+where).Scan(&n)
				})
				return n
			}
			// The "old" ones become wakeshift.message webhooks, as #83 made them.
			for _, user := range []string{"u1", "u2"} {
				c.DB.InTenant(ctx, userTenant(t, c, "chat", user), func(tx pgx.Tx) error {
					_, err := tx.Exec(ctx, `UPDATE mcp_event_webhooks SET name = 'wakeshift.message' WHERE url LIKE '%/old'`)
					return err
				})
			}
			if count("u1", "name = 'wakeshift.message'") != 1 || count("u2", "name = 'wakeshift.message'") != 1 {
				t.Fatal("setup")
			}
			sql, err := os.ReadFile("../db/migrations/" + file)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.DB.Pool.Exec(ctx, string(sql)[strings.Index(string(sql), "DO $$"):]); err != nil {
				t.Fatal(err)
			}
			if count("u1", "name = 'wakeshift.message'") != 0 || count("u2", "name = 'wakeshift.message'") != 0 {
				t.Fatal("wakeshift.message webhooks survived")
			}
			if count("u1", "name = 'chat.message'") != 1 {
				t.Fatal("an event type's webhook was ended")
			}
			var targets int
			c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM mcp_event_targets`).Scan(&targets)
			if targets != 1 {
				t.Fatalf("targets left: %d, want the event type's one", targets)
			}
		})
	}
}

// A call without a user (a server without identify) lists, and ends by ID,
// the calling conversation's subscriptions: found by its session key, as
// an unsubscribe finds it, never another conversation's.
func TestEventSubscriptionsListByConversation(t *testing.T) {
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	subscribe := func(call map[string]any, channel string) eventSubResult {
		var out eventSubResult
		if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": call, "name": "chat.message",
			"arguments": map[string]any{"channel": channel}}, &out); code != 200 {
			t.Fatalf("subscribe: %d %s", code, e)
		}
		return out
	}
	type listed struct {
		Subscriptions []appapi.EventSubscription `json:"subscriptions"`
	}
	list := func(body map[string]any) listed {
		var out listed
		if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions/list", body, &out); code != 200 {
			t.Fatalf("list %v: %d %s", body, code, e)
		}
		return out
	}
	mine := map[string]any{"client": "claude-code"}
	first := subscribe(mine, "eng")
	mine["session"] = first.Session
	subscribe(mine, "ops")
	theirs := map[string]any{"client": "claude-code"}
	other := subscribe(theirs, "eng")
	theirs["session"] = other.Session

	if got := list(map[string]any{"call": mine}); len(got.Subscriptions) != 2 {
		t.Fatalf("the conversation's own list: %+v", got)
	}
	if got := list(map[string]any{"call": theirs}); len(got.Subscriptions) != 1 || got.Subscriptions[0].ID != other.EventSubscription.ID {
		t.Fatalf("the other conversation's list: %+v", got)
	}
	// No key, or one Wakeshift never made: nothing to list, and no error.
	for _, call := range []map[string]any{{"client": "claude-code"}, {"client": "claude-code", "session": "wsc_" + strings.Repeat("a", 32)}, {"client": "claude-code", "session": "made-up"}} {
		if got := list(map[string]any{"call": call}); len(got.Subscriptions) != 0 {
			t.Fatalf("call %v listed %+v", call, got)
		}
	}
	// Neither a user nor a call.
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions/list", map[string]any{}, nil); code != 400 || e != "bad_request" {
		t.Fatalf("an empty list request: %d %s", code, e)
	}

	// Ending by ID: another conversation's ID ends nothing; its own ends it.
	var ended struct{ Ended bool }
	if code, _ := appAPI(t, c, app, "/api/apps/v1/event-subscriptions/end", map[string]any{"call": mine, "id": other.EventSubscription.ID}, &ended); code != 200 || ended.Ended {
		t.Fatalf("ended another conversation's subscription: %d %+v", code, ended)
	}
	if code, _ := appAPI(t, c, app, "/api/apps/v1/event-subscriptions/end", map[string]any{"call": theirs, "id": other.EventSubscription.ID}, &ended); code != 200 || !ended.Ended {
		t.Fatalf("end its own: %d %+v", code, ended)
	}
	if got := list(map[string]any{"call": theirs}); len(got.Subscriptions) != 0 {
		t.Fatalf("after ending: %+v", got)
	}
	if got := list(map[string]any{"call": mine}); len(got.Subscriptions) != 2 {
		t.Fatalf("the first conversation's, after the other ended its own: %+v", got)
	}
}
