package coretest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/connector"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/pluginmark"
	"github.com/wakeshift/server/internal/version"
)

// The async claim (the default): the PreToolUse hook hands the claim to the
// session's waiter and returns at once ("handed"); the waiter, which
// registered the session's key when it started, sends it. When the claim
// gets to the core before the call's ticket is minted, the call connects
// by its ID; when it loses that race, the call connects from the beacon.
func TestRedeemByCallAsync(t *testing.T) {
	c := coretest.StartWith(t, func(o *core.Options) { o.RedeemByToolCall = true })
	t.Setenv("WAKESHIFT_REDEEM_BY_TOOL_CALL", "1")
	t.Setenv("CLAUDE_PID", "51515")
	// A Unix socket's path has to be short.
	root, err := os.MkdirTemp("/tmp", "wsa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	t.Setenv("WAKESHIFT_HOME", root)
	dir := filepath.Join(root, "claude-code")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	app := noteApp(t, c, "ci")
	mark := pluginmark.Value("claude-code", version.Version, []string{c.URL})
	const conv = "0a3f1c2e-5b53-4d0e-9a71-0c3e2f6d8b10"

	// The session's waiter: it registers the session and serves the relay.
	w := hookIn(c, "claude-code", dir)
	w.Rewake, w.RewakeWait, w.WaiterKey = true, 30*time.Second, "51515"
	wctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b, _ := json.Marshal(map[string]any{"session_id": conv, "hook_event_name": "SessionStart", "source": "startup", "transcript_path": filepath.Join(root, "t.jsonl")})
		var out bytes.Buffer
		w.Run(wctx, bytes.NewReader(b), &out, &out)
	}()
	t.Cleanup(func() { stop(); <-done })
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(connector.ClaimSocket(dir, "51515")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Registration, in the background.
	waitLog := func(what string) {
		t.Helper()
		for i := 0; i < 100; i++ {
			if b, _ := os.ReadFile(connector.RedeemLog(dir)); strings.Contains(string(b), what) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		b, _ := os.ReadFile(connector.RedeemLog(dir))
		t.Fatalf("no %q in redeems.log:\n%s", what, b)
	}
	waitLog(`path=register outcome="registered"`)

	pre := func(call string) time.Duration {
		start := time.Now()
		if out := runHook(hookIn(c, "claude-code", dir), connector.ClaudeClaimEvent, map[string]any{"session_id": conv, "transcript_path": filepath.Join(root, "t.jsonl"),
			"hook_event_name": "PreToolUse", "tool_name": "mcp__ci__watch_builds", "tool_input": map[string]any{"harness": mark}, "tool_use_id": call,
			"mcp_server": map[string]any{"name": "ci"}}); out != "" {
			t.Fatalf("the claim printed %q", out)
		}
		return time.Since(start)
	}
	sub := func(call string) noteSub {
		t.Helper()
		s, err := subscribe(app, caller{User: "bo", Client: "claude-code", Tool: "watch_builds", Meta: map[string]any{"claudecode/toolUseId": call}, Harness: mark}, "ci/acme/web", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	post := func(call, tr string) string {
		return runHook(hookIn(c, "claude-code", dir), "", map[string]any{"session_id": conv, "transcript_path": tr, "hook_event_name": "PostToolUse",
			"tool_name": "mcp__ci__watch_builds", "tool_response": "ok", "tool_use_id": call, "mcp_server": map[string]any{"name": "ci"}})
	}

	// 1. The claim wins the race: handed, sent, bound, connected by call.
	if d := pre("toolu_W1"); d > 500*time.Millisecond {
		t.Errorf("the async claim held the call %v", d)
	}
	waitLog(`path=claim outcome="handed via=warm`)
	time.Sleep(200 * time.Millisecond) // the relay's send
	sub("toolu_W1")
	if out := post("toolu_W1", filepath.Join(root, "none.jsonl")); !strings.Contains(out, "Wakeshift: connected") {
		t.Fatalf("won race, by call: %s", out)
	}

	// 2. The claim loses the race (the ticket is minted before it gets
	// there): the redeem by call is refused, and the beacon connects it.
	const conv2 = "0b3f1c2e-5b53-4d0e-9a71-0c3e2f6d8b10"
	s2, err := subscribe(app, caller{User: "bo", Client: "claude-code", Tool: "watch_builds", Meta: map[string]any{"claudecode/toolUseId": "toolu_L1"}, Harness: mark}, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	runHook(hookIn(c, "claude-code", dir), connector.ClaudeClaimEvent, map[string]any{"session_id": conv2, "transcript_path": filepath.Join(root, "t2.jsonl"),
		"hook_event_name": "PreToolUse", "tool_name": "mcp__ci__watch_builds", "tool_input": map[string]any{"harness": mark}, "tool_use_id": "toolu_L1",
		"mcp_server": map[string]any{"name": "ci"}})
	time.Sleep(200 * time.Millisecond)
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	line, _ := json.Marshal(map[string]any{"type": "user", "sessionId": conv2,
		"message": map[string]any{"role": "user", "content": []any{map[string]any{"tool_use_id": "toolu_L1", "type": "tool_result", "content": "ok"}}},
		"mcpMeta": map[string]any{"_meta": s2.Addition.Meta}})
	os.WriteFile(tr, append(line, '\n'), 0o600)
	out := runHook(hookIn(c, "claude-code", dir), "", map[string]any{"session_id": conv2, "transcript_path": tr, "hook_event_name": "PostToolUse",
		"tool_name": "mcp__ci__watch_builds", "tool_response": "ok", "tool_use_id": "toolu_L1", "mcp_server": map[string]any{"name": "ci"}})
	if !strings.Contains(out, "Wakeshift: connected") {
		t.Fatalf("lost race, by beacon: %s", out)
	}
	waitLog(`path=call outcome="lost_race"`)
	waitLog(`path=beacon outcome="ok fallback=lost_race"`)
}
