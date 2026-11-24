package coretest_test

import (
	"strings"
	"testing"

	"github.com/wakeshift/server/internal/coretest"
)

// A native client is told which events to subscribe to itself, or that it
// has: one line per event and arguments, worded by whether its user has a
// live native subscription to it.
func TestCallsAdditionNativeLines(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	chatgpt := newFakeChatGPT(t)
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/subscribe", map[string]any{"call": map[string]any{"user": "u3", "client": "openai-mcp"},
		"name": "game.turn", "arguments": map[string]any{"table": "juniper"}, "delivery": map[string]any{"mode": "webhook", "url": chatgpt.url(), "secret": chatgpt.secret()},
		"ttl_ms": 3600000}, nil); code != 200 {
		t.Fatalf("u3 subscribes natively: %d %s", code, e)
	}
	lines := func(user string) string {
		t.Helper()
		var block additionOut
		if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": map[string]any{"user": user, "client": "openai-mcp"},
			"native": []any{map[string]any{"name": "game.turn", "arguments": map[string]any{"table": "juniper"}}}}, &block); code != 200 {
			t.Fatalf("addition for %s: %d %s", user, code, e)
		}
		return block.Text
	}
	if got := lines("u3"); !strings.HasPrefix(got, `You're subscribed to game.turn with {"table":"juniper"}`) {
		t.Fatalf("u3: %q", got)
	}
	if got := lines("u4"); !strings.HasPrefix(got, `To be woken by game.turn with {"table":"juniper"}, subscribe to it`) {
		t.Fatalf("u4: %q", got)
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": map[string]any{"user": "u4"},
		"native": []any{map[string]any{"name": "no.such", "arguments": map[string]any{}}}}, nil); code != 404 || e != "unknown_event" {
		t.Fatalf("an unknown event: %d %s", code, e)
	}
}
