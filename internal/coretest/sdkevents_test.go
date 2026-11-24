package coretest_test

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// The Go SDK's event API end to end, against a real core: an app defines
// event types with wakeshift.DefineEvent, publishes them, and serves them
// with Attach behind its own authentication (here a bearer token
// that is the user's ID).

type chanArgs struct {
	Channel string `json:"channel" jsonschema:"The channel"`
}

type chanMsg struct {
	From string `json:"from"`
	Text string `json:"text"`
}

type tableArgs struct {
	Table string `json:"table" jsonschema:"Your table"`
}

type turn struct {
	ToCall string `json:"to_call"`
	Hand   string `json:"hand"` // only the player's own: a personal event
}

type sdkEventsApp struct {
	app     *wakeshift.Client
	message *wakeshift.Event[chanArgs, chanMsg]
	turn    *wakeshift.Event[tableArgs, turn]
	mcp     string // the app's MCP endpoint
}

func newSDKEventsApp(t *testing.T, c *coretest.Core) *sdkEventsApp {
	t.Helper()
	s := &sdkEventsApp{app: bareApp(t, c, "parlor")}
	var err error
	s.message, err = wakeshift.DefineEvent(s.app, "chat.message", wakeshift.EventDef[chanArgs, chanMsg]{
		Description: "A new message in a channel.",
		Authorize: func(_ context.Context, sub wakeshift.Subscriber, a chanArgs) (bool, error) {
			return a.Channel != "secret", nil
		},
		Text: func(m chanMsg, a chanArgs) string { return m.From + " in #" + a.Channel + ": " + m.Text },
	})
	if err != nil {
		t.Fatal(err)
	}
	s.turn, err = wakeshift.DefineEvent(s.app, "game.turn", wakeshift.EventDef[tableArgs, turn]{
		Description: "It's your move.", Personal: true,
		Text: func(tu turn, a tableArgs) string { return "Your move at " + a.Table + ": " + tu.ToCall + " to call" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.app.PublishManifest(ctx, wakeshift.Manifest{Description: "A parlor with chat and games."}); err != nil {
		t.Fatal(err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "parlor", Version: "1"}, nil)
	attached := s.app.Attach(srv, wakeshift.AttachOptions{Identify: identifyUser})
	s.mcp = serveAttached(t, srv, attached, tokenIsUser)
	return s
}

// call sends one JSON-RPC request to the app's MCP endpoint as user, with
// a User-Agent, and returns the result (or the error's code and message).
func (s *sdkEventsApp) call(t *testing.T, user, ua, method string, params any) (map[string]any, int, string) {
	t.Helper()
	raw, status := postRPC(t, s.mcp, bearer(user, ua), 1, method, params)
	var out struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: %d %s", method, status, raw)
	}
	if out.Error != nil {
		return nil, out.Error.Code, out.Error.Message
	}
	return out.Result, 0, ""
}

// tool calls one of the app's tools as user from Claude Code, returning
// its text and whether it's an error.
func (s *sdkEventsApp) tool(t *testing.T, user, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, code, msg := s.call(t, user, "claude-code/2.1.280", "tools/call", map[string]any{"name": name, "arguments": args})
	if code != 0 {
		t.Fatalf("%s: %d %s", name, code, msg)
	}
	var text strings.Builder
	for _, c := range res["content"].([]any) {
		text.WriteString(c.(map[string]any)["text"].(string))
	}
	isErr, _ := res["isError"].(bool)
	return text.String(), isErr
}

// A bridged client subscribes with subscribe_events and is woken by an
// emit; Authorize refuses before anything is recorded; list_subscriptions
// and unsubscribe_events (by event, or by id) work.
func TestSDKEventsBridged(t *testing.T) {
	c := coretest.Start(t)
	s := newSDKEventsApp(t, c)

	text, isErr := s.tool(t, "u1", "subscribe_events", map[string]any{"event": "chat.message", "arguments": map[string]any{"channel": "eng"}})
	if isErr || !strings.Contains(text, `Subscribed to chat.message {"channel":"eng"}.`) {
		t.Fatalf("subscribe_events: %v %s", isErr, text)
	}
	// The result carries the wake setup for Claude Code: how to be woken,
	// and a session key the agent passes back.
	session := regexp.MustCompile(`wsc_[A-Za-z0-9_-]+`).FindString(text)
	if !strings.Contains(text, "session_key") || session == "" {
		t.Fatalf("no wake setup in the result: %s", text)
	}
	if text, isErr := s.tool(t, "u1", "subscribe_events", map[string]any{"event": "chat.message", "arguments": map[string]any{"channel": "secret"}}); !isErr ||
		!strings.Contains(text, "You may not subscribe to chat.message with these arguments.") {
		t.Fatalf("Authorize didn't refuse: %v %s", isErr, text)
	}
	if text, isErr := s.tool(t, "u1", "subscribe_events", map[string]any{"event": "chat.nope", "arguments": map[string]any{}}); !isErr ||
		!strings.Contains(text, "This app offers no event named chat.nope.") {
		t.Fatalf("an unknown event: %v %s", isErr, text)
	}

	tenant := userTenant(t, c, "parlor", "u1")
	if got, err := s.message.Emit(ctx, chanArgs{Channel: "eng"}, chanMsg{From: "ada", Text: "shipped"}); err != nil || got.Matched != 1 {
		t.Fatalf("emit: %+v %v", got, err)
	}
	waitFor(t, "the message delivered to the conversation", func() bool { return deliveryCount(t, c, tenant) == 1 })
	if _, err := s.message.Emit(ctx, chanArgs{Channel: "ops"}, chanMsg{From: "ada", Text: "elsewhere"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if n := deliveryCount(t, c, tenant); n != 1 {
		t.Fatalf("another channel's message reached the conversation: %d", n)
	}

	list, _ := s.tool(t, "u1", "list_subscriptions", nil)
	if !strings.Contains(list, `chat.message {"channel":"eng"}, wakes a conversation`) {
		t.Fatalf("list_subscriptions: %s", list)
	}
	subs, err := s.app.EventSubscriptions(ctx, "u1")
	if err != nil || len(subs) != 1 {
		t.Fatalf("EventSubscriptions: %+v %v", subs, err)
	}
	if text, _ := s.tool(t, "u2", "list_subscriptions", nil); text != "You have no subscriptions to this app's events." {
		t.Fatalf("another user's list: %s", text)
	}
	// Someone else can't stop it by its id.
	if text, _ := s.tool(t, "u2", "unsubscribe_events", map[string]any{"id": subs[0].ID}); !strings.Contains(text, "No subscription of yours has the id") {
		t.Fatalf("u2 stopped u1's subscription: %s", text)
	}
	if text, isErr := s.tool(t, "u1", "unsubscribe_events", map[string]any{"event": "chat.message", "arguments": map[string]any{"channel": "eng"},
		"session_key": session}); isErr ||
		!strings.Contains(text, `Stopped chat.message {"channel":"eng"}.`) {
		t.Fatalf("unsubscribe_events: %v %s", isErr, text)
	}
	if text, _ := s.tool(t, "u1", "list_subscriptions", nil); text != "You have no subscriptions to this app's events." {
		t.Fatalf("after unsubscribing: %s", text)
	}
}

// A subscription to an event type the manifest no longer lists can still
// be ended, bridged or native.
func TestUnsubscribeRemovedEventType(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	s := newSDKEventsApp(t, c)
	text, _ := s.tool(t, "u1", "subscribe_events", map[string]any{"event": "chat.message", "arguments": map[string]any{"channel": "eng"}})
	session := regexp.MustCompile(`wsc_[A-Za-z0-9_-]+`).FindString(text)
	cb := newFakeChatGPT(t)
	native := map[string]any{"name": "chat.message", "arguments": map[string]any{"channel": "eng"},
		"delivery": map[string]any{"mode": "webhook", "url": cb.url(), "secret": cb.secret()}, "ttlMs": 3600000}
	if _, code, msg := s.call(t, "u1", draftUA, "events/subscribe", native); code != 0 {
		t.Fatalf("native subscribe: %d %s", code, msg)
	}
	if err := c.Apps.SetManifest(ctx, "parlor", apps.Manifest{Description: "No events now."}); err != nil {
		t.Fatal(err)
	}
	var ended struct{ Ended bool }
	if code, e := appAPI(t, c, s.app, "/api/apps/v1/event-subscriptions/unsubscribe", map[string]any{"call": map[string]any{"user": "u1",
		"client": "claude-code", "session": session}, "name": "chat.message", "arguments": map[string]any{"channel": "eng"}}, &ended); code != 200 || !ended.Ended {
		t.Fatalf("bridged unsubscribe of a removed event: %d %s %+v", code, e, ended)
	}
	if _, code, msg := s.call(t, "u1", draftUA, "events/unsubscribe", native); code != 0 {
		t.Fatalf("native unsubscribe of a removed event: %d %s", code, msg)
	}
	if subs, err := s.app.EventSubscriptions(ctx, "u1"); err != nil || len(subs) != 0 {
		t.Fatalf("left: %+v %v", subs, err)
	}
}

// A personal event's occurrence reaches only the user it's for, though two
// users subscribed with the same arguments; Emit needs To; the core
// refuses "to" on a shared event and a personal one without it.
func TestSDKEventsPersonal(t *testing.T) {
	c := coretest.Start(t)
	s := newSDKEventsApp(t, c)
	for _, u := range []string{"ada", "bob"} {
		if text, isErr := s.tool(t, u, "subscribe_events", map[string]any{"event": "game.turn", "arguments": map[string]any{"table": "t1"}}); isErr {
			t.Fatalf("%s subscribe: %s", u, text)
		}
	}
	ada, bob := userTenant(t, c, "parlor", "ada"), userTenant(t, c, "parlor", "bob")
	if got, err := s.turn.Emit(ctx, tableArgs{Table: "t1"}, turn{ToCall: "2", Hand: "As Kd"}, wakeshift.To("ada")); err != nil || got.Matched != 1 {
		t.Fatalf("emit to ada: %+v %v", got, err)
	}
	waitFor(t, "ada's turn", func() bool { return deliveryCount(t, c, ada) == 1 })
	time.Sleep(200 * time.Millisecond)
	if n := deliveryCount(t, c, bob); n != 0 {
		t.Fatalf("bob got ada's turn (%d deliveries)", n)
	}
	if _, err := s.turn.Emit(ctx, tableArgs{Table: "t1"}, turn{ToCall: "2"}); err == nil {
		t.Fatal("a personal emit without To")
	}
	for name, body := range map[string]map[string]any{
		"a personal event without to": {"name": "game.turn", "arguments": map[string]any{"table": "t1"}, "payload": map[string]any{"to_call": "2", "hand": "x"}, "text": "t"},
		"to on a shared event":        {"name": "chat.message", "arguments": map[string]any{"channel": "eng"}, "payload": map[string]any{"from": "a", "text": "b"}, "text": "t", "to": "ada"},
	} {
		if code, _ := appAPI(t, c, s.app, "/api/apps/v1/events/emit", body, nil); code != 400 {
			t.Errorf("%s: %d, want 400", name, code)
		}
	}
	// A personal subscription needs a user: one with none is refused.
	if code, _ := appAPI(t, c, s.app, "/api/apps/v1/event-subscriptions", map[string]any{"call": map[string]any{"client": "claude-code"},
		"name": "game.turn", "arguments": map[string]any{"table": "t1"}}, nil); code != 400 {
		t.Fatalf("a personal subscription with no user: %d", code)
	}
}

// A native client (a draft one) subscribes with events/subscribe through
// Attach: the event types are listed, Authorize refuses with
// Forbidden, an emit reaches its webhook under the event's own name with
// the payload untouched, and unsubscribe_events by id ends it (the
// receiver is told).
func TestSDKEventsNative(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	s := newSDKEventsApp(t, c)
	cb := newFakeChatGPT(t)

	list, code, _ := s.call(t, "u1", draftUA, "events/list", map[string]any{})
	if code != 0 || len(list["events"].([]any)) != 2 {
		t.Fatalf("events/list: %d %+v", code, list)
	}
	params := func(channel string) map[string]any {
		return map[string]any{"name": "chat.message", "arguments": map[string]any{"channel": channel},
			"delivery": map[string]any{"mode": "webhook", "url": cb.url(), "secret": cb.secret()}, "ttlMs": 3600000,
			"_meta": map[string]any{"io.modelcontextprotocol/clientInfo": map[string]any{"name": "acme-agent", "version": "2"}}}
	}
	if _, code, msg := s.call(t, "u1", draftUA, "events/subscribe", params("secret")); code != -32024 || !strings.Contains(msg, "you may not subscribe to chat.message") {
		t.Fatalf("Authorize: %d %s", code, msg)
	}
	res, code, msg := s.call(t, "u1", draftUA, "events/subscribe", params("eng"))
	if code != 0 || res["id"] == "" {
		t.Fatalf("subscribe: %d %s", code, msg)
	}
	if _, err := s.message.Emit(ctx, chanArgs{Channel: "eng"}, chanMsg{From: "ada", Text: "shipped"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the webhook delivery", func() bool { return len(cb.got()) == 1 })
	e := cb.got()[0]
	if e.Body.Name != "chat.message" || e.Body.Data["from"] != "ada" || e.Body.Data["text"] != "shipped" || len(e.Body.Data) != 2 {
		t.Fatalf("delivered: %+v", e.Body)
	}
	if line := e.Body.Meta["ai.wakeshift/text"]; !strings.Contains(line, "ada in #eng: shipped") {
		t.Fatalf("rendered line: %q", line)
	}
	// The user can stop it from any client, by its id.
	subs, err := s.app.EventSubscriptions(ctx, "u1")
	if err != nil || len(subs) != 1 || subs[0].Destination != "webhook" {
		t.Fatalf("EventSubscriptions: %+v %v", subs, err)
	}
	if text, isErr := s.tool(t, "u1", "unsubscribe_events", map[string]any{"id": subs[0].ID}); isErr || !strings.Contains(text, "Stopped subscription") {
		t.Fatalf("unsubscribe_events by id: %v %s", isErr, text)
	}
	waitFor(t, "the receiver told", func() bool {
		for _, env := range cb.envelopes() {
			if env.Type == "terminated" {
				return true
			}
		}
		return false
	})
	if _, err := s.message.Emit(ctx, chanArgs{Channel: "eng"}, chanMsg{From: "ada", Text: "after"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(cb.got()); n != 1 {
		t.Fatalf("delivered after it ended: %d", n)
	}
}
