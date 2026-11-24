package coretest_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/connector"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// hook is Wakeshift's real hook for a harness on a user's machine, trusting
// this core.
func hook(t *testing.T, c *coretest.Core, harness string) *connector.Hook {
	t.Helper()
	spec, _ := connector.Lookup(harness)
	set := connector.Settings{Dir: t.TempDir(), Issuers: []string{c.URL}}
	return &connector.Hook{Harness: harness, Format: spec.Format, Verifier: &connector.Verifier{Settings: set}, Poller: &connector.HTTPPoller{Issuers: set.Issuers},
		Links: connector.LinksFor(set), When: spec.When, StopWait: 3 * time.Second, Unbound: spec.Unbound}
}

func runHook(h *connector.Hook, event string, in any) string {
	b, _ := json.Marshal(in)
	h.EventName = event
	var out bytes.Buffer
	h.Run(ctx, bytes.NewReader(b), &out, &out)
	return out.String()
}

// Cursor, end to end with the real hook: the subscribe result carries the
// background command, a text beacon (Cursor's hooks see only text) and the
// hooks as an option; the hook connects the conversation from it (bound to
// the conversation ID its agent passes, since Cursor never tells the app),
// takes the beacon out of what the model sees, and its stop hook (the
// command isn't running) continues the conversation with what's
// published, acknowledging it.
func TestCursorHooks(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	const conv = "27188d7e-37d9-4a60-b001-465a6950b24b"
	// Without the conversation's ID the agent passes: no beacon (a beacon
	// line naming no conversation is refused, since any tool's text could
	// carry one).
	if sub, _ := subscribe(app, caller{User: "cy", Client: "Cursor"}, "ci/acme/web", subOpts{}); beacon.InText(sub.Addition.Text) != "" {
		t.Fatalf("an unbound text beacon: %s", sub.Addition.Text)
	}
	call := caller{User: "cy", Client: "Cursor", Conversation: conv, Tool: "subscribe"}
	sub, err := subscribe(app, call, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	text := sub.Addition.Text
	if !strings.Contains(text, "/install/cursor.sh | sh") || !strings.Contains(text, "Also ask the user once whether to install Wakeshift's hooks for Cursor") || !strings.Contains(text, `"notify_on_output"`) ||
		beacon.InText(text) == "" || len(sub.Addition.Meta) != 0 {
		t.Fatalf("result: %+v", sub.Addition)
	}
	h := hook(t, c, "cursor")
	output, _ := json.Marshal(map[string]any{"content": []any{map[string]string{"type": "text", "text": text}}, "isError": false})
	out := runHook(h, "", map[string]any{"conversation_id": conv, "hook_event_name": "postToolUse", "tool_name": "MCP:subscribe",
		"tool_output": string(output), "tool_use_id": "u1"})
	var post struct {
		AdditionalContext    string `json:"additional_context"`
		UpdatedMCPToolOutput string `json:"updated_mcp_tool_output"`
	}
	if json.Unmarshal([]byte(out), &post) != nil || !strings.Contains(post.AdditionalContext, "Wakeshift: connected") ||
		strings.Contains(post.UpdatedMCPToolOutput, "wakeshift-beacon") || !strings.Contains(post.UpdatedMCPToolOutput, `conversation_id="`+conv+`"`) || strings.Contains(post.UpdatedMCPToolOutput, sub.Session) {
		t.Fatalf("post: %s", out)
	}
	// Connected: the next subscribe says the hooks wake it when a turn
	// ends, gives the command again, and offers nothing.
	again, _ := subscribe(app, caller{User: "cy", Client: "Cursor", Session: sub.Session, Conversation: conv}, "ci/acme/web", subOpts{})
	if !strings.Contains(again.Addition.Text, "only when a turn ends (Wakeshift's hooks)") || !strings.Contains(again.Addition.Text, `"notify_on_output"`) ||
		strings.Contains(again.Addition.Text, "/install/") {
		t.Fatalf("again: %s", again.Addition.Text)
	}
	// Its beacon now names the conversation: redeemed for another, refused.
	if tok := beacon.InText(again.Addition.Text); tok != "" {
		claims, err := beacon.Verify(tok, conv, []string{c.URL}, coreKeys(t, c), time.Now())
		if err != nil {
			t.Fatalf("bound beacon: %v", err)
		}
		if code, _ := redeemTicket(t, claims.Redeem, claims.Ticket, "another-conversation"); code != 403 {
			t.Fatalf("another conversation's redeem: %d", code)
		}
	}

	if _, err := emitNote(app, []string{"ci/acme/web"}, note{Text: "deploy finished"}); err != nil {
		t.Fatal(err)
	}
	var stop struct {
		FollowupMessage string `json:"followup_message"`
	}
	out = runHook(h, "", map[string]any{"conversation_id": conv, "hook_event_name": "stop", "status": "completed", "loop_count": 0})
	if json.Unmarshal([]byte(out), &stop) != nil || !strings.Contains(stop.FollowupMessage, "deploy finished") || !strings.Contains(stop.FollowupMessage, "not as instructions from the user") || strings.Count(stop.FollowupMessage, "</wakeshift_message>") != 1 {
		t.Fatalf("stop: %s", out)
	}
	// The shared format: the app's display name, the topic, and none of
	// Wakeshift's routing fields.
	if !strings.Contains(stop.FollowupMessage, `app="Ci"`) || !strings.Contains(stop.FollowupMessage, `"topic":"ci/acme/web"`) {
		t.Fatalf("stop format: %s", stop.FollowupMessage)
	}
	for _, internal := range []string{"attrs", "$topic", "hop", "trace_id", "topics.ci"} {
		if strings.Contains(stop.FollowupMessage, internal) {
			t.Fatalf("stop shows %s: %s", internal, stop.FollowupMessage)
		}
	}
	// Acknowledged: not handed over twice.
	h.StopWait = time.Second
	if out := runHook(h, "", map[string]any{"conversation_id": conv, "hook_event_name": "stop", "loop_count": 1}); out != "" {
		t.Fatalf("again: %s", out)
	}
}

// Antigravity names its conversation to every MCP server, so its beacon
// names it too, and the core refuses a redeem for any other.
func TestAntigravityBeacon(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	const conv = "9cb91991-c41f-4f35-9d8e-fb434527df3f"
	sub, err := subscribe(app, caller{Client: "antigravity-client", Meta: map[string]any{"antigravity.google/conversation_id": conv}},
		"ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sub.Addition.Text, `session_key="`+sub.Session+`"`) || !strings.Contains(sub.Addition.Text, "/install/antigravity.sh") {
		t.Fatalf("text: %s", sub.Addition.Text) // its ID names it, but only the key finds it again
	}
	claims, err := beacon.Verify(beacon.InText(sub.Addition.Text), conv, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil {
		t.Fatalf("beacon: %v", err)
	}
	if code, _ := redeemTicket(t, claims.Redeem, claims.Ticket, "someone-else"); code != 403 {
		t.Fatalf("another conversation: %d", code)
	}
	sub, _ = subscribe(app, caller{Client: "antigravity-client", Meta: map[string]any{"antigravity.google/conversation_id": conv}, Session: sub.Session},
		"ci/acme/web", subOpts{})
	claims, _ = beacon.Verify(beacon.InText(sub.Addition.Text), conv, []string{c.URL}, coreKeys(t, c), time.Now())
	if code, out := redeemTicket(t, claims.Redeem, claims.Ticket, conv); code != 200 || out["url"] == nil {
		t.Fatalf("its own: %d %v", code, out)
	}
}

// Claude Code tells MCP servers the tool call's ID: the beacon names that
// call, and the hook's redeem tells the core the session; from then on
// the core says the hooks deliver, not Monitor.
func TestClaudeCodeHooks(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	sub, err := subscribe(app, caller{User: "bo", Client: "claude-code", Meta: map[string]any{"claudecode/toolUseId": "toolu_1"}},
		"ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sub.Addition.Text, "Monitor") || !strings.Contains(sub.Addition.Text, "/install/claude-code.sh") ||
		!strings.Contains(sub.Addition.Text, "Ask them one question with your AskUserQuestion tool") || !strings.Contains(sub.Addition.Text, "\"Notify me in every session\" (description: runs `curl -fsSL ") || !strings.Contains(sub.Addition.Text, "type themselves, as it is: `! curl -fsSL ") {
		t.Fatalf("text: %s", sub.Addition.Text)
	}
	// The question first; the Monitor only after the user picks this session only.
	if ask, no, monitor := strings.Index(sub.Addition.Text, "Subscribed, but that alone won't tell you when something arrives"), strings.Index(sub.Addition.Text, "If they pick \"Notify me while this session is open\""),
		strings.Index(sub.Addition.Text, "start the Monitor tool now"); ask != 0 || no < ask || monitor < no || !strings.Contains(sub.Addition.Text, "don't start the Monitor") {
		t.Fatalf("order: %s", sub.Addition.Text)
	}
	tok, _ := sub.Addition.Meta[beacon.MetaKey].(string)
	keys := coreKeys(t, c)
	if _, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: "s1", Call: "toolu_2"}, []string{c.URL}, keys, time.Now()); err == nil {
		t.Fatal("accepted for another call")
	}
	claims, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: "s1", Call: "toolu_1"}, []string{c.URL}, keys, time.Now())
	if err != nil || claims.Thread != "" || claims.Call != "toolu_1" {
		t.Fatalf("beacon: %+v %v", claims, err)
	}
	code, out := redeemTicket(t, claims.Redeem, claims.Ticket, "s1")
	if code != 200 {
		t.Fatalf("redeem: %d %v", code, out)
	}
	waiterPolls(t, c, out) // the hooks deliver once their waiter listens
	again, _ := subscribe(app, caller{User: "bo", Client: "claude-code", Session: sub.Session, Meta: map[string]any{"claudecode/toolUseId": "toolu_3"}},
		"ci/acme/web", subOpts{})
	if !strings.Contains(again.Addition.Text, "hooks for Claude Code deliver") || strings.Contains(again.Addition.Text, "curl -sN") {
		t.Fatalf("again: %s", again.Addition.Text)
	}
	// Bound now: its next beacon names the session's conversation.
	tok, _ = again.Addition.Meta[beacon.MetaKey].(string)
	if _, err := beacon.Verify(tok, "s1", []string{c.URL}, keys, time.Now()); err != nil {
		t.Fatalf("bound: %v", err)
	}
	// Without the call's ID (an older Claude Code): no beacon at all.
	other, _ := subscribe(app, caller{User: "cat", Client: "claude-code"}, "ci/acme/web", subOpts{})
	if len(other.Addition.Meta) != 0 {
		t.Fatalf("a beacon bound to nothing: %v", other.Addition.Meta)
	}
}

