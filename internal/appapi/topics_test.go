package appapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wakeshift/server/apps/agentkit"
)

func TestPrivateTo(t *testing.T) {
	for topic, want := range map[string]string{"u/ada/inbox": "ada", "u/ada": "ada", "users/ada": "", "ci/u/ada": ""} {
		if got, _ := privateTo(topic); got != want {
			t.Errorf("privateTo(%q) = %q, want %q", topic, got, want)
		}
	}
}

// The core reads a Codex thread from _meta exactly as wakeshift does.
func TestCodexThreadMatchesAppkit(t *testing.T) {
	for _, meta := range []map[string]any{
		nil,
		{"threadId": "t1"},
		{"x-codex-turn-metadata": map[string]any{"thread_id": "t2"}},
		{"threadId": "t1", "x-codex-turn-metadata": map[string]any{"thread_id": "t2"}},
		{"threadId": strings.Repeat("x", 201)},
		{"threadId": 7},
	} {
		if got, want := codexThread(meta), agentkit.CodexThread(meta); got != want {
			t.Errorf("%v: %q, wakeshift %q", meta, got, want)
		}
	}
}

func TestHarnessOf(t *testing.T) {
	codex := map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": "t1"}}
	claude := map[string]any{"claudecode/toolUseId": "toolu_1"}
	agy := map[string]any{"antigravity.google/conversation_id": "9cb9", "progressToken": "x:4"}
	copilot := map[string]any{"wakeshift/conversation": "1b47", "progressToken": 1}
	for _, c := range []struct {
		client         string
		meta           map[string]any
		harness, agent string
		via            string
	}{
		{"codex-mcp-client", codex, harnessCodex, "codex", "client"},
		{"codex-mcp-client", map[string]any{"threadId": "t1"}, harnessCodex, "codex", "client"}, // before 0.160
		{"anything", codex, harnessCodex, "codex", "meta"},                                      // a client name that names no harness
		{"codex-mcp-client", nil, "", "codex", "client"},                                        // no thread: nothing to wake
		{"claude-code", nil, harnessClaudeCode, "claude-code", "client"},
		{"Claude Code (wakeshift)", nil, harnessClaudeCode, "claude-code", "client"},
		{"claude-ai", nil, harnessCowork, "claude-ai", "client"},                                             // Claude's apps (Cowork, claude.ai chat): the agent keeps the client's name
		{"local-agent-mode-plugin:acme:ci", nil, harnessCowork, "local-agent-mode-plugin-acme-ci", "client"}, // a Cowork plugin's local server
		{"Cursor", nil, "cursor", "cursor", "client"},                                                        // Wakeshift's hooks reach these (internal/harnesses)
		{"cursor-vscode", nil, "cursor", "cursor", "client"},                                                 // the Cursor IDE
		{"copilot-cli", nil, "copilot", "copilot", "client"},                                                 // named after the harness, not the client
		{"antigravity-client", nil, "antigravity", "antigravity", "client"},
		{"opencode", nil, "opencode", "opencode", "client"},
		{"mcp", nil, "hermes", "mcp", "client"},                      // Hermes Agent: the MCP Python SDK's default name, so the agent keeps it
		{"hermes-agent", nil, "hermes", "hermes-agent", "client"},    // Hermes Agent 0.22.5's own name: its agent is named as before
		{"gemini-cli-mcp-client", nil, "gemini", "gemini", "client"}, // retired: recognized only to say so
		{"goose-cli", nil, "", "goose", "client"},                    // researched, not reached yet
		{"cursor-nightly", nil, "", "cursor", "client"},              // a variant names the agent; only the real name gets the hooks
		// A stateless server sees the name a client registered with: its
		// own name, or a tag in parentheses, still gets the hooks.
		{"OpenCode", nil, "opencode", "opencode", "client"},
		{"opencode (wakebench)", nil, "opencode", "opencode", "client"},
		{"Cursor (wakebench)", nil, "cursor", "cursor", "client"},
		{"cursor-nightly (beta)", nil, "", "cursor", "client"}, // still a variant
		{"", nil, "", "agent", ""},
		// A stateless server never sees initialize: no client name, so the
		// _meta only that harness sends names it.
		{"", codex, harnessCodex, "codex", "meta"},
		{"", claude, harnessClaudeCode, "claude-code", "meta"},
		{"", agy, "antigravity", "antigravity", "meta"},
		{"", copilot, "copilot", "copilot", "meta"},
		{"", map[string]any{"threadId": "t1"}, "", "agent", ""}, // too generic to name Codex
		{"", map[string]any{"claudecode/toolUseId": ""}, "", "agent", ""},
		{"", map[string]any{"claudecode/toolUseId": 7}, "", "agent", ""},
		{"", map[string]any{"progressToken": 1}, "", "agent", ""},
		// A stated client wins over _meta that names another harness.
		{"claude-code", codex, harnessClaudeCode, "claude-code", "client"},
		{"Cursor", agy, "cursor", "cursor", "client"},
		{"codex-mcp-client", claude, "", "codex", "client"},
	} {
		h, a, rec, _, _ := harnessOf(callIn{Client: c.client, Meta: c.meta})
		via := ""
		if rec.Via != nil {
			via = *rec.Via
		}
		if h != c.harness || a != c.agent || via != c.via || (rec.Harness == nil) != (c.via == "") {
			t.Errorf("harnessOf(%q, %v) = %q, %q, %v; want %q, %q, via %q", c.client, c.meta, h, a, rec, c.harness, c.agent, c.via)
		}
	}
}

