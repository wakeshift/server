package coretest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/pkg/wakeshift"
	"github.com/wakeshift/server/pkg/wakeshift/mcpgo"
)

// The Go SDK on an MCP server built with mark3labs/mcp-go (wakeshift/mcpgo),
// end to end against a real core, as clients on protocol 2026-07-28 call
// it: server/discover and tools/list through Wrap, a wrapped tool's block,
// and a delivery: Claude Code, which takes MCP tasks, waits with the wait
// tool and its task completes with the event; Codex gets its beacon.

type mcpgoApp struct {
	app  *wakeshift.Client
	turn *wakeshift.Event[tableArgs, turn]
	url  string
}

// newMCPGoApp serves an app with a personal game.turn on an mcp-go server,
// minimal as mcp-go's README builds one, behind bearer tokens that name
// the user.
func newMCPGoApp(t *testing.T, c *coretest.Core, id string) *mcpgoApp {
	t.Helper()
	return newMCPGoAppWait(t, c, id, true)
}

// newMCPGoAppWait is newMCPGoApp, serving the wait tool only with wait
// (AttachOptions.Wait).
func newMCPGoAppWait(t *testing.T, c *coretest.Core, id string, wait bool) *mcpgoApp {
	t.Helper()
	m := &mcpgoApp{app: bareApp(t, c, id)}
	var err error
	if m.turn, err = wakeshift.DefineEvent(m.app, "game.turn", wakeshift.EventDef[tableArgs, turn]{
		Description: "It's your move.", Personal: true,
		Text: func(tu turn, a tableArgs) string { return "Your move at " + a.Table + ": " + tu.ToCall + " to call" },
	}); err != nil {
		t.Fatal(err)
	}
	srv := server.NewMCPServer(id, "1.0.0", server.WithToolCapabilities(false))
	at := mcpgo.Attach(m.app, srv, wakeshift.AttachOptions{BuiltinTools: wakeshift.BuiltinTools{Wait: wait}, Identify: identifyUser})
	mcpgo.AddTool(at, mcp.NewTool("join", mcp.WithDescription("Sit at a table"), mcp.WithString("table", mcp.Required(), mcp.Description("The table"))),
		func(ctx context.Context, call *wakeshift.ToolCall, _ mcp.CallToolRequest, in wrappedIn) (*mcp.CallToolResult, error) {
			if _, err := m.turn.Subscribe(ctx, call, tableArgs{Table: in.Table}, wakeshift.Reason("Playing at "+in.Table)); err != nil {
				return nil, err
			}
			return mcp.NewToolResultText("Seated at " + in.Table + "."), nil
		}, wakeshift.Subscribes(m.turn))
	verify := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		return &auth.TokenInfo{UserID: token, Expiration: time.Now().Add(time.Hour)}, nil
	}
	hs := httptest.NewServer(wakeshift.RequireBearer(verify, "")(at.Handler(server.NewStreamableHTTPServer(srv))))
	t.Cleanup(hs.Close)
	m.url = hs.URL
	return m
}

// modernClient is a client on protocol 2026-07-28: its name, User-Agent,
// whether it takes MCP tasks, and _meta it adds.
type modernClient struct {
	name, ua string
	tasks    bool
	meta     map[string]any
}

