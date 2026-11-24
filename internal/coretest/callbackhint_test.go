package coretest_test

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/mcpevents"
)

// A webhook events/subscribe refused because its callback can't be reached
// (not https, not a public address, a failed verification) keeps its code
// and reason and adds a hint: poll always, and the plugin, with the docs
// that install it, for an agent Wakeshift installs into. The callback, its
// host and its address never appear in the answer.
func TestUnreachableCallbackHint(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.StartWith(t, func(o *core.Options) { o.SiteURL = "https://wakeshift.example" })
	app := eventsApp(t, c, "chat")
	hook := newVoucherHook(t)
	hook.set("e404", 0, 404)
	hook.set("e500", 0, 500)
	hook.set("noecho", 0, 204) // answers, without the challenge

	subscribe := func(client, cb, secret string) (int, map[string]any, string) {
		t.Helper()
		code, raw, err := appPostRaw(c, app, "/api/apps/v1/events/subscribe", map[string]any{"call": map[string]any{"user": "u1", "client": client},
			"name": "chat.message", "arguments": eng, "delivery": delivery(cb, secret), "ttl_ms": 3600000})
		if err != nil {
			t.Fatal(err)
		}
		var out struct{ Error map[string]any }
		json.Unmarshal(raw, &out)
		return code, out.Error, string(raw)
	}
	noCallback := func(name, raw, cb string) {
		t.Helper()
		u, _ := url.Parse(cb)
		for _, s := range []string{cb, u.Host, u.Hostname(), u.Path} {
			if s != "" && s != "/" && strings.Contains(raw, s) {
				t.Errorf("%s: the answer repeats %q: %s", name, s, raw)
			}
		}
	}
	// ChatGPT, OpenClaw and Hermes Agent take events by webhook alone: never
	// poll or push. An unknown client may poll or push (this core serves
	// push: coretest turns it on); Claude Code has a plugin and may too.
	const hermes = `{"alternatives":["plugin"],"install":"https://wakeshift.example/docs/hermes","kind":"callback_unreachable"}`
	const openclaw = `{"alternatives":["plugin"],"install":"https://wakeshift.example/docs/agents#the-one-time-install","kind":"callback_unreachable"}`
	const claudeCode = `{"alternatives":["plugin","poll","push"],"install":"https://wakeshift.example/docs/agents#the-one-time-install","kind":"callback_unreachable"}`
	const none = `{"alternatives":[],"kind":"callback_unreachable"}`
	const pollPush = `{"alternatives":["poll","push"],"kind":"callback_unreachable"}`
	const helpPlugin = ". This client needs a public HTTPS callback for webhook delivery. To be woken without one, install Wakeshift's plugin for Hermes Agent (https://wakeshift.example/docs/hermes)."
	const helpNone = ". This client needs a public HTTPS callback for webhook delivery."
	const helpPoll = ". This client needs a public HTTPS callback for webhook delivery. Without one, use poll or push if the client supports them."

	for _, tc := range []struct {
		name, client, cb string
		loopback         bool
		code, reason     string
		hint, message    string
	}{
		{"not https", "hermes-agent", "http://agent.example/hooks/abc123", false, "bad_request", "", hermes, "delivery.url must be https" + helpPlugin},
		{"not a URL", "openai-mcp", "agent.example/hooks/abc123", false, "bad_request", "", none, "delivery.url must be an absolute https URL" + helpNone},
		{"a private address", "hermes-agent", "https://10.1.2.3/hooks/abc123", false, "callback_failed", "connection_refused", hermes,
			"callback verification failed: the callback's address isn't public" + helpPlugin},
		{"the metadata address", "openclaw", "https://169.254.169.254/latest", false, "callback_failed", "connection_refused", openclaw, ""},
		{"loopback", "openai-mcp", "https://127.0.0.1:9/hooks/abc123", false, "callback_failed", "connection_refused", none,
			"callback verification failed: the callback's address isn't public" + helpNone},
		{"a 404", "hermes-agent", hook.url("e404"), true, "callback_failed", "http_4xx", hermes, "callback verification failed: the callback answered HTTP 404" + helpPlugin},
		{"a 500", "openai-mcp", hook.url("e500"), true, "callback_failed", "http_5xx", none, "callback verification failed: the callback answered HTTP 500" + helpNone},
		{"no echo", "some-draft-client", hook.url("noecho"), true, "callback_failed", "challenge_failed", pollPush,
			"callback verification failed: the callback didn't echo the challenge" + helpPoll},
		// What it installs is named as this Wakeshift offers it (KitFor:
		// a plugin from the marketplace, or hooks here).
		{"a 500 to Claude Code", "claude-code", hook.url("e500"), true, "callback_failed", "http_5xx", claudeCode, ""},
	} {
		mcpevents.AllowLoopback = tc.loopback
		code, e, raw := subscribe(tc.client, tc.cb, voucherSecret())
		if code != 400 || e["code"] != tc.code {
			t.Fatalf("%s: %d %s", tc.name, code, raw)
		}
		if r, _ := e["reason"].(string); r != tc.reason {
			t.Errorf("%s: reason %q, want %q", tc.name, r, tc.reason)
		}
		if h, _ := json.Marshal(e["hint"]); string(h) != tc.hint {
			t.Errorf("%s: hint %s, want %s", tc.name, h, tc.hint)
		}
		if tc.message != "" && e["message"] != tc.message {
			t.Errorf("%s: message %q, want %q", tc.name, e["message"], tc.message)
		}
		msg, _ := e["message"].(string)
		for _, w := range []string{"again", "retry"} {
			if strings.Contains(strings.ToLower(msg), w) {
				t.Errorf("%s: the message says %q: %s", tc.name, w, msg)
			}
		}
		noCallback(tc.name, raw, tc.cb)
		t.Logf("%s (%s): %s", tc.name, tc.client, raw)
	}
	mcpevents.AllowLoopback = true

	// No hint for anything else: a bad secret, a missing user, a callback
	// URL too long, an unknown event.
	for _, tc := range []struct {
		name string
		body map[string]any
		code string
	}{
		{"a bad secret", map[string]any{"call": map[string]any{"user": "u1", "client": "hermes-agent"}, "name": "chat.message", "arguments": eng,
			"delivery": delivery(hook.url("ok"), "whsec_short")}, "bad_request"},
		{"no user", map[string]any{"call": map[string]any{"client": "hermes-agent"}, "name": "chat.message", "arguments": eng,
			"delivery": delivery(hook.url("ok"), voucherSecret())}, "bad_request"},
		{"too long", map[string]any{"call": map[string]any{"user": "u1", "client": "hermes-agent"}, "name": "chat.message", "arguments": eng,
			"delivery": delivery("https://agent.example/"+strings.Repeat("a", 2000), voucherSecret())}, "bad_request"},
		{"an unknown event", map[string]any{"call": map[string]any{"user": "u1", "client": "hermes-agent"}, "name": "chat.nope", "arguments": eng,
			"delivery": delivery("http://agent.example/x", voucherSecret())}, "unknown_event"},
	} {
		code, raw, err := appPostRaw(c, app, "/api/apps/v1/events/subscribe", tc.body)
		if err != nil {
			t.Fatal(err)
		}
		if code/100 != 4 || !strings.Contains(string(raw), `"code":"`+tc.code+`"`) || strings.Contains(string(raw), "hint") || strings.Contains(string(raw), "public HTTPS callback") {
			t.Errorf("%s: %d %s", tc.name, code, raw)
		}
	}

	// Another client's subscription to the same callback: Forbidden
	// (other_client), no hint.
	if code, _, raw := subscribe("openai-mcp", hook.url("ok"), voucherSecret()); code != 200 {
		t.Fatalf("subscribe: %d %s", code, raw)
	}
	code, raw, err := appPostRaw(c, app, "/api/apps/v1/events/subscribe", map[string]any{"call": map[string]any{"user": "u1", "client": "hermes-agent"},
		"name": "chat.message", "arguments": eng, "delivery": delivery(hook.url("ok"), voucherSecret()), "ttl_ms": 3600000})
	if err != nil {
		t.Fatal(err)
	}
	if code != 403 || strings.Contains(string(raw), "hint") {
		t.Errorf("another client's: %d %s", code, raw)
	}
}

