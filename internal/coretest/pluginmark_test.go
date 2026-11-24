package coretest_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/codexconnector"
	"github.com/wakeshift/server/internal/connector"
	"github.com/wakeshift/server/internal/connectorversion"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/pluginmark"
)

// sessionMark runs the SessionStart session-context hook, the plugin's
// (plugin) or the one Wakeshift's installer writes to user settings, and
// returns the mark its context tells the model to pass ("" if it adds none).
func sessionMark(t *testing.T, c *coretest.Core, plugin bool) string {
	t.Helper()
	h := hook(t, c, "claude-code")
	if plugin { // what the plugin's install leaves (connector.InstallPlugin)
		os.WriteFile(filepath.Join(h.Verifier.Settings.Dir, connector.PluginIssuerFile), []byte(c.URL+"\n"), 0o600)
	}
	out := strings.TrimSpace(runHook(h, connector.ClaudeSessionContextEvent,
		map[string]any{"session_id": "5e551011-0000-4000-8000-000000000001", "transcript_path": "/x.jsonl", "hook_event_name": "SessionStart", "source": "startup"}))
	if out == "" {
		return ""
	}
	var o struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &o); err != nil {
		t.Fatalf("SessionStart output: %s (%v)", out, err)
	}
	ctx := o.HookSpecificOutput.AdditionalContext
	i := strings.Index(ctx, `pass harness="`)
	if i < 0 {
		t.Fatalf("context: %s", ctx)
	}
	rest := ctx[i+len(`pass harness="`):]
	return rest[:strings.Index(rest, `"`)]
}

// Wakeshift's plugin for Claude Code tells the model, when a session
// starts, a mark to pass as the harness argument of an app's tools; a call
// with it gets a result that asks nothing and starts no Monitor: the
// plugin's hooks connect the conversation from the result's beacon when the
// turn ends. Only a mark from a connector that trusts this Wakeshift, and
// that it serves, quiets the result.
func TestClaudeCodePluginMark(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ATTENDED", "") // the hook as an interactive session runs it
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	const session = "5e551011-0000-4000-8000-000000000001"
	mark := sessionMark(t, c, true)
	claim, m, ok := pluginmark.Split(mark)
	if !ok || claim != "claude-code" || !m.TrustsIssuer(c.URL) || strings.Contains(mark, c.URL) || m.Hooks {
		t.Fatalf("mark: %q", mark)
	}

	cc := caller{User: "bo", Client: "claude-code", Meta: map[string]any{"claudecode/toolUseId": "toolu_m1"}}
	sub, err := subscribe(app, cc.WithHarness(mark), "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	text := sub.Addition.Text
	for _, no := range []string{"AskUserQuestion", "Monitor tool", "curl", "claude plugin install", "harness=\""} {
		if strings.Contains(text, no) {
			t.Fatalf("the marked result says %q: %s", no, text)
		}
	}
	if !strings.HasPrefix(text, "Subscribed. Wakeshift's plugin for Claude Code is installed here") ||
		!strings.Contains(text, "when this turn ends") || !strings.Contains(text, "don't ask the user") || !strings.Contains(text, "session_key=\""+sub.Session+"\"") {
		t.Fatalf("text: %s", text)
	}
	// Its beacon is the one the plugin's PostToolUse hook redeems.
	tok, _ := sub.Addition.Meta[beacon.MetaKey].(string)
	claims, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: session, Call: "toolu_m1"}, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil {
		t.Fatalf("beacon: %v", err)
	}
	if code, out := redeemTicket(t, claims.Redeem, claims.Ticket, session); code != 200 {
		t.Fatalf("redeem: %d %v", code, out)
	}

	// A mark from a connector that trusts another Wakeshift (it wouldn't
	// redeem this one's beacon): the offer, as without one.
	elsewhere := pluginmark.Value("claude-code", "2026.11.05.1200", []string{"https://other.example"})
	sub2, err := subscribe(app, caller{User: "cy", Client: "claude-code", Meta: map[string]any{"claudecode/toolUseId": "toolu_m2"}}.WithHarness(elsewhere), "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sub2.Addition.Text, "Ask them one question with your AskUserQuestion tool") {
		t.Fatalf("another Wakeshift's mark quieted the result: %s", sub2.Addition.Text)
	}
	// The mark never reaches the claim: no "pass harness=" correction.
	if strings.Contains(sub2.Addition.Text, "pass harness=") {
		t.Fatalf("claim note: %s", sub2.Addition.Text)
	}
}

