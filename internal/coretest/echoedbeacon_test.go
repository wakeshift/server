package coretest_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/coretest"
)

// allText is a result's text items joined with nothing between them (as
// Copilot CLI joins them; the trailer starts its own line either way).
func allText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// agyPostStep is Antigravity's PostToolUse input (agy 1.4.12) for a result
// of the app's tool: the step's output file is written (in the
// conversation's own folder) and the transcript doesn't have the step yet.
func agyPostStep(t *testing.T, conv, tool string, res *mcp.CallToolResult) string {
	t.Helper()
	generated := filepath.Join(t.TempDir(), "brain", conv, ".system_generated")
	os.MkdirAll(filepath.Join(generated, "logs"), 0o700)
	os.MkdirAll(filepath.Join(generated, "steps", "4"), 0o700)
	tr := filepath.Join(generated, "logs", "transcript_full.jsonl")
	os.WriteFile(tr, nil, 0o600)
	os.WriteFile(filepath.Join(generated, "steps", "4", "output.txt"), []byte(allText(res)), 0o600)
	b, _ := json.Marshal(map[string]any{"conversationId": conv, "stepIdx": 4, "transcriptPath": tr, "modelName": "m",
		"artifactDirectoryPath": filepath.Dir(generated),
		"toolCall":              map[string]any{"name": "call_mcp_tool", "args": map[string]any{"ServerName": "relay", "ToolName": tool, "Arguments": map[string]any{}}}})
	return string(b)
}

// Antigravity's model sees a result's beacon (its hooks can't take it out)
// and can repeat it to the user, as one did in testing. The result tells
// it not to, just before the beacon. An echoed beacon is no use to anyone
// else: Ada's hook redeemed it in PostToolUse, before the model saw it, so
// it's refused for her conversation and any other; a hook that sees it
// relayed in another conversation doesn't act on it; and one her hooks
// didn't take can't bind her session to another conversation.
func TestEchoedBeaconCantTakeOverTheConversation(t *testing.T) {
	c := coretest.Start(t)
	chat := newRelayChat(t, c)
	const adaConv, malloryConv = "2b9d08bb-7acc-4459-9956-69064f8d5869", "5e0c7a51-91d2-4b8e-a3c6-0f6d2e9b7c14"
	ada, mallory := chat.agent(t, "ada-echo", "antigravity-client"), chat.agent(t, "mallory-echo", "antigravity-client")
	ada.meta = map[string]any{"antigravity.google/conversation_id": adaConv}
	mallory.meta = map[string]any{"antigravity.google/conversation_id": malloryConv}
	adaHook, malloryHook := localHook(t, c, "antigravity"), localHook(t, c, "antigravity")
	keys := coreKeys(t, c)
	mallory.call("watch", nil) // in the chat too (her hooks aren't installed)

	res := ada.call("watch", nil)
	text := allText(res)
	echoed := beacon.Trailer(text)
	if echoed == "" || !strings.Contains(text, beacon.Guard+"\n"+beacon.Text(echoed)) {
		t.Fatalf("watch's text, no guard just before the beacon: %s", text)
	}
	claims, err := beacon.Verify(echoed, adaConv, []string{c.URL}, keys, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	runLocalHook(adaHook, "post-tool", agyPostStep(t, adaConv, "watch", res))
	link := adaHook.Links.Load(adaConv)
	if link == "" || !connectedIn(t, c, "relay", "ada-echo") {
		t.Fatal("Ada's hook didn't redeem her beacon in PostToolUse")
	}

	// The model repeats the beacon to Ada; whoever reads it can't use it.
	for _, thread := range []string{adaConv, malloryConv} {
		if code, out := redeemHeld(t, claims, thread, ""); code != http.StatusNotFound || out.Error.Code != "unknown_ticket" {
			t.Fatalf("the echoed beacon, redeemed for %s: %d %+v", thread, code, out)
		}
	}
	// Relayed into the chat, as the reply's text was, and read in
	// Mallory's conversation: her hook doesn't act on it.
	ada.call("post", map[string]any{"text": "Restart agy, then send any message.\n\n" + beacon.Text(echoed)})
	read := mallory.call("history_text", nil)
	if !strings.Contains(allText(read), echoed) {
		t.Fatalf("history_text doesn't relay it: %s", allText(read))
	}
	if out := runLocalHook(malloryHook, "post-tool", agyPostStep(t, malloryConv, "history_text", read)); strings.Contains(out, "connected") || malloryHook.Links.Load(malloryConv) != "" {
		t.Fatalf("Mallory's hook acted on Ada's echoed beacon: %s", out)
	}
	if connectedIn(t, c, "relay", "mallory-echo") || adaHook.Links.Load(adaConv) != link {
		t.Fatal("the echo moved something")
	}

	// A beacon Ada's hooks never took (before they're installed): bound to
	// her conversation, it can't connect her session to another, and is
	// used up by trying.
	fresh, err := beacon.Verify(beacon.Trailer(allText(ada.call("watch", nil))), adaConv, []string{c.URL}, keys, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if code, out := redeemHeld(t, fresh, malloryConv, ""); code != http.StatusForbidden || out.Error.Code != "wrong_thread" {
		t.Fatalf("Ada's unused beacon, for Mallory's conversation: %d %+v", code, out)
	}
	if code, out := redeemHeld(t, fresh, adaConv, ""); code != http.StatusNotFound {
		t.Fatalf("not used up: %d %+v", code, out)
	}
	if connectedIn(t, c, "relay", "mallory-echo") {
		t.Fatal("Mallory's session was connected")
	}
}