// Without a site, there's no install link, so no plugin; Hermes Agent
// can't poll, so nothing else either.
func TestUnreachableCallbackHintNoSite(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	mcpevents.AllowLoopback = false
	code, raw, err := appPostRaw(c, app, "/api/apps/v1/events/subscribe", map[string]any{"call": map[string]any{"user": "u1", "client": "hermes-agent"},
		"name": "chat.message", "arguments": eng, "delivery": delivery("https://192.168.1.20/hook", voucherSecret())})
	if err != nil {
		t.Fatal(err)
	}
	if code != 400 || !strings.Contains(string(raw), `"hint":{"alternatives":[],"kind":"callback_unreachable"}`) || strings.Contains(string(raw), "192.168") {
		t.Fatalf("%d %s", code, raw)
	}
}

// A callback whose address isn't public, given as an IP or as a name that
// resolves only to such addresses, is refused before anything is sent:
// the callback client's dialer checks each resolved address before it
// connects (mcpevents.Client), so no connection reaches the listener, and
// the refusal has the hint.
func TestUnreachableCallbackNothingSent(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var accepted atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			conn.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	mcpevents.AllowLoopback = false
	for _, cb := range []string{
		fmt.Sprintf("https://127.0.0.1:%d/hook", port), // an IP that isn't public
		fmt.Sprintf("https://localhost:%d/hook", port), // a name that resolves only to loopback
	} {
		code, raw, err := appPostRaw(c, app, "/api/apps/v1/events/subscribe", map[string]any{"call": map[string]any{"user": "u1", "client": "openai-mcp"},
			"name": "chat.message", "arguments": eng, "delivery": delivery(cb, voucherSecret())})
		if err != nil {
			t.Fatal(err)
		}
		if code != 400 || !strings.Contains(string(raw), `"reason":"connection_refused"`) ||
			!strings.Contains(string(raw), `"hint":{"alternatives":[],"kind":"callback_unreachable"}`) ||
			strings.Contains(string(raw), "localhost") || strings.Contains(string(raw), "127.0.0.1") {
			t.Fatalf("%s: %d %s", cb, code, raw)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := accepted.Load(); n != 0 {
		t.Fatalf("%d connections reached the listener", n)
	}
}

// With push off (WAKESHIFT_MCP_EVENTS_PUSH=0), the hint and the message
// offer poll alone; a client that takes webhooks alone still gets neither.
func TestUnreachableCallbackHintPushOff(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.StartWith(t, func(o *core.Options) { o.SiteURL = "https://wakeshift.example"; o.MCPEventsPush = false })
	app := eventsApp(t, c, "chat")
	mcpevents.AllowLoopback = false
	for _, tc := range []struct{ client, hint, help string }{
		{"some-draft-client", `{"alternatives":["poll"],"kind":"callback_unreachable"}`, "Without one, use poll if the client supports it."},
		{"claude-code", `{"alternatives":["plugin","poll"],"install":"https://wakeshift.example/docs/agents#the-one-time-install","kind":"callback_unreachable"}`,
			"(https://wakeshift.example/docs/agents#the-one-time-install), or use poll if the client supports it."},
		{"openai-mcp", `{"alternatives":[],"kind":"callback_unreachable"}`, "This client needs a public HTTPS callback for webhook delivery."},
	} {
		code, raw, err := appPostRaw(c, app, "/api/apps/v1/events/subscribe", map[string]any{"call": map[string]any{"user": "u1", "client": tc.client},
			"name": "chat.message", "arguments": eng, "delivery": delivery("https://10.9.8.7/hook", voucherSecret())})
		if err != nil {
			t.Fatal(err)
		}
		var out struct{ Error map[string]any }
		json.Unmarshal(raw, &out)
		h, _ := json.Marshal(out.Error["hint"])
		msg, _ := out.Error["message"].(string)
		if code != 400 || string(h) != tc.hint || !strings.HasSuffix(msg, tc.help) || strings.Contains(msg, "push") {
			t.Errorf("%s: %d %s", tc.client, code, raw)
		}
	}
}
