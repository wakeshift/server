package coretest_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/coretest"
)

// turnEvents is chatEvents plus a personal event, game.turn {table}: each
// subscription belongs to its subscriber, and an occurrence (carrying that
// subscriber's own cards) is emitted to one of them.
func turnEvents() apps.Manifest {
	m := chatEvents()
	m.Events = append(m.Events, apps.EventType{
		Name:        "game.turn",
		Description: "It's your move.",
		Arguments:   json.RawMessage(`{"type":"object","properties":{"table":{"type":"string"}},"required":["table"],"additionalProperties":false}`),
		Payload:     json.RawMessage(`{"type":"object","properties":{"hole":{"type":"string"},"turn":{"type":"string"}},"required":["hole","turn"]}`),
		Topic:       "tables/{table}/turns",
		Personal:    true,
	})
	return m
}

// subjects is what a workspace's conversations were delivered.
func subjects(t *testing.T, c *coretest.Core, tenant string) []string {
	t.Helper()
	var out []string
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT e.subject FROM deliveries d JOIN events e ON e.id = d.event_id ORDER BY d.created_at`)
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

// A personal event's occurrence reaches only the subscriber it's emitted
// to, on the bridged and the native path: two players subscribed to the
// same table with the same arguments never see each other's turns.
func TestPersonalEvents(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := bareApp(t, c, "poker")
	if err := c.Apps.SetManifest(ctx, "poker", turnEvents()); err != nil {
		t.Fatal(err)
	}
	table := map[string]any{"table": "juniper"}
	bridged := func(user string) (eventSubResult, int, string) {
		var out eventSubResult
		code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": map[string]any{"user": user, "client": "claude-code"},
			"name": "game.turn", "arguments": table}, &out)
		return out, code, e
	}
	if _, code, _ := bridged("u1"); code != 200 {
		t.Fatalf("u1 subscribes: %d", code)
	}
	if _, code, _ := bridged("u2"); code != 200 {
		t.Fatalf("u2 subscribes: %d", code)
	}
	// No user: nobody to emit to.
	if _, code, _ := bridged(""); code != 400 {
		t.Fatalf("a personal subscription without a user: %d", code)
	}
	// u3 subscribes natively (a webhook), the same table.
	chatgpt := newFakeChatGPT(t)
	var native map[string]any
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/subscribe", map[string]any{"call": map[string]any{"user": "u3", "client": "openai-mcp"},
		"name": "game.turn", "arguments": table, "delivery": map[string]any{"mode": "webhook", "url": chatgpt.url(), "secret": chatgpt.secret()},
		"ttl_ms": 3600000}, &native); code != 200 {
		t.Fatalf("u3 subscribes natively: %d %s", code, e)
	}

	emit := func(to, card string) (int, string) {
		body := map[string]any{"name": "game.turn", "arguments": table, "payload": map[string]any{"hole": card, "turn": "t-" + card},
			"text": "Your move, cards " + card}
		if to != "" {
			body["to"] = to
		}
		return appAPI(t, c, app, "/api/apps/v1/events/emit", body, nil)
	}
	if code, e := emit("", "As"); code != 400 || e != "bad_arguments" {
		t.Fatalf("a personal emit without to: %d %s", code, e)
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/emit", map[string]any{"name": "chat.message", "arguments": map[string]any{"channel": "eng"},
		"payload": map[string]any{"text": "hi", "from": "ada"}, "text": "hi", "to": "u1"}, nil); code != 400 || e != "bad_arguments" {
		t.Fatalf("to on a shared event: %d %s", code, e)
	}
	if code, _ := emit("u1", "As"); code != 200 {
		t.Fatal("emit to u1")
	}
	if code, _ := emit("u3", "Kd"); code != 200 {
		t.Fatal("emit to u3")
	}
	u1, u2 := userTenant(t, c, "poker", "u1"), userTenant(t, c, "poker", "u2")
	waitFor(t, "u1's turn", func() bool { return len(subjects(t, c, u1)) == 1 })
	waitFor(t, "u3's turn at the webhook", func() bool { return len(chatgpt.got()) == 1 })
	time.Sleep(300 * time.Millisecond)
	if got := subjects(t, c, u1); len(got) != 1 || !strings.Contains(got[0], "As") {
		t.Fatalf("u1 got %q", got)
	}
	if got := subjects(t, c, u2); len(got) != 0 {
		t.Fatalf("u2 got someone else's turn: %q", got)
	}
	if got := chatgpt.got(); len(got) != 1 || got[0].Body.Data["hole"] != "Kd" || !strings.Contains(got[0].Body.Meta["ai.wakeshift/text"], "Kd") {
		t.Fatalf("u3's webhook got %d, want only its own turn", len(got))
	}
	// An emit to someone not subscribed reaches nobody.
	if code, _ := emit("u9", "2c"); code != 200 {
		t.Fatal("emit to u9")
	}
	time.Sleep(300 * time.Millisecond)
	if len(subjects(t, c, u1)) != 1 || len(subjects(t, c, u2)) != 0 || len(chatgpt.got()) != 1 {
		t.Fatal("an emit to an unsubscribed user was delivered")
	}
}

// A shared event's arguments can't render its topic into the private space
// where personal events go: {a} = "u", {b} = "p<hash of u1>" would
// otherwise subscribe u2 to u1's turns.
func TestSharedEventCantReachPersonalTopics(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := bareApp(t, c, "poker")
	m := turnEvents()
	m.Events = append(m.Events, apps.EventType{
		Name:        "path.watch",
		Description: "Anything under a path.",
		Arguments: json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"},"c":{"type":"string"},"d":{"type":"string"},"e":{"type":"string"}},` +
			`"required":["a","b","c","d","e"],"additionalProperties":false}`),
		Payload: json.RawMessage(`{"type":"object"}`),
		Topic:   "{a}/{b}/{c}/{d}/{e}",
	})
	if err := c.Apps.SetManifest(ctx, "poker", m); err != nil {
		t.Fatal(err)
	}
	victim := apps.PersonalTopic("tables/juniper/turns", "u1") // u/p<hash>/tables/juniper/turns
	parts := strings.Split(victim, "/")
	args := map[string]any{"a": parts[0], "b": parts[1], "c": parts[2], "d": parts[3], "e": parts[4]}
	code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": map[string]any{"user": "u2", "client": "claude-code"},
		"name": "path.watch", "arguments": args}, nil)
	if code != 400 {
		t.Fatalf("subscribing to u1's personal topic through a shared event: %d %s", code, e)
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/emit", map[string]any{"name": "path.watch", "arguments": args, "payload": map[string]any{},
		"text": "into u1's turns"}, nil); code != 400 || e != "bad_arguments" {
		t.Fatalf("emitting a shared event into a personal topic: %d %s", code, e)
	}
	// Other values still work.
	args["a"] = "x"
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": map[string]any{"user": "u2", "client": "claude-code"},
		"name": "path.watch", "arguments": args}, nil); code != 200 {
		t.Fatalf("an ordinary subscription: %d %s", code, e)
	}
}