// threadOf: the conversation a call names, for harnesses that name it.
func TestThreadOf(t *testing.T) {
	for _, c := range []struct {
		harness string
		meta    map[string]any
		want    string
	}{
		{harnessCodex, map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": "t1"}}, "t1"},
		{"antigravity", map[string]any{"antigravity.google/conversation_id": "9cb9", "progressToken": "x:4"}, "9cb9"},
		{"copilot", map[string]any{"wakeshift/conversation": "1b47", "progressToken": 1}, "1b47"},
		{"copilot", map[string]any{"progressToken": 1}, ""}, // without Wakeshift's hooks
		{"cursor", map[string]any{"wakeshift/conversation": "spoof"}, ""},
		{"", map[string]any{"antigravity.google/conversation_id": "9cb9"}, ""},
	} {
		if got := threadOf(c.harness, c.meta); got != c.want {
			t.Errorf("threadOf(%q, %v) = %q, want %q", c.harness, c.meta, got, c.want)
		}
	}
}

func TestDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{`300`: 5 * time.Minute, `"5m"`: 5 * time.Minute, `"30d"`: 30 * 24 * time.Hour, `"90s"`: 90 * time.Second, `1.5`: 1500 * time.Millisecond} {
		var d duration
		if err := json.Unmarshal([]byte(in), &d); err != nil || time.Duration(d) != want {
			t.Errorf("%s: %v %v", in, time.Duration(d), err)
		}
	}
	for _, in := range []string{`"soon"`, `"xd"`, `true`} {
		var d duration
		if err := json.Unmarshal([]byte(in), &d); err == nil {
			t.Errorf("%s parsed", in)
		}
	}
}

