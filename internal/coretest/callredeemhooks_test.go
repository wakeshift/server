package coretest_test

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/connector"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/pluginmark"
	"github.com/wakeshift/server/internal/version"
)

// Redeem by call with the real hooks: the PreToolUse claim, then PostToolUse by the call's ID with no
// transcript; every other case by the beacon.
func TestRedeemByCallHooks(t *testing.T) {
	c := coretest.StartWith(t, func(o *core.Options) { o.RedeemByToolCall = true })
	t.Setenv("WAKESHIFT_REDEEM_BY_TOOL_CALL", "1")
	root := t.TempDir()
	t.Setenv("WAKESHIFT_HOME", root)
	dir := filepath.Join(root, "claude-code")
	app := noteApp(t, c, "ci")
	mark := pluginmark.Value("claude-code", version.Version, []string{c.URL})
	cc := callCore{t: t, c: c, mark: mark}

	sub := func(call, session, harness string) noteSub {
		t.Helper()
		s, err := subscribe(app, caller{User: "bo", Client: "claude-code", Tool: "watch_builds", Meta: map[string]any{"claudecode/toolUseId": call},
			Session: session, Harness: harness}, "ci/acme/web", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	preIn := func(conv, call, harness, server string) map[string]any {
		input := map[string]any{}
		if harness != "" {
			input["harness"] = harness
		}
		return map[string]any{"session_id": conv, "transcript_path": filepath.Join(root, conv+".jsonl"), "hook_event_name": "PreToolUse",
			"tool_name": "mcp__" + server + "__watch_builds", "tool_input": input, "tool_use_id": call, "mcp_server": map[string]any{"name": server}}
	}
	pre := func(conv, call, harness string) {
		t.Helper()
		if out := runHook(hookIn(c, "claude-code", dir), connector.ClaudeClaimEvent, preIn(conv, call, harness, "ci")); out != "" {
			t.Fatalf("the claim printed %q: it must never decide the call", out)
		}
	}
	claimOutcome := func(call string) string {
		h := sha256.Sum256([]byte("wakeshift-call\x00" + call))
		b, err := os.ReadFile(filepath.Join(dir, "claims", hex.EncodeToString(h[:16])))
		if err != nil {
			return ""
		}
		var rec struct{ Outcome string }
		json.Unmarshal(b, &rec)
		return rec.Outcome
	}
	transcript := func(conv, call string, s noteSub) string {
		tr := filepath.Join(t.TempDir(), "t.jsonl")
		line, _ := json.Marshal(map[string]any{"type": "user", "sessionId": conv,
			"message": map[string]any{"role": "user", "content": []any{map[string]any{"tool_use_id": call, "type": "tool_result", "content": "ok"}}},
			"mcpMeta": map[string]any{"_meta": s.Addition.Meta}})
		os.WriteFile(tr, append(line, '\n'), 0o600)
		return tr
	}
	none := filepath.Join(t.TempDir(), "none.jsonl") // no transcript line ever
	postIn := func(conv, call, tr string) map[string]any {
		return map[string]any{"session_id": conv, "transcript_path": tr, "hook_event_name": "PostToolUse",
			"tool_name": "mcp__ci__watch_builds", "tool_response": "ok", "tool_use_id": call, "mcp_server": map[string]any{"name": "ci"}}
	}
	post := func(conv, call, tr string) string {
		return runHook(hookIn(c, "claude-code", dir), "", postIn(conv, call, tr))
	}
	sessionKey := func(conv string) ed25519.PrivateKey {
		h := sha256.Sum256([]byte("wakeshift-session\x00" + conv))
		b, err := os.ReadFile(filepath.Join(dir, "sessions", hex.EncodeToString(h[:16]), "key"))
		if err != nil {
			t.Fatal(err)
		}
		seed, _ := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(b)))
		return ed25519.NewKeyFromSeed(seed)
	}
	connected := func(what, out string) {
		t.Helper()
		if !strings.Contains(out, "Wakeshift: connected") {
			t.Fatalf("%s: %s", what, out)
		}
	}

	// 1. A marked call on a server not pinned yet: claimed, connected by
	// call with no transcript; the answer's app is pinned.
	const convA = "0a1f1c2e-5b53-4d0e-9a71-0c3e2f6d8b10"
	pre(convA, "toolu_A1", mark)
	if o := claimOutcome("toolu_A1"); o != "claimed" {
		t.Fatalf("claim: %q", o)
	}
	sub("toolu_A1", "", mark)
	start := time.Now()
	connected("by call", post(convA, "toolu_A1", none))
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("by call took %v: it waited for the transcript", d)
	}

	// 2. Held: the next marked call with this plugin's own mark isn't
	// claimed; one with another mark is; another mark to a server no app is
	// pinned to sends nothing.
	pre(convA, "toolu_A2", mark)
	if o := claimOutcome("toolu_A2"); o != "skipped_held" {
		t.Errorf("held: %q", o)
	}
	foreign := "claude-code; wakeshift-plugin=0.0.1; trusts=" + pluginmark.IssuerTag(c.URL)
	pre(convA, "toolu_A3", foreign)
	if o := claimOutcome("toolu_A3"); o != "claimed" {
		t.Errorf("foreign mark: %q", o)
	}
	runHook(hookIn(c, "claude-code", dir), connector.ClaudeClaimEvent, preIn(convA, "toolu_A4", foreign, "other"))
	if o := claimOutcome("toolu_A4"); o != "skipped_foreign_unpinned" {
		t.Errorf("foreign mark, unpinned server: %q", o)
	}

	// 3. An unmarked call: no claim, the beacon connects it.
	const convB = "0b1f1c2e-5b53-4d0e-9a71-0c3e2f6d8b10"
	pre(convB, "toolu_B1", "")
	if o := claimOutcome("toolu_B1"); o != "" {
		t.Errorf("unmarked call claimed: %q", o)
	}
	b1 := sub("toolu_B1", "", "")
	connected("unmarked, by beacon", post(convB, "toolu_B1", transcript(convB, "toolu_B1", b1)))

	// 4. A stranger with the call's ID, before the victim's hook: refused;
	// the victim connects by call and is told someone tried.
	const convC = "0c1f1c2e-5b53-4d0e-9a71-0c3e2f6d8b10"
	pre(convC, "toolu_C1", mark)
	c1 := sub("toolu_C1", "", mark)
	if st, _, _ := cc.redeem(newKey(), "toolu_C1", convC, time.Now()); st != 404 {
		t.Errorf("stranger: %d", st)
	}
	out := post(convC, "toolu_C1", none)
	connected("victim after the stranger", out)
	if !strings.Contains(out, "someone else tried to connect this conversation with 1 other key") {
		t.Errorf("no note about the stranger: %s", out)
	}

	// 4b. A stranger claims the call first: contested, bound to neither
	// key; the victim's claim conflicts, it connects by the beacon and the
	// agent is told.
	const convE = "0e3f1c2e-5b53-4d0e-9a71-0c3e2f6d8b10"
	tenant := workspaceOf(t, c, "ci", "bo")
	cc.claim(newKey(), "toolu_C5", convE, "ci")
	pre(convE, "toolu_C5", mark)
	if o := claimOutcome("toolu_C5"); o != "conflict" {
		t.Errorf("claimed first by a stranger: %q", o)
	}
	c5 := sub("toolu_C5", "", mark)
	if got := ticketOf(t, c, tenant, "toolu_C5"); got != "contested/-" {
		t.Errorf("C5: %s", got)
	}
	out = post(convE, "toolu_C5", transcript(convE, "toolu_C5", c5))
	if strings.Contains(out, "couldn't") {
		t.Errorf("contested, by beacon: %s", out)
	}
	notes, _ := os.ReadDir(filepath.Join(dir, "notes"))
	told := strings.Contains(out, "claimed one of this conversation's tool calls first")
	for _, n := range notes {
		b, _ := os.ReadFile(filepath.Join(dir, "notes", n.Name()))
		told = told || strings.Contains(string(b), "claimed one of this conversation's tool calls first")
	}
	if !told {
		t.Errorf("the agent isn't told the claim was taken: %q", out)
	}
	if n := count(t, c, tenant, `SELECT count(*) FROM connector_tickets WHERE redeemed_via = 'beacon' AND binding = 'contested'`); n != 1 {
		t.Errorf("contested tickets redeemed by beacon: %d", n)
	}

	// 5. Both the plugin's and the installer's hooks installed: both claim
	// (the same session key: a repeat) and both run PostToolUse at once;
	// one connects it, and the other says nothing (no failure).
	const convD = "0d1f1c2e-5b53-4d0e-9a71-0c3e2f6d8b10"
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runHook(hookIn(c, "claude-code", dir), connector.ClaudeClaimEvent, preIn(convD, "toolu_D1", mark, "ci"))
		}()
	}
	wg.Wait()
	d1 := sub("toolu_D1", "", mark)
	tr := transcript(convD, "toolu_D1", d1)
	outs := make([]string, 2)
	for i := range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); outs[i] = post(convD, "toolu_D1", tr) }()
	}
	wg.Wait()
	conn, failed := 0, 0
	for _, o := range outs {
		if strings.Contains(o, "Wakeshift: connected") {
			conn++
		}
		if strings.Contains(o, "couldn't") || strings.Contains(o, "expired") {
			failed++
		}
	}
	if conn != 1 || failed != 0 {
		t.Errorf("two hook sets: %q", outs)
	}

	// 6. A copied key file: refused for another conversation.
	keyC := sessionKey(convC)
	pre(convC, "toolu_C2", foreign)
	sub("toolu_C2", c1.Session, mark)
	if st, _, _ := cc.redeem(keyC, "toolu_C2", "0f1f1c2e-5b53-4d0e-9a71-0c3e2f6d8b10", time.Now()); st != 404 {
		t.Errorf("copied key, another conversation: %d", st)
	}

	// 7. A resume without the key file: a new key, nothing bound, the beacon.
	h := sha256.Sum256([]byte("wakeshift-session\x00" + convC))
	if err := os.RemoveAll(filepath.Join(dir, "sessions", hex.EncodeToString(h[:16]))); err != nil {
		t.Fatal(err)
	}
	pre(convC, "toolu_C3", foreign)
	c3 := sub("toolu_C3", c1.Session, mark)
	connected("resume without the key, by beacon", post(convC, "toolu_C3", transcript(convC, "toolu_C3", c3)))

	// 8. A core that never answers: the claim gives up at its timeout
	// (750 ms), prints nothing, and the call goes on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	slowDir := filepath.Join(t.TempDir(), "slow")
	slow := "http://" + ln.Addr().String()
	sh := hookIn(c, "claude-code", slowDir)
	sh.Verifier.Settings.Issuers = []string{slow}
	start = time.Now()
	if out := runHook(sh, connector.ClaudeClaimEvent, preIn("0e1f1c2e-5b53-4d0e-9a71-0c3e2f6d8b10", "toolu_E1",
		pluginmark.Value("claude-code", version.Version, []string{slow}), "ci")); out != "" {
		t.Errorf("printed %q", out)
	}
	if d := time.Since(start); d < 700*time.Millisecond || d > 1500*time.Millisecond {
		t.Errorf("the claim held the call %v", d)
	}

	// 9. A subagent's context gets the mark.
	out = runHook(hookIn(c, "claude-code", dir), connector.ClaudeSubagentContextEvent, map[string]any{"session_id": convA, "hook_event_name": "SubagentStart",
		"agent_id": "a1", "agent_type": "general-purpose"})
	if !strings.Contains(out, `"hookEventName":"SubagentStart"`) || !strings.Contains(out, "wakeshift-plugin=") {
		t.Errorf("subagent context: %s", out)
	}

	// 10. The redeem log has one line per claim and redeem, and no IDs.
	logb, err := os.ReadFile(connector.RedeemLog(dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"toolu_", convA, convC, c.URL} {
		if strings.Contains(string(logb), secret) {
			t.Errorf("redeems.log names %q", secret)
		}
	}
	if !strings.Contains(string(logb), `path=call outcome="ok"`) || !strings.Contains(string(logb), `fallback=no_claim`) {
		t.Errorf("redeems.log: %s", logb)
	}
}
