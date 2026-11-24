package coretest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/pluginmark"
)

// A subscribe's wakes (the SDKs' call.wakes) says whether a wake is
// confirmed for the conversation as it stands, so a tool skips its own
// polling only then: true where a listener runs with nothing to install or
// ask (Claude Code's Monitor where the plugin can't be offered, Cursor's
// background command), false while the user's notify choice is pending
// (Claude Code's question), where the result says it can't be woken
// (ChatGPT through a tool, Goose, an unknown program) or only offers an
// install (Codex without its connector, opencode without its hooks). Its
// call_limit (the SDKs' call.callLimit) is the harness's per-call limit in
// seconds where it's known (Claude Code's 60, Codex's 300), and left out
// otherwise.
func TestSubscribeSaysWhetherItWakes(t *testing.T) {
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	for i, tc := range []struct {
		name  string
		call  map[string]any
		wakes bool
		limit int
	}{
		// No tool call ID: the plugin can't be offered, so nothing is asked.
		{"Claude Code, Monitor only", map[string]any{"client": "claude-code"}, true, 60},
		// The plugin-first question: asked by the first call, still pending
		// on the second.
		{"Claude Code, asking the user", map[string]any{"client": "claude-code", "meta": map[string]any{"claudecode/toolUseId": "toolu_w1"}}, false, 60},
		{"Cursor", map[string]any{"client": "Cursor"}, true, 0},
		{"Codex without its connector", codexCall("", "thread-w", ""), false, 300},
		{"opencode without its hooks", map[string]any{"client": "opencode"}, false, 0},
		{"ChatGPT through a tool", map[string]any{"client": "openai-mcp"}, false, 0},
		// Recognized, but nothing of Wakeshift's wakes it while idle.
		{"ChatGPT's Codex mode (desktop), which waits in the foreground", map[string]any{"client": "openai-mcp (Codex)",
			"meta": map[string]any{"io.modelcontextprotocol/clientCapabilities": capsJSON(desktopCaps)}}, false, 0},
		{"a Cursor cloud agent", map[string]any{"client": "Cursor", "harness": "cursor-cloud"}, false, 0},
		{"a Grok Bot without its webhook, which waits in the foreground", map[string]any{"client": "Cursor", "harness": "grok-bot",
			"conversation": "bc-0f3e2d1c-5b6a-4789-8abc-def012345679"}, false, 0},
		{"Gemini CLI, retired", map[string]any{"client": "gemini-cli-mcp-client"}, false, 0},
		{"Goose", map[string]any{"client": "goose-cli"}, false, 0},
		{"an unknown program", map[string]any{"client": "some-agent"}, false, 0},
	} {
		for _, deferred := range []bool{true, false} {
			call := tc.call
			call["user"] = "w" + string(rune('a'+i))
			call["tool"] = "join"
			var out struct {
				Wakes     *bool `json:"wakes"`
				CallLimit int   `json:"call_limit"`
			}
			code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions",
				map[string]any{"call": call, "name": "game.turn", "arguments": map[string]any{"table": "oak"}, "defer_addition": deferred}, &out)
			if code != 200 || out.Wakes == nil || *out.Wakes != tc.wakes {
				t.Fatalf("%s (deferred %v): %d %s, wakes %v, want %v", tc.name, deferred, code, e, out.Wakes, tc.wakes)
			}
			if out.CallLimit != tc.limit {
				t.Fatalf("%s: call_limit %d, want %d", tc.name, out.CallLimit, tc.limit)
			}
		}
	}
}