// An app's user ends one of their subscriptions by its ID (a headless
// app's tools do this for them): any destination; another user's ID, or
// another app's, names nothing.
func TestEndEventSubscriptionByID(t *testing.T) {
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	other := eventsApp(t, c, "other")
	var mine, theirs eventSubResult
	appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": map[string]any{"user": "u1", "client": "claude-code"},
		"name": "chat.message", "arguments": map[string]any{"channel": "eng"}}, &mine)
	appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": map[string]any{"user": "u2", "client": "claude-code"},
		"name": "chat.message", "arguments": map[string]any{"channel": "eng"}}, &theirs)
	end := func(user, id string) bool {
		t.Helper()
		var out struct{ Ended bool }
		if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions/end", map[string]any{"user": user, "id": id}, &out); code != 200 {
			t.Fatalf("end: %d %s", code, e)
		}
		return out.Ended
	}
	if end("u1", theirs.EventSubscription.ID) {
		t.Fatal("u1 ended u2's subscription")
	}
	if end("u1", "not-a-uuid") {
		t.Fatal("ended a malformed ID")
	}
	var out struct{ Ended bool }
	if code, _ := appAPI(t, c, other, "/api/apps/v1/event-subscriptions/end", map[string]any{"user": "u1", "id": mine.EventSubscription.ID}, &out); code != 200 || out.Ended {
		t.Fatalf("another app ended it: %d %+v", code, out)
	}
	if !end("u1", mine.EventSubscription.ID) {
		t.Fatal("u1 couldn't end its own")
	}
	if end("u1", mine.EventSubscription.ID) {
		t.Fatal("ended twice")
	}
	var list struct {
		Subscriptions []appapi.EventSubscription `json:"subscriptions"`
	}
	appAPI(t, c, app, "/api/apps/v1/event-subscriptions/list", map[string]any{"user": "u2"}, &list)
	if len(list.Subscriptions) != 1 {
		t.Fatalf("u2's subscription went too: %+v", list.Subscriptions)
	}
	if got := strings.Join(auditActions(t, c, userTenant(t, c, "chat", "u1")), ","); got != "event_subscription.created,event_subscription.unsubscribed" {
		t.Fatalf("audit: %s", got)
	}
}