// A mark from a connector older than the core serves (it would be refused)
// doesn't quiet the result: the user is asked as without the plugin.
func TestClaudeCodePluginMarkOutdated(t *testing.T) {
	c := coretest.StartWith(t, func(o *core.Options) {
		o.Connectors = connectorversion.Minimums{"wakeshift": "2026.10.20"}
	})
	app := noteApp(t, c, "ci")
	for v, quiet := range map[string]bool{"2026.10.19.0900": false, "2026.10.21.0900": true} {
		mark := pluginmark.Value("claude-code", v, []string{c.URL})
		sub, err := subscribe(app, caller{User: "u" + v, Client: "claude-code", Meta: map[string]any{"claudecode/toolUseId": "toolu_" + v}}.WithHarness(mark), "ci/acme/web", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		asked := strings.Contains(sub.Addition.Text, "AskUserQuestion")
		if asked == quiet {
			t.Fatalf("connector %s: quiet %v, text: %s", v, quiet, sub.Addition.Text)
		}
	}
}

// A marked watch by a route the plugin can't connect is never told the
// plugin connects it: through Anthropic's connector proxy (an app added as
// a claude.ai connector: clientInfo "claude-ai", and no tool call ID, as on
// November 9, 2026, from the desktop app's Code tab), or from a client that
// names no tool call to bind a beacon to. Each gets the Monitor's command,
// no install offer and no beacon (nothing on that route would redeem it),
// and a later watch gets the command again, not the plugin's ask. The
// local route stays as it was.
func TestClaudeCodePluginMarkUnconnectable(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	mark := pluginmark.Value("claude-code", "2026.11.09.1200", []string{c.URL})
	for _, tc := range []struct {
		name string
		call caller
	}{
		{"claude.ai connector", caller{User: "bo", Client: "claude-ai", Tool: "watch"}},
		{"claude.ai connector, with a call ID", caller{User: "cy", Client: "claude-ai", Tool: "watch", Meta: map[string]any{"claudecode/toolUseId": "toolu_p1"}}},
		{"no call ID", caller{User: "di", Client: "claude-code", Tool: "watch"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub, err := subscribe(app, tc.call.WithHarness(mark), "ci/acme/web", subOpts{})
			if err != nil {
				t.Fatal(err)
			}
			text := sub.Addition.Text
			if strings.Contains(text, "is installed here") || strings.Contains(text, "connects this conversation when this turn ends") {
				t.Fatalf("the result promises the plugin connects it: %s", text)
			}
			for _, no := range []string{"AskUserQuestion", "claude plugin install", "Notify me in every session"} {
				if strings.Contains(text, no) {
					t.Fatalf("the result offers the plugin (%q): %s", no, text)
				}
			}
			if !strings.HasPrefix(text, harnesses.UnconnectableNote) || !strings.Contains(text, "start the Monitor tool now") || !strings.Contains(text, "session_key=\""+sub.Session+"\"") {
				t.Fatalf("text: %s", text)
			}
			if sub.Addition.Meta[beacon.MetaKey] != nil || strings.Contains(text, "wakeshift-beacon") {
				t.Fatal("a beacon on a route nothing redeems it from")
			}
			// Again in the same conversation, before any Monitor ran: the
			// command again, never the plugin's question.
			next := tc.call.WithHarness(mark)
			next.Session = sub.Session
			again, err := subscribe(app, next, "ci/acme/api", subOpts{})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(again.Addition.Text, harnesses.UnconnectableNote) || strings.Contains(again.Addition.Text, "question") || !strings.Contains(again.Addition.Text, "start the Monitor tool now") {
				t.Fatalf("again: %s", again.Addition.Text)
			}
		})
	}

	// The local route (Claude Code naming the tool call) keeps the
	// plugin's result.
	local, err := subscribe(app, caller{User: "ed", Client: "claude-code", Tool: "watch", Meta: map[string]any{"claudecode/toolUseId": "toolu_l1"}}.WithHarness(mark), "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(local.Addition.Text, "Subscribed. Wakeshift's plugin for Claude Code is installed here") || local.Addition.Meta[beacon.MetaKey] == nil {
		t.Fatalf("local route: %s", local.Addition.Text)
	}
}

// withoutCodexMark is a Codex hook's context without the mark's note.
func withoutCodexMark(ctx string) string {
	var keep []string
	for _, p := range strings.Split(ctx, "\n\n") {
		if !strings.HasPrefix(p, "Wakeshift's plugin is installed in this Codex") && !strings.HasPrefix(p, "Wakeshift's hooks are installed in this Codex") {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, "\n\n")
}

// codexSessionMark runs Codex's SessionStart hook for thread (the plugin's
// install, with its issuer file) and returns the mark its context tells the
// model to pass.
func codexSessionMark(t *testing.T, c *coretest.Core, thread string) string {
	t.Helper()
	set := connector.Settings{Dir: t.TempDir(), Issuers: []string{c.URL}}
	os.WriteFile(filepath.Join(set.Dir, connector.PluginIssuerFile), []byte(c.URL+"\n"), 0o600)
	h := &codexconnector.Hook{Settings: set, Socket: codexDaemon(t, thread),
		Start: func(string, string, string, string, bool) error { return nil }}
	b, _ := json.Marshal(map[string]any{"session_id": thread, "hook_event_name": codexconnector.EventSessionStart, "source": "startup"})
	var out bytes.Buffer
	h.Run(ctx, bytes.NewReader(b), &out)
	var o struct {
		HookSpecificOutput struct{ AdditionalContext string } `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &o); err != nil {
		t.Fatalf("SessionStart output: %s (%v)", out.String(), err)
	}
	i := strings.Index(o.HookSpecificOutput.AdditionalContext, `pass harness="`)
	if i < 0 {
		t.Fatalf("context: %s", o.HookSpecificOutput.AdditionalContext)
	}
	rest := o.HookSpecificOutput.AdditionalContext[i+len(`pass harness="`):]
	return rest[:strings.Index(rest, `"`)]
}

// Codex's hook tells the model a mark at SessionStart, as Claude Code's
// plugin does; a watch with it gets no install offer, only that the plugin
// connects the thread from this result, and still the beacon its
// PostToolUse hook redeems. A session asked before is quiet too once
// marked. A mark from a connector that trusts another Wakeshift, or one
// the core no longer serves, gets the offer.
func TestCodexPluginMark(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("WAKESHIFT_HOME", t.TempDir())
	c := coretest.StartWith(t, func(o *core.Options) {
		o.Connectors = connectorversion.Minimums{"wakeshift": "2026.10.20"}
	})
	app := noteApp(t, c, "ci")
	const thread = "01a1ae69-49bf-7fc3-9c57-5729ede9aa01"
	codexCall := func(user, th, harness string) caller {
		return caller{User: user, Client: "codex-mcp-client", Tool: "watch", Meta: map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": th}}}.WithHarness(harness)
	}
	mark := codexSessionMark(t, c, thread)
	claim, m, ok := pluginmark.Split(mark)
	if !ok || claim != "codex" || !m.TrustsIssuer(c.URL) || m.Hooks || strings.Contains(mark, c.URL) {
		t.Fatalf("mark: %q", mark)
	}
	// The mark from a dev connector: the one this test builds has version
	// "dev", which the minimum would refuse, so a released one stands in.
	mark = pluginmark.Value("codex", "2026.11.09.1200", []string{c.URL})

	sub, err := subscribe(app, codexCall("bo", thread, mark), "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	text := sub.Addition.Text
	for _, no := range []string{"Install Wakeshift", "codex plugin", "curl", "resume --last", "harness=\""} {
		if strings.Contains(text, no) {
			t.Fatalf("the marked result says %q: %s", no, text)
		}
	}
	if !strings.HasPrefix(text, "Subscribed. Wakeshift's plugin for Codex is installed here") || !strings.Contains(text, "from this result") || !strings.Contains(text, "don't ask the user") {
		t.Fatalf("text: %s", text)
	}
	tok, _ := sub.Addition.Meta[beacon.MetaKey].(string)
	// (Redeeming it is TestConnectorBeacon's: this core's minimum refuses a
	// connector that doesn't say its version.)
	if _, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: thread}, []string{c.URL}, coreKeys(t, c), time.Now()); err != nil {
		t.Fatalf("beacon: %v", err)
	}

	// Asked before (an unmarked watch got the offer), then marked: quiet.
	const asked = "01a1ae69-49bf-7fc3-9c57-5729ede9aa02"
	first, err := subscribe(app, codexCall("cy", asked, ""), "ci/acme", subOpts{})
	if err != nil || !strings.Contains(first.Addition.Text, "Install Wakeshift") {
		t.Fatalf("unmarked: %v %s", err, first.Addition.Text)
	}
	again, err := subscribe(app, codexCall("cy", asked, mark), "ci/acme", subOpts{})
	if err == nil && again.Session != first.Session {
		t.Fatalf("a new session for the same thread: %s %s", again.Session, first.Session)
	}
	if err != nil || !strings.HasPrefix(again.Addition.Text, "Subscribed. Wakeshift's plugin for Codex") {
		t.Fatalf("asked, then marked: %v %s", err, again.Addition.Text)
	}

	for _, o := range []struct{ name, thread, mark string }{
		{"another Wakeshift's", "01a1ae69-49bf-7fc3-9c57-5729ede9aa03", pluginmark.Value("codex", "2026.11.09.1200", []string{"https://other.example"})},
		{"an outdated connector's", "01a1ae69-49bf-7fc3-9c57-5729ede9aa04", pluginmark.Value("codex", "2026.10.19.0900", []string{c.URL})},
	} {
		name, th, other := o.name, o.thread, o.mark
		s, err := subscribe(app, codexCall("u"+th, th, other), "ci/acme", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(s.Addition.Text, "Install Wakeshift") || strings.Contains(s.Addition.Text, "pass harness=") {
			t.Fatalf("%s mark: %s", name, s.Addition.Text)
		}
	}
}