// One Claude Code conversation watching two apps through the hooks: each
// app's beacon connects a link of its own (the second leaves the first's in
// place), the waiter listens on both, and a message from either wakes the
// session, acknowledged on its own link, once.
func TestClaudeCodeHooksTwoApps(t *testing.T) {
	c := coretest.Start(t)
	a, b := noteApp(t, c, "ci"), noteApp(t, c, "ci2")
	h := hook(t, c, "claude-code")
	const conv = "c95dca59-2222-4ebb-a70e-8d69fde72b92"
	tr := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(tr, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	connect := func(app *wakeshift.Client, server, topic, session string) noteSub {
		t.Helper()
		calls++
		call := fmt.Sprintf("toolu_%d", calls)
		sub, err := subscribe(app, caller{User: "bo", Client: "claude-code", Session: session, Tool: "watch",
			Meta: map[string]any{"claudecode/toolUseId": call}}, topic, subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		line, _ := json.Marshal(map[string]any{"type": "user", "sessionId": conv,
			"message": map[string]any{"role": "user", "content": []any{map[string]any{"tool_use_id": call, "type": "tool_result", "content": "ok"}}},
			"mcpMeta": map[string]any{"_meta": sub.Addition.Meta}})
		f, _ := os.OpenFile(tr, os.O_APPEND|os.O_WRONLY, 0o600)
		f.Write(append(line, '\n'))
		f.Close()
		out := runHook(h, "", map[string]any{"session_id": conv, "transcript_path": tr, "hook_event_name": "PostToolUse",
			"tool_name": "mcp__" + server + "__watch", "tool_response": "ok", "tool_use_id": call, "mcp_server": map[string]any{"name": server}})
		if !strings.Contains(out, "Wakeshift: connected") {
			t.Fatalf("connecting %s: %s", server, out)
		}
		return sub
	}
	subA := connect(a, "ci", "ci/acme/web", "")
	connect(b, "ci2", "ci2/acme/web", "")
	if got := h.Links.All(conv); len(got) != 2 || got[0].App == got[1].App {
		t.Fatalf("links: %+v", got)
	}
	h.Rewake, h.RewakeWait = true, 20*time.Second
	wake := func(want, not string) {
		t.Helper()
		out := runHook(h, "", map[string]any{"session_id": conv, "transcript_path": tr, "hook_event_name": "Stop"})
		if !strings.Contains(out, want) || not != "" && strings.Contains(out, not) {
			t.Fatalf("woken with %q, want %q", out, want)
		}
	}
	for _, step := range []struct {
		app        *wakeshift.Client
		topic, msg string
	}{{a, "ci/acme/web", "from the first"}, {b, "ci2/acme/web", "from the second"}, {a, "ci/acme/web", "first again"}} {
		if p, err := emitNote(step.app, []string{step.topic}, note{Text: step.msg}); err != nil || p.Matched != 1 {
			t.Fatalf("emit %q: %+v %v", step.msg, p, err)
		}
		wake(step.msg, "")
	}
	// The first app watched again (its session, by key): its link only is
	// replaced, and both still wake it.
	connect(a, "ci", "ci/acme/web", subA.Session)
	if got := h.Links.All(conv); len(got) != 2 {
		t.Fatalf("links after watching again: %+v", got)
	}
	emitNote(b, []string{"ci2/acme/web"}, note{Text: "second, still"})
	wake("second, still", "first again")
	emitNote(a, []string{"ci/acme/web"}, note{Text: "first, still"})
	wake("first, still", "second, still")
}

// A stateless MCP server (a new server per request) never sees
// initialize, so its calls have no client name: the core tells the harness
// from the call's _meta, else its User-Agent, and each gets its own
// harness's result. A stated client name wins, and _meta that names a
// harness gets exactly what stating that harness's client name gets.
func TestStatelessCalls(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	keys := coreKeys(t, c)
	agent := func(t *testing.T, sub noteSub, harness, via string) {
		t.Helper()
		if got := sub.Addition.Agent; got.Harness != harness || got.Via != via {
			t.Fatalf("agent %+v, want %s via %s", got, harness, via)
		}
	}

	t.Run("claude code by _meta", func(t *testing.T) {
		sub, err := subscribe(app, caller{Meta: map[string]any{"claudecode/toolUseId": "toolu_9"}}, "ci/acme/web", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		agent(t, sub, "claude-code", "meta")
		if !strings.Contains(sub.Addition.Text, "start the Monitor tool") || !strings.Contains(sub.Addition.Text, "/v1/stream/wait?link=") ||
			!strings.Contains(sub.Addition.Text, `session_key="`+sub.Session+`"`) || !strings.HasPrefix(sub.Session, "wsc_") {
			t.Fatalf("text: %s", sub.Addition.Text)
		}
		tok, _ := sub.Addition.Meta[beacon.MetaKey].(string)
		if _, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: "s9", Call: "toolu_9"}, []string{c.URL}, keys, time.Now()); err != nil {
			t.Fatalf("beacon for the call: %v", err)
		}
	})

	t.Run("codex by _meta, as by its client name", func(t *testing.T) {
		meta := map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": "019a-stateless"}}
		sub, err := subscribe(app, caller{Meta: meta}, "ci/acme/web", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		agent(t, sub, "codex", "meta")
		tok, _ := sub.Addition.Meta[beacon.MetaKey].(string)
		if !strings.Contains(sub.Addition.Text, `"Wakeshift: connected"`) || !strings.HasPrefix(sub.Session, "wsc_") {
			t.Fatalf("result: %+v", sub)
		}
		if _, err := beacon.Verify(tok, "019a-stateless", []string{c.URL}, keys, time.Now()); err != nil {
			t.Fatalf("beacon for the thread: %v", err)
		}
		// Stating the client name (with its key) reaches the same subscription,
		// nothing more: the _meta gained nothing the name didn't give.
		named, err := subscribe(app, caller{Client: "codex-mcp-client", Meta: meta, Session: sub.Session}, "ci/acme/web", subOpts{})
		if err != nil || named.ID != sub.ID || named.Session != sub.Session {
			t.Fatalf("named: %+v %v (was %s)", named, err, sub.ID)
		}
		agent(t, named, "codex", "client")
	})

	t.Run("antigravity by _meta", func(t *testing.T) {
		const conv = "5d2c1a7e-stateless"
		sub, err := subscribe(app, caller{UserAgent: "Go-http-client/1.1", Meta: map[string]any{"antigravity.google/conversation_id": conv}},
			"ci/acme/web", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		agent(t, sub, "antigravity", "meta")
		if !strings.Contains(sub.Addition.Text, "/install/antigravity.sh") {
			t.Fatalf("text: %s", sub.Addition.Text)
		}
		if _, err := beacon.Verify(beacon.InText(sub.Addition.Text), conv, []string{c.URL}, keys, time.Now()); err != nil {
			t.Fatalf("beacon for the conversation: %v", err)
		}
	})

	t.Run("copilot by _meta", func(t *testing.T) {
		const conv = "1b47c0de-stateless"
		sub, err := subscribe(app, caller{Meta: map[string]any{"wakeshift/conversation": conv, "progressToken": 1}}, "ci/acme/web", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		agent(t, sub, "copilot", "meta")
		if !strings.Contains(sub.Addition.Text, "/install/copilot.sh") {
			t.Fatalf("text: %s", sub.Addition.Text)
		}
		if _, err := beacon.Verify(beacon.InText(sub.Addition.Text), conv, []string{c.URL}, keys, time.Now()); err != nil {
			t.Fatalf("beacon for the conversation: %v", err)
		}
	})

	t.Run("opencode by User-Agent", func(t *testing.T) {
		sub, err := subscribe(app, caller{UserAgent: "opencode/1.18.7", Meta: map[string]any{"progressToken": 2}}, "ci/acme/web", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		agent(t, sub, "opencode", "user_agent")
		if !strings.Contains(sub.Addition.Text, "/install/opencode.sh") || sub.Addition.Meta[beacon.MetaKey] == nil {
			t.Fatalf("result: %+v", sub.Addition)
		}
	})

	t.Run("nothing to recognize", func(t *testing.T) {
		sub, err := subscribe(app, caller{UserAgent: "Go-http-client/1.1", Meta: map[string]any{"threadId": "t1"}}, "ci/acme/web", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		agent(t, sub, "", "")
		if !strings.HasPrefix(sub.Addition.Text, "Wakeshift can't wake this agent, so nothing will notify you here") || len(sub.Addition.Meta) != 0 {
			t.Fatalf("result: %+v", sub.Addition)
		}
	})

	t.Run("a stated client wins", func(t *testing.T) {
		sub, err := subscribe(app, caller{Client: "Cursor", UserAgent: "claude-code/2.1.280", Meta: map[string]any{"claudecode/toolUseId": "toolu_10"}},
			"ci/acme/web", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		agent(t, sub, "cursor", "client")
		if !strings.Contains(sub.Addition.Text, "/install/cursor.sh") || strings.Contains(sub.Addition.Text, "Monitor") || len(sub.Addition.Meta) != 0 {
			t.Fatalf("result: %+v", sub.Addition)
		}
	})

	t.Run("annotate says what it recognized", func(t *testing.T) {
		r, err := annotate(app, caller{UserAgent: "claude-code/2.1.280 (cli)"})
		if err != nil || r.Agent.Harness != "claude-code" || r.Agent.Via != "user_agent" || r.Text != "" {
			t.Fatalf("annotate: %+v %v", r, err)
		}
		if r, err := annotate(app, caller{}); err != nil || r.Agent != (wakeshift.Recognized{}) {
			t.Fatalf("annotate, nothing: %+v %v", r, err)
		}
	})
}