// Claude Code's wakes follows the user's notify choice: false on the call
// that asks the question and on later ones while it's unanswered; true once
// the Monitor runs, and still after it stopped (the user picked this
// session, and the result has the agent start it again). A call
// Wakeshift's plugin marked wakes at once (nothing to ask), and so does a
// later call once its hooks have connected the conversation.
func TestClaudeCodeWakesOnceTheNotifyChoiceRuns(t *testing.T) {
	c := coretest.Start(t)
	c.Streams.WaitGrace = 300 * time.Millisecond
	app := noteApp(t, c, "ci")
	call := func(session, toolUse string) caller {
		return caller{Client: "claude-code", Session: session, Tool: "watch", Meta: map[string]any{"claudecode/toolUseId": toolUse}}
	}
	first, err := subscribe(app, call("", "toolu_1"), "ci/acme", subOpts{})
	if err != nil || first.Wakes || !strings.Contains(first.Addition.Text, "AskUserQuestion") {
		t.Fatalf("the call that asks: wakes %v, %v: %s", first.Wakes, err, first.Addition.Text)
	}
	tenant, _ := topicSubRow(t, c, first.ID)
	age := func() { // calls a while apart: no earlier beacon still connecting
		t.Helper()
		if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE connector_tickets SET created_at = created_at - interval '1 minute'`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	age()
	pending, err := subscribe(app, call(first.Session, "toolu_2"), "ci/acme", subOpts{})
	if err != nil || pending.Wakes {
		t.Fatalf("while the question is unanswered: wakes %v, %v", pending.Wakes, err)
	}

	// The user picks "this session only": the Monitor runs.
	m := monitorRE.FindStringSubmatch(first.Addition.Text)
	_, kill := runMonitor(t, strings.Replace(m[1], "timeout="+harnesses.MonitorWait, "timeout=3s", 1))
	waitFor(t, "the Monitor connected", func() bool {
		st, _ := noteStatus(app, "ci/acme")
		return st.Connected == 1
	})
	age()
	running, err := subscribe(app, call(first.Session, "toolu_3"), "ci/acme", subOpts{})
	if err != nil || !running.Wakes {
		t.Fatalf("while the Monitor runs: wakes %v, %v", running.Wakes, err)
	}
	kill()
	waitFor(t, "the Monitor gone", func() bool {
		st, _ := noteStatus(app, "ci/acme")
		return st.Connected == 0
	})
	age()
	stopped, err := subscribe(app, call(first.Session, "toolu_4"), "ci/acme", subOpts{})
	if err != nil || !stopped.Wakes || !strings.Contains(stopped.Addition.Text, "Your Wakeshift Monitor isn't running") {
		t.Fatalf("after the Monitor stopped: wakes %v, %v: %s", stopped.Wakes, err, stopped.Addition.Text)
	}

	// Wakeshift's plugin marked the call: nothing to ask.
	const conv = "5e551011-0000-4000-8000-0000000000aa"
	mark := pluginmark.Value("claude-code", "2026.11.12.0000", []string{c.URL})
	bo := caller{User: "bo", Client: "claude-code", Tool: "watch", Meta: map[string]any{"claudecode/toolUseId": "toolu_m1"}}
	marked, err := subscribe(app, bo.WithHarness(mark), "ci/acme", subOpts{})
	if err != nil || !marked.Wakes || strings.Contains(marked.Addition.Text, "AskUserQuestion") {
		t.Fatalf("a marked call: wakes %v, %v: %s", marked.Wakes, err, marked.Addition.Text)
	}
	// Its hooks connect the conversation from the result's beacon: a later
	// call wakes too, marked or not.
	tok, _ := marked.Addition.Meta[beacon.MetaKey].(string)
	claims, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: conv, Call: "toolu_m1"}, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil {
		t.Fatalf("beacon: %v", err)
	}
	code, out := redeemTicket(t, claims.Redeem, claims.Ticket, conv)
	if code != 200 {
		t.Fatalf("redeem: %d %v", code, out)
	}
	waiterPolls(t, c, out) // the hooks deliver once their waiter listens
	bo.Session, bo.Meta = marked.Session, map[string]any{"claudecode/toolUseId": "toolu_m2"}
	connected, err := subscribe(app, bo, "ci/acme", subOpts{})
	if err != nil || !connected.Wakes {
		t.Fatalf("once its hooks connected it: wakes %v, %v: %s", connected.Wakes, err, connected.Addition.Text)
	}
}
