package coretest_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/connector"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// An app's manifest names the tools that can be pre-approved: Claude
// Code's install question asks to allow exactly those, by name, and its
// command passes them to the installer; once the user said yes, the hook
// that connects a conversation from the app's result writes one exact rule
// per tool for the server Claude Code names, as the redeem says the app
// offers them.
func TestAppToolsPreapproved(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	if err := app.PublishManifest(ctx, wakeshift.Manifest{Description: "Builds", PreapproveTools: []string{"status", "history"}}); err != nil {
		t.Fatal(err)
	}
	call := caller{User: "bo", Client: "claude-code", Tool: "watch", Meta: map[string]any{"claudecode/toolUseId": "toolu_1"}}
	sub, err := subscribe(app, call, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	const install = "/install/claude-code.sh | WAKESHIFT_ALLOW_TOOLS=ci:status,history sh"
	if !strings.Contains(sub.Addition.Text, install) ||
		!strings.Contains(sub.Addition.Text, "it also lets Claude Code run Ci's status and history tools without asking each time (no other tools)") {
		t.Fatalf("offer: %s", sub.Addition.Text)
	}
	// A harness without exact per-tool rules isn't offered them.
	agy, _ := subscribe(app, caller{User: "bo", Client: "antigravity-client", Meta: map[string]any{"antigravity.google/conversation_id": "5d1c2b3a-4e5f-4a6b-9c7d-8e9f0a1b2c3d"}},
		"ci/acme/web", subOpts{})
	if strings.Contains(agy.Addition.Text, "WAKESHIFT_ALLOW_TOOLS") || strings.Contains(agy.Addition.Text, "status and history") {
		t.Fatalf("antigravity: %s", agy.Addition.Text)
	}

	// The user's machine: Claude Code with the app's server as "ci".
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WAKESHIFT_HOME", filepath.Join(home, ".wakeshift"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"projects": {"/p": {"mcpServers": {"ci": {"type": "http", "url": "`+coretest.CredsOf(app).MCPURL+`"}}}}}`), 0o600)
	spec, _ := connector.Lookup("claude-code")
	// The install, with the tools the question named (and one the app
	// doesn't name, which the hook never writes).
	if err := spec.AllowTools("ci", []string{"status", "history", "deploy"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	h := spec.NewHook(nil, nil)
	h.Verifier.Settings.Issuers = []string{c.URL}
	h.Bin = "/Users/bo/.wakeshift/bin/wakeshift"
	tok, _ := sub.Addition.Meta[beacon.MetaKey].(string)
	tr := filepath.Join(t.TempDir(), "s.jsonl")
	line, _ := json.Marshal(map[string]any{"type": "user", "sessionId": "s1",
		"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"}}},
		"mcpMeta": map[string]any{"_meta": map[string]any{beacon.MetaKey: tok}}})
	os.WriteFile(tr, append(line, '\n'), 0o600)
	in, _ := json.Marshal(map[string]any{"session_id": "s1", "transcript_path": tr, "cwd": "/p", "hook_event_name": "PostToolUse",
		"tool_name": "mcp__ci__watch", "tool_input": map[string]any{}, "tool_response": "ok", "tool_use_id": "toolu_1", "mcp_server": map[string]any{"name": "ci"}})
	var out bytes.Buffer
	h.Run(ctx, bytes.NewReader(in), &out, &out)
	if !strings.Contains(out.String(), "Wakeshift: connected") || !strings.Contains(out.String(), "Claude Code now runs Ci's status and history tools without asking") {
		t.Fatalf("hook: %s", out.String())
	}
	b, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	var settings struct{ Permissions struct{ Allow []string } }
	json.Unmarshal(b, &settings)
	if !slices.Equal(settings.Permissions.Allow, []string{"mcp__ci__status", "mcp__ci__history"}) {
		t.Fatalf("settings: %s", b)
	}
}
