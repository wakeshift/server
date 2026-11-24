package coretest_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// Claude Code's hooks, a conversation that watches the same app again
// without its session key (a new session, wakebench S15's step d): the
// hooks connect the new session and the old one ends, so each publish
// reaches the conversation once, and what the old session still held goes
// to the new one rather than being lost. Before, the redeem was refused
// (the old session kept the conversation), the old session stayed
// subscribed and connected through the hooks, and the new one's Monitor
// delivered every publish a second time.
func TestClaudeCodeHooksNewSessionSameApp(t *testing.T) {
	c := coretest.Start(t)
	a := noteApp(t, c, "ci")
	h := hook(t, c, "claude-code")
	const conv = "7d4b2a10-5555-4ebb-a70e-8d69fde72b92"
	tr := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(tr, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	connect := func(app *wakeshift.Client, server, topic, session string) (noteSub, string) {
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
		return sub, runHook(h, "", map[string]any{"session_id": conv, "transcript_path": tr, "hook_event_name": "PostToolUse",
			"tool_name": "mcp__" + server + "__watch", "tool_response": "ok", "tool_use_id": call, "mcp_server": map[string]any{"name": server}})
	}
	first, out := connect(a, "ci", "ci/acme/web", "")
	if !strings.Contains(out, "Wakeshift: connected") {
		t.Fatalf("connecting: %s", out)
	}
	h.Rewake, h.RewakeWait = true, 20*time.Second
	stop := func() string {
		return runHook(h, "", map[string]any{"session_id": conv, "transcript_path": tr, "hook_event_name": "Stop"})
	}
	emit := func(msg string, matched int) {
		t.Helper()
		p, err := emitNote(a, []string{"ci/acme/web"}, note{Text: msg})
		if err != nil || p.Matched != matched {
			t.Fatalf("emit %q: matched %d, want %d (%v)", msg, p.Matched, matched, err)
		}
	}
	emit("one", 1)
	if got := stop(); !strings.Contains(got, "one") {
		t.Fatalf("woken with %q", got)
	}
	// Published before the new session subscribes: only the first has it.
	emit("held by the first", 1)

	second, out := connect(a, "ci", "ci/acme/web", "")
	if second.Session == first.Session {
		t.Fatalf("watching without the key kept the session %s", first.Session)
	}
	if !strings.Contains(out, "Wakeshift: connected") {
		t.Fatalf("connecting the new session: %s", out)
	}
	if got := stop(); !strings.Contains(got, "held by the first") {
		t.Fatalf("what the first session held: woken with %q", got)
	}
	// One session of the conversation subscribed: one delivery, once.
	emit("two", 1)
	if got := stop(); !strings.Contains(got, "two") {
		t.Fatalf("woken with %q", got)
	}
	h.RewakeWait = 3 * time.Second
	if got := stop(); strings.Contains(got, "two") || strings.Contains(got, "held by the first") {
		t.Fatalf("delivered again: %q", got)
	}
}
