package appapi

import (
	"strings"
	"testing"
)

// opencode 2.4.0 has no watch_mcp_event tool: its event watch is an
// automation with an event trigger. The note names that first, with the
// trigger's fields as opencode's automation.create takes them, keeps
// watch_mcp_event for the 2.3 flag build, says this call didn't subscribe,
// and asks for nothing but the server, event and arguments (the voucher is
// in the arguments): never the session key, which a model would then copy
// into the automation it stores.
func TestOpencodeWatchNote(t *testing.T) {
	n := opencodeWatchNote
	for _, want := range []string{
		"automation.create",
		`"type": "event"`,
		`"server": this MCP server's name`,
		`"event": the event's name`,
		`"args": those arguments`,
		"before opencode 2.4, the watch_mcp_event tool",
		"This tool call didn't subscribe you",
		"nothing wakes this conversation until you subscribe there",
	} {
		if !strings.Contains(n, want) {
			t.Errorf("note lacks %q: %q", want, n)
		}
	}
	// 2.4.0 first: a model reads the tool it has before the one it hasn't.
	if strings.Index(n, "automation.create") > strings.Index(n, "watch_mcp_event") {
		t.Errorf("watch_mcp_event before automation.create: %q", n)
	}
	for _, bad := range []string{"session_key", "session key", "wsc_", "call watch_mcp_event"} {
		if strings.Contains(n, bad) {
			t.Errorf("note mentions %q: %q", bad, n)
		}
	}
}
