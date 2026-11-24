package coretest_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/connector"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/pluginmark"
)

// Wakeshift's plugin for Copilot CLI tells the model a mark when a session
// starts, as Claude Code's and Codex's do (#1107). A watch with it, from a
// session whose plugin hooks named the conversation (preMcpToolCall), gets
// no install offer and no shell loop on the session's link: only that the
// plugin connects the conversation from this result and gives its wait
// command. The result still carries the beacon, which the plugin's real
// postToolUse hook redeems, saying "connected" and giving the command. A
// mark from a connector that trusts another Wakeshift, or one where the
// plugin's hooks didn't name the conversation (nothing could connect it),
// gets what an unmarked call gets.
func TestCopilotPluginMark(t *testing.T) {
	t.Setenv("WAKESHIFT_HOME", t.TempDir())
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	const session = "1b47c0de-2f6e-4a8e-9c1d-6a1e2b3c4d5e"

	h := hook(t, c, "copilot")
	os.WriteFile(filepath.Join(h.Verifier.Settings.Dir, connector.PluginIssuerFile), []byte(c.URL+"\n"), 0o600)
	h.WaitCommand = connector.WaitCommand("/Users/ada/.wakeshift/bin/wakeshift")
	var start struct {
		AdditionalContext string `json:"additionalContext"`
	}
	out := runHook(h, connector.CopilotSessionStartEvent, map[string]any{"sessionId": session, "timestamp": 1, "cwd": "/p", "source": "new"})
	if err := json.Unmarshal([]byte(out), &start); err != nil {
		t.Fatalf("sessionStart: %q", out)
	}
	i := strings.Index(start.AdditionalContext, `pass harness="`)
	if i < 0 {
		t.Fatalf("context: %s", start.AdditionalContext)
	}
	mark := start.AdditionalContext[i+len(`pass harness="`):]
	mark = mark[:strings.Index(mark, `"`)]
	claim, m, ok := pluginmark.Split(mark)
	if !ok || claim != "copilot" || !m.TrustsIssuer(c.URL) || m.Hooks || strings.Contains(mark, c.URL) {
		t.Fatalf("mark: %q", mark)
	}

	// The plugin's preMcpToolCall names the conversation to the app's server.
	var pre struct {
		MetaToUse map[string]any `json:"metaToUse"`
	}
	json.Unmarshal([]byte(runHook(h, "preMcpToolCall", map[string]any{"sessionId": session, "toolCallId": "call_1", "serverName": "ci", "toolName": "watch", "arguments": map[string]any{}})), &pre)
	name, _ := pre.MetaToUse[connector.CopilotConversationMeta].(string)
	if name == "" {
		t.Fatalf("preMcpToolCall: %+v", pre)
	}
	call := func(user, conv, harness string) caller {
		cl := caller{User: user, Client: "copilot-cli", Tool: "watch"}
		if conv != "" {
			cl.Meta = map[string]any{connector.CopilotConversationMeta: conv}
		}
		return cl.WithHarness(harness)
	}

	sub, err := subscribe(app, call("bo", name, mark), "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	text := sub.Addition.Text
	for _, no := range []string{"/v1/stream/wait", "copilot plugin", "Install", "ask_user", "--allow-all-tools", "harness=\""} {
		if strings.Contains(text, no) {
			t.Fatalf("the marked result says %q: %s", no, text)
		}
	}
	if !strings.HasPrefix(text, "Subscribed. Wakeshift's plugin for GitHub Copilot CLI is installed here") || !strings.Contains(text, "from this result") ||
		!strings.Contains(text, `shellId "`+harnesses.CopilotShell+`"`) || !strings.Contains(text, "don't ask the user") {
		t.Fatalf("text: %s", text)
	}
	tok := beacon.InText(text)
	if _, err := beacon.Verify(tok, name, []string{c.URL}, coreKeys(t, c), time.Now()); err != nil {
		t.Fatalf("beacon: %v", err)
	}

	// The plugin's postToolUse connects from it, says so and gives the
	// wait command, as the result promised.
	var post struct {
		AdditionalContext string `json:"additionalContext"`
		ModifiedResult    struct {
			TextResultForLlm string `json:"textResultForLlm"`
		} `json:"modifiedResult"`
	}
	out = runHook(h, "postToolUse", map[string]any{"sessionId": session, "toolName": "ci-watch", "toolArgs": map[string]any{},
		"toolResult": map[string]any{"resultType": "success", "textResultForLlm": text}})
	if err := json.Unmarshal([]byte(out), &post); err != nil {
		t.Fatalf("postToolUse: %q", out)
	}
	if !strings.HasPrefix(post.AdditionalContext, harnesses.Connected) || !strings.Contains(post.AdditionalContext, "`"+h.WaitCommand+"`") ||
		beacon.InText(post.ModifiedResult.TextResultForLlm) != "" {
		t.Fatalf("postToolUse: %s", out)
	}

	// The same mark trusting another Wakeshift, or with no conversation
	// named (the hooks that would connect it didn't run): unmarked.
	elsewhere := pluginmark.Value("copilot", "2026.11.09.1200", []string{"https://other.example"})
	for what, cl := range map[string]caller{
		"another Wakeshift's mark": call("cy", "ci-conv-cy", elsewhere),
		"no conversation named":    call("di", "", mark),
		"no mark":                  call("ed", "ci-conv-ed", ""),
	} {
		s, err := subscribe(app, cl, "ci/acme/web", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(s.Addition.Text, "Subscribed. Wakeshift's plugin") || !strings.Contains(s.Addition.Text, "/v1/stream/wait") {
			t.Fatalf("%s: %s", what, s.Addition.Text)
		}
	}
}