// rpc sends one 2026-07-28 request as user, and returns its result.
func (m *mcpgoApp) rpc(t *testing.T, c modernClient, user, method string, params map[string]any) map[string]any {
	t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	caps := map[string]any{}
	if c.tasks {
		caps["extensions"] = map[string]any{tasksExtension: map[string]any{}}
	}
	meta := map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28",
		"io.modelcontextprotocol/clientInfo":         map[string]any{"name": c.name, "version": "1"},
		"io.modelcontextprotocol/clientCapabilities": caps}
	for k, v := range c.meta {
		meta[k] = v
	}
	params["_meta"] = meta
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest("POST", m.url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+user)
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", method)
	if name, ok := params["name"].(string); ok {
		req.Header.Set("Mcp-Name", name)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	for _, line := range strings.Split(string(raw), "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			raw = []byte(data)
		}
	}
	var out struct {
		Result map[string]any `json:"result"`
		Error  any            `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Result == nil {
		t.Fatalf("%s: %d %s", method, resp.StatusCode, raw)
	}
	return out.Result
}

func mcpgoText(res map[string]any) string {
	var parts []string
	content, _ := res["content"].([]any)
	if r, ok := res["result"].(map[string]any); ok {
		content, _ = r["content"].([]any)
	}
	for _, c := range content {
		m, _ := c.(map[string]any)
		s, _ := m["text"].(string)
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n\n")
}

// tasksExtension is the MCP tasks extension's capability key.
const tasksExtension = "io.modelcontextprotocol/tasks"

// Claude Code names the tool call in every call's _meta, as it always does
// on a local route (a call with none can't carry a beacon its plugin
// redeems, so it gets the Monitor alone: pluginConnectable).
var claudeCodeTasks = modernClient{name: "claude-code", ua: "claude-code/2.1.280", tasks: true, meta: map[string]any{"claudecode/toolUseId": "toolu_mcpgo1"}}

// Claude Code on 2026-07-28, which takes tasks: discover and tools/list
// carry what 2026-07-28 requires and what Wrap adds; the wrapped tool's
// block tells it to call the wait tool; the wait tool's task completes
// with the emitted event.
func TestMCPGoClaudeCodeTasks(t *testing.T) {
	c := coretest.Start(t)
	m := newMCPGoApp(t, c, "cards")

	disc := m.rpc(t, claudeCodeTasks, "ada", "server/discover", nil)
	caps := disc["capabilities"].(map[string]any)
	ext, _ := caps["extensions"].(map[string]any)
	if ext["io.modelcontextprotocol/events"] == nil || ext[tasksExtension] == nil || caps["tools"] == nil {
		t.Fatalf("discover's capabilities: %v", caps)
	}
	versions, _ := disc["supportedVersions"].([]any)
	if disc["resultType"] != "complete" || disc["ttlMs"] == nil || disc["cacheScope"] == nil || len(versions) == 0 || versions[0] != "2026-07-28" {
		t.Fatalf("discover: %v", disc)
	}

	list := m.rpc(t, claudeCodeTasks, "ada", "tools/list", nil)
	if list["resultType"] != "complete" || list["ttlMs"] == nil || list["cacheScope"] == nil {
		t.Fatalf("tools/list: %v", list)
	}
	names := map[string]map[string]any{}
	for _, tl := range list["tools"].([]any) {
		names[tl.(map[string]any)["name"].(string)] = tl.(map[string]any)
	}
	for _, want := range []string{"join", "subscribe_events", "unsubscribe_events", "list_subscriptions", harnesses.WaitTool} {
		if names[want] == nil {
			t.Fatalf("tools/list has no %s: %v", want, list["tools"])
		}
	}
	props := names["join"]["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if props["table"] == nil || props[harnesses.SessionArg] == nil || props[harnesses.HarnessArg] == nil {
		t.Fatalf("join's input schema: %v", props)
	}

	join := m.rpc(t, claudeCodeTasks, "ada", "tools/call", map[string]any{"name": "join", "arguments": map[string]any{"table": "oak", "harness": "claude-code"}})
	text := mcpgoText(join)
	if join["isError"] == true || !strings.HasPrefix(text, "Seated at oak.\n\n") || !strings.Contains(text, "call "+harnesses.WaitTool) {
		t.Fatalf("join: %v", join)
	}
	subs, err := m.app.EventSubscriptions(ctx, "ada")
	if err != nil || len(subs) != 1 || subs[0].Event != "game.turn" {
		t.Fatalf("subscriptions: %+v %v", subs, err)
	}
	session := between(text, `session_key="`, `"`)
	if session == "" {
		t.Fatalf("no session_key in the block: %s", text)
	}

	task := m.rpc(t, claudeCodeTasks, "ada", "tools/call", map[string]any{"name": harnesses.WaitTool, "arguments": map[string]any{"session_key": session}})
	id, _ := task["taskId"].(string)
	if task["resultType"] != "task" || !strings.HasPrefix(id, "wkt1_") {
		t.Fatalf("the wait tool's call: %v", task)
	}
	if got, err := m.turn.Emit(ctx, tableArgs{Table: "oak"}, turn{ToCall: "20"}, wakeshift.To("ada")); err != nil || got.Matched != 1 {
		t.Fatalf("emit: %+v %v", got, err)
	}
	var done map[string]any
	waitFor(t, "the task completes", func() bool {
		done = m.rpc(t, claudeCodeTasks, "ada", "tasks/get", map[string]any{"taskId": id})
		return done["status"] == "completed"
	})
	if !strings.Contains(mcpgoText(done), "Your move at oak: 20 to call") {
		t.Fatalf("the completed task: %v", done)
	}
}

// Without AttachOptions.Wait (the default), the same Claude Code, whose
// requests take tasks, sees no wait tool and no tasks extension, and the
// wrapped tool's block offers the plugin with the Monitor as the
// fallback: nothing names the wait tool.
func TestMCPGoClaudeCodeWithoutWait(t *testing.T) {
	c := coretest.Start(t)
	m := newMCPGoAppWait(t, c, "cards", false)

	disc := m.rpc(t, claudeCodeTasks, "ada", "server/discover", nil)
	ext, _ := disc["capabilities"].(map[string]any)["extensions"].(map[string]any)
	if ext["io.modelcontextprotocol/events"] == nil || ext[tasksExtension] != nil {
		t.Fatalf("discover's capabilities: %v", disc["capabilities"])
	}
	for _, tl := range m.rpc(t, claudeCodeTasks, "ada", "tools/list", nil)["tools"].([]any) {
		if tl.(map[string]any)["name"] == harnesses.WaitTool {
			t.Fatalf("tools/list has %s", harnesses.WaitTool)
		}
	}
	join := m.rpc(t, claudeCodeTasks, "ada", "tools/call", map[string]any{"name": "join", "arguments": map[string]any{"table": "oak", "harness": "claude-code"}})
	text := mcpgoText(join)
	if join["isError"] == true || !strings.HasPrefix(text, "Seated at oak.\n\n") {
		t.Fatalf("join: %v", join)
	}
	if strings.Contains(text, harnesses.WaitTool) {
		t.Fatalf("the block names %s: %s", harnesses.WaitTool, text)
	}
	for _, want := range []string{"\"" + harnesses.AskSession + "\" (description: starts a background listener (the Monitor)", "\"" + harnesses.AskEvery + "\""} {
		if !strings.Contains(text, want) {
			t.Fatalf("the block doesn't say %q: %s", want, text)
		}
	}
}

// Codex on 2026-07-28 names its thread in _meta: the wrapped tool's reply
// carries the beacon its hook reads, in _meta, and the install offer.
func TestMCPGoCodexBeacon(t *testing.T) {
	c := coretest.Start(t)
	m := newMCPGoApp(t, c, "cards")
	codex := modernClient{name: "codex-mcp-client", ua: "codex_exec/0.50.0", meta: map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": "thread-1"}}}
	res := m.rpc(t, codex, "ada", "tools/call", map[string]any{"name": "join", "arguments": map[string]any{"table": "oak"}})
	meta, _ := res["_meta"].(map[string]any)
	if res["isError"] == true || meta[beacon.MetaKey] == nil {
		t.Fatalf("join: %v", res)
	}
	text := mcpgoText(res)
	if !strings.HasPrefix(text, "Seated at oak.\n\n") || !strings.Contains(text, "install Wakeshift's hooks for Codex") || strings.Count(text, "wakeshift-beacon") > 1 {
		t.Fatalf("join's text: %s", text)
	}
	if subs, err := m.app.EventSubscriptions(ctx, "ada"); err != nil || len(subs) != 1 {
		t.Fatalf("subscriptions: %+v %v", subs, err)
	}
}

// between is the text in s between the first from and the next to.
func between(s, from, to string) string {
	_, rest, ok := strings.Cut(s, from)
	if !ok {
		return ""
	}
	out, _, _ := strings.Cut(rest, to)
	return out
}