func TestTopicFilters(t *testing.T) {
	f, err := topicFilters("ci/x", map[string]json.RawMessage{"branch": json.RawMessage(`"main"`), "pr": json.RawMessage(`42.0`), "draft": json.RawMessage(`false`)}, true)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"$topic:ci/x": "1", "$data.branch": `"main"`, "$data.pr": "42", "$data.draft": "false", "$once": "true"}
	if len(f) != len(want) {
		t.Fatalf("%v", f)
	}
	for k, v := range want {
		if f[k] != v {
			t.Errorf("%s = %q, want %q", k, f[k], v)
		}
	}
	if !dataMatches("ci", "ci/x", f, []byte(`{"branch":"main","pr":42,"draft":false,"other":[1]}`)) {
		t.Error("an event with the fields didn't match")
	}
	for _, data := range []string{`{"branch":"dev","pr":42,"draft":false}`, `{"branch":"main","pr":"42","draft":false}`, `{"branch":"main"}`, ``, `[1]`} {
		if dataMatches("ci", "ci/x", f, []byte(data)) {
			t.Errorf("%s matched", data)
		}
	}
	if dataMatches("ci", "ci/y", f, []byte(`{"branch":"main","pr":42,"draft":false}`)) {
		t.Error("another topic matched")
	}
	for _, bad := range []map[string]json.RawMessage{{"x": json.RawMessage(`{"a":1}`)}, {"x": json.RawMessage(`[1]`)}, {"": json.RawMessage(`1`)}} {
		if _, err := topicFilters("ci/x", bad, false); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

// A duration too large to convert is refused, not wrapped into one that
// passes the caller's limits.
func TestDurationRange(t *testing.T) {
	for in, want := range map[string]time.Duration{`300`: 5 * time.Minute, `"5m"`: 5 * time.Minute, `"30d"`: 30 * 24 * time.Hour} {
		var d duration
		if err := json.Unmarshal([]byte(in), &d); err != nil || time.Duration(d) != want {
			t.Errorf("%s: %v %v", in, time.Duration(d), err)
		}
	}
	// 213504 days is just over 2^64 ns: it used to wrap to about 25 minutes.
	for _, in := range []string{`1e300`, `-1e300`, `9.3e9`, `"213504d"`, `"106752d"`, `"-1d"`, `"99999999999h"`} {
		var d duration
		if err := json.Unmarshal([]byte(in), &d); err == nil {
			t.Errorf("%s: accepted as %v", in, time.Duration(d))
		}
	}
}

// The agent's claim (harness): it refines the recognized harness within
// its family (the conversation keeps Cursor's wake path and its agent's
// name), names the harness of a call nothing recognized (the agent keeps
// the client's name), and is ignored where it contradicts recognition.
// Detection order stays client name, then _meta, then User-Agent.
func TestHarnessOfClaims(t *testing.T) {
	codex := map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": "t1"}}
	for _, c := range []struct {
		call                  callIn
		harness, agent, final string // final: the harness reported (agent.harness)
		via, variant          string
	}{
		{callIn{Client: "Cursor", Harness: "Grok Bot"}, "cursor", "cursor", "grok-bot", "client", "grok-bot"},
		{callIn{UserAgent: "Cursor/1.0.0", Harness: "grok"}, "cursor", "cursor", "grok-bot", "user_agent", "grok-bot"},
		{callIn{Client: "Cursor", Harness: "cursor-cloud"}, "cursor", "cursor", "cursor-cloud", "client", "cursor-cloud"},
		{callIn{Client: "Cursor", Harness: "claude-code"}, "cursor", "cursor", "cursor", "client", ""},
		{callIn{Client: "claude-code", Harness: "grok-bot"}, harnessClaudeCode, "claude-code", "claude-code", "client", ""},
		{callIn{Client: "claude-code", Meta: codex, Harness: "codex"}, harnessClaudeCode, "claude-code", "claude-code", "client", ""}, // the client name wins, claim or not
		{callIn{Meta: codex, Harness: "claude-code"}, harnessCodex, "codex", "codex", "meta", ""},
		{callIn{Harness: "claude-code"}, harnessClaudeCode, "agent", "claude-code", "claim", ""},
		{callIn{Client: "my-client", Harness: "Grok Bot"}, "cursor", "my-client", "grok-bot", "claim", "grok-bot"},
		{callIn{Client: "my-client", Harness: "my-client"}, "", "my-client", "", "", ""},
		{callIn{Client: "mcp", Harness: "opencode"}, "opencode", "mcp", "opencode", "claim", ""},
		// Hermes Agent's own client name (0.22.5) names it surely: a claim
		// can't move it elsewhere, and the agent keeps the client's name.
		{callIn{Client: "hermes-agent"}, "hermes", "hermes-agent", "hermes", "client", ""},
		{callIn{Client: "hermes-agent", Harness: "hermes"}, "hermes", "hermes-agent", "hermes", "client", ""},
		{callIn{Client: "hermes-agent", Harness: "claude-code"}, "hermes", "hermes-agent", "hermes", "client", ""},
		{callIn{Client: "mcp", Harness: "hermes"}, "hermes", "mcp", "hermes", "claim", ""},
		// What Hermes's plugin passes where the app sees no client name.
		{callIn{UserAgent: "python-httpx2/2.12.0", Harness: "hermes"}, "hermes", "agent", "hermes", "claim", ""},
		{callIn{Client: "cursor-nightly", Harness: "cursor"}, "cursor", "cursor", "cursor", "client", ""}, // a variant, confirmed
		{callIn{Client: "cursor-nightly"}, "", "cursor", "cursor", "client", ""},
	} {
		h, a, rec, variant, _ := harnessOf(c.call)
		final, via := "", ""
		if rec.Harness != nil {
			final, via = *rec.Harness, *rec.Via
		}
		if h != c.harness || a != c.agent || final != c.final || via != c.via || variant.ID != c.variant {
			t.Errorf("harnessOf(%+v) = %q, %q, %q via %q, variant %q; want %q, %q, %q via %q, variant %q",
				c.call, h, a, final, via, variant.ID, c.harness, c.agent, c.final, c.via, c.variant)
		}
		if c.call.Harness != "" && (rec.Claimed == nil || *rec.Claimed == "") {
			t.Errorf("%+v: claimed not reported", c.call)
		}
	}
}

// An emit's refusals name the request's own fields: text, id and payload.
func TestEmitCheckNamesTheRequestsFields(t *testing.T) {
	long := strings.Repeat("x", 201)
	for _, c := range []struct {
		ev   topicEvent
		want string
	}{
		{topicEvent{Text: " "}, "text is required"},
		{topicEvent{Text: "hi", ID: long}, "id is at most"},
		{topicEvent{Text: "hi", Data: json.RawMessage(`[1]`)}, "payload is a JSON object"},
		{topicEvent{Text: "hi", Data: json.RawMessage(`{"a":"` + strings.Repeat("x", maxTopicData) + `"}`)}, "payload is at most"},
	} {
		err := c.ev.check()
		if err == nil || !strings.HasPrefix(err.Error(), c.want) {
			t.Errorf("check() = %v, want it to start %q", err, c.want)
		}
	}
}

// A text over maxTopicText bytes is cut at a character boundary and ends
// with an ellipsis, at most maxTopicText bytes in all; one that fits is
// left as it is.
func TestTrimText(t *testing.T) {
	ascii := func(n int) string { return strings.Repeat("a", n) }
	for _, c := range []struct {
		name string
		text string
		want string // "" for unchanged
	}{
		{"empty", "", ""},
		{"exactly the limit", ascii(maxTopicText), ""},
		{"exactly the limit, multi-byte", ascii(maxTopicText-4) + "😀", ""},
		{"one byte over", ascii(maxTopicText + 1), ascii(maxTopicText-3) + "…"},
		{"far over", ascii(5000), ascii(maxTopicText-3) + "…"},
		// The cut is at byte 997: a character that starts before it and ends
		// after it is dropped whole.
		{"2-byte character straddling the cut", ascii(996) + "é" + ascii(10), ascii(996) + "…"},
		{"2-byte character ending at the cut", ascii(995) + "é" + ascii(10), ascii(995) + "é…"},
		{"3-byte character straddling the cut", ascii(995) + "€" + ascii(10), ascii(995) + "…"},
		{"3-byte character ending at the cut", ascii(994) + "€" + ascii(10), ascii(994) + "€…"},
		{"4-byte emoji straddling the cut", ascii(994) + "😀" + ascii(10), ascii(994) + "…"},
		{"4-byte emoji ending at the cut", ascii(993) + "😀" + ascii(10), ascii(993) + "😀…"},
		{"all emoji", strings.Repeat("😀", 300), strings.Repeat("😀", 249) + "…"},
		{"all 3-byte", strings.Repeat("€", 400), strings.Repeat("€", 332) + "…"},
	} {
		got, trimmed := trimText(c.text)
		want := c.want
		if want == "" {
			want = c.text
		}
		if got != want || trimmed != (c.want != "") {
			t.Errorf("%s: trimText = %d bytes (trimmed %v), want %d bytes (trimmed %v)", c.name, len(got), trimmed, len(want), c.want != "")
		}
		if len(got) > maxTopicText || !utf8.ValidString(got) {
			t.Errorf("%s: %d bytes, valid UTF-8 %v", c.name, len(got), utf8.ValidString(got))
		}
	}
}

// check trims a long text, and says so, rather than refusing it; a payload
// over its limit is still refused.
func TestEmitCheckTrimsText(t *testing.T) {
	ev := topicEvent{Text: strings.Repeat("x", maxTopicText+1)}
	if err := ev.check(); err != nil || !ev.trimmed || len(ev.Text) != maxTopicText || !strings.HasSuffix(ev.Text, "…") {
		t.Fatalf("check() = %v, trimmed %v, %d bytes", err, ev.trimmed, len(ev.Text))
	}
	ev = topicEvent{Text: strings.Repeat("x", maxTopicText)}
	if err := ev.check(); err != nil || ev.trimmed || len(ev.Text) != maxTopicText {
		t.Fatalf("check() at the limit = %v, trimmed %v, %d bytes", err, ev.trimmed, len(ev.Text))
	}
	ev = topicEvent{Text: strings.Repeat("x", 2000), Data: json.RawMessage(`{"a":"` + strings.Repeat("x", maxTopicData) + `"}`)}
	if err := ev.check(); err == nil || !strings.HasPrefix(err.Error(), "payload is at most") {
		t.Fatalf("a long payload with a long text: %v", err)
	}
}
