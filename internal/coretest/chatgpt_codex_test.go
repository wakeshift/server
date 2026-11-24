package coretest_test

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/connectorversion"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/pluginmark"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// chatgptCodexMeta is a tools/call _meta as ChatGPT's Codex mode sends it (as
// observed: keys only, made-up values), with its
// capabilities.
func chatgptCodexMeta(session string) map[string]any {
	return map[string]any{
		"sessionId":                          session,
		"windowId":                           session + ":0",
		"callId":                             "call-1",
		"openai/session":                     "v1/chat-" + session,
		"io.modelcontextprotocol/clientInfo": map[string]any{"name": "openai-mcp (Codex)", "version": "1.0.0"},
		"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
		"io.modelcontextprotocol/clientCapabilities": capsJSON(desktopCaps),
	}
}

// foregroundCmdRE is the command ChatGPT's Codex mode is given
// (harnesses.ForegroundLoop: one round of its ForegroundTimeout, reading
// the link file), and the url it waits on; finding it writes the link
// file, as the agent's file tool would.
var foregroundCmdRE = cmdRE{re: regexp.MustCompile(`run this command now and let it run: (f=\$HOME/\.wakeshift/links/[0-9a-f]{16}; [^\n]*?u="\$b&timeout=[0-9]+s"; c=; s=\$\(date \+%s\); for i in 1 2; do if r=\$\(curl [^\n]*?; done; fi) \.`)}

// roundRE is a foreground command's round: the wait's timeout and curl's
// limit, in seconds.
var roundRE = regexp.MustCompile(`u="\$b&timeout=([0-9]+)s"; c=; s=\$\(date \+%s\); for i in 1 2; do if r=\$\(curl -s -m ([0-9]+) `)

// withKey is the call passing a session key.
func withKey(c caller, key string) caller {
	c.Session = key
	return c
}

// shortRound is a foreground command waiting 1s a round rather than its
// ForegroundTimeout (curl's limit to match), so a quiet round ends at once.
func shortRound(t *testing.T, cmd string) string {
	t.Helper()
	m := roundRE.FindStringSubmatch(cmd)
	if m == nil {
		t.Fatalf("no round in %s", cmd)
	}
	return strings.Replace(cmd, m[0], `u="$b&timeout=1s"; c=; s=$(date +%s); for i in 1 2; do if r=$(curl -s -m 10 `, 1)
}

// foregroundRun runs a foreground command once, as ChatGPT's agent does,
// and returns the lines it printed (as JSON).
func foregroundRun(t *testing.T, cmd string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range runCommand(t, cmd) {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not a JSON line: %q", l)
		}
		out = append(out, m)
	}
	return out
}

// restarted is whether lines is just the hub.restart line a quiet round
// ends with.
func restarted(lines []map[string]any) bool {
	if len(lines) != 1 || lines[0]["type"] != "hub.restart" {
		return false
	}
	n, _ := lines[0]["note"].(string)
	return strings.Contains(n, "Run the same command again now") && strings.Contains(n, "nothing was missed")
}

// ChatGPT's Codex mode ("openai-mcp (Codex)") waits in the foreground, as
// Grok Bot does, but with a command whose one round (90 seconds) ends
// inside ChatGPT's 120-second cutoff: it writes the link file once (the
// link is in no command), and the same command, run again every time,
// prints and confirms what arrived, or ends a quiet round with a
// hub.restart line. ChatGPT's Work chats ("openai-mcp") keep their line.
// Its "(Codex)" never makes it Codex. Each run hands out only what's new;
// a forged sessionId, a call without the key or another user's key
// reaches nothing of the conversation's.
func TestChatGPTCodexForeground(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	const sid = "6f1d2c3b-4a59-4e8f-9d7c-0b1a2c3d4e5f"
	cora := caller{User: "cora", Client: "openai-mcp (Codex)", UserAgent: "openai-mcp/1.0.0 (Codex)", Meta: chatgptCodexMeta(sid)}

	sub, err := subscribe(app, cora, "ci/cora", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if sub.Addition.Agent != (wakeshift.Recognized{Harness: "chatgpt-codex", Via: "client"}) {
		t.Fatalf("agent: %+v", sub.Addition.Agent)
	}
	text := sub.Addition.Text
	m := foregroundCmdRE.FindStringSubmatch(text)
	if m == nil || !strings.HasPrefix(text, "ChatGPT (Codex mode) can't be woken by a command in the background, so to get Ci's messages as they arrive, wait for them in the foreground. If you haven't written this conversation's link to the file ~/.wakeshift/links/") ||
		!strings.Contains(text, "you're blocked on that command, but the user can still talk to you") ||
		!strings.Contains(text, `or after 90 seconds with only a last line ({"type":"hub.restart",…})`) ||
		strings.Contains(text, "can't be woken for this subscription") || strings.Contains(text, "wakeshift-beacon") || strings.Contains(text, "printenv") {
		t.Fatalf("subscribe: %s", text)
	}
	if !strings.Contains(text, "Run the same command again every time, right away: after you've handled the messages it printed, and after it ended with nothing") || !strings.Contains(text, "keep reading its output until it ends") {
		t.Fatalf("the rerun rule: %s", text)
	}
	if !strings.Contains(text, sub.Session) {
		t.Fatalf("the result doesn't hand out the session key: %s", text)
	}
	// No command holds the link, and nothing asks the wait for a command
	// to print (say=command would print one holding the link).
	cmd, link := m[1], strings.SplitN(m[2], "&", 2)[0]
	if coretest.LeaksLink(text) || strings.Contains(cmd, link) || strings.Contains(text, "say=command") || strings.Contains(text, "curl -sN") {
		t.Fatalf("a link outside the link step: %s", text)
	}
	if got := readLinkFile(t, link); got != link {
		t.Fatalf("the link file holds %q, not the plain link", got)
	}
	// Each round ends well inside ChatGPT's 120-second cutoff.
	r := roundRE.FindStringSubmatch(cmd)
	if r == nil {
		t.Fatalf("no round: %s", cmd)
	}
	wait, _ := strconv.Atoi(r[1])
	limit, _ := strconv.Atoi(r[2])
	if wait <= 0 || wait > 95 || limit < wait || limit > 110 || strings.Count(cmd, "timeout=") != 1 {
		t.Fatalf("round: timeout=%ds, curl -m %d: %s", wait, limit, cmd)
	}

	// The model's codex claim doesn't make it Codex; a Work chat stays
	// ChatGPT, with its line and no command.
	claimed, _ := subscribe(app, withKey(cora.WithHarness("codex"), sub.Session), "ci/cora", subOpts{})
	if claimed.Session != sub.Session || claimed.Addition.Agent.Harness != "chatgpt-codex" || claimed.Addition.Agent.Claimed != "codex" ||
		!strings.Contains(claimed.Addition.Text, "in the foreground") || strings.Contains(claimed.Addition.Text, "codex plugin") {
		t.Fatalf("claimed codex: %+v", claimed)
	}
	if cm := foregroundCmdRE.FindStringSubmatch(claimed.Addition.Text); cm == nil || cm[1] != cmd {
		t.Fatalf("subscribing again changed the command: %s", claimed.Addition.Text)
	}
	// A Work chat on the web sends the same client name, with its own
	// capabilities: ChatGPT's line, as before.
	webMeta := chatgptCodexMeta("0a1b2c3d-0000-4000-8000-000000000001")
	webMeta["io.modelcontextprotocol/clientCapabilities"] = capsJSON(webCaps)
	webMeta["traceparent"] = "00-x"
	for _, w := range []caller{
		{User: "wanda", Client: "openai-mcp", UserAgent: "openai-mcp/1.0.0 (Codex)", Meta: map[string]any{"openai/session": "v1/work"}},
		{User: "wendy", Client: "openai-mcp (Codex)", UserAgent: "openai-mcp/1.0.0 (Codex)", Meta: webMeta},
	} {
		work, _ := subscribe(app, w, "ci/"+w.User, subOpts{})
		if work.Addition.Agent.Harness != "chatgpt" || !strings.Contains(work.Addition.Text, "ChatGPT can't be woken for this subscription") || strings.Contains(work.Addition.Text, "curl") {
			t.Fatalf("work chat %s: %+v", w.User, work)
		}
	}

	// The command, run from the link file as the agent wrote it: a message
	// that arrived is printed and confirmed before it ends; the same
	// command again ends its quiet round with the restart line (nothing
	// comes twice); what arrives next is printed once, not redelivered.
	tenant, _ := topicSubRow(t, c, sub.ID)
	acked := func(id any) int {
		return count(t, c, tenant, `SELECT count(*) FROM deliveries WHERE event_id = $1 AND state = 'acked'`, id)
	}
	run := shortRound(t, cmd)
	emitNote(app, []string{"ci/cora"}, note{Text: "first"})
	got := foregroundRun(t, run)
	if len(got) != 1 || got[0]["type"] != "event" || got[0]["subject"] != "first" || got[0]["redelivered"] == true {
		t.Fatalf("first run: %v", got)
	}
	if acked(got[0]["id"]) != 1 {
		t.Fatal("the first message wasn't confirmed before the command ended")
	}
	if quiet := foregroundRun(t, run); !restarted(quiet) {
		t.Fatalf("a confirmed message came again, or no restart line: %v", quiet)
	}
	emitNote(app, []string{"ci/cora"}, note{Text: "second"})
	again := foregroundRun(t, run)
	if len(again) != 1 || again[0]["subject"] != "second" || again[0]["redelivered"] == true || again[0]["id"] == got[0]["id"] {
		t.Fatalf("second run: %v", again)
	}
	if acked(again[0]["id"]) != 1 {
		t.Fatal("the second message wasn't confirmed")
	}
	// The cursor moved past both: nothing more comes.
	if quiet := foregroundRun(t, run); !restarted(quiet) {
		t.Fatalf("after the second: %v", quiet)
	}
	if got := readLinkFile(t, link); got != link {
		t.Fatalf("the link file now holds %q", got)
	}

	// Another user sending the same sessionId gets a conversation of its
	// own: its command (its own link file) sees nothing of cora's. So does
	// cora's own call without her key (sessionId never names a
	// conversation). Cora's key in mallory's call names nothing.
	mallory := caller{User: "mallory", Client: "openai-mcp (Codex)", UserAgent: "openai-mcp/1.0.0 (Codex)", Meta: chatgptCodexMeta(sid)}
	forged, err := subscribe(app, mallory, "ci/mallory", subOpts{})
	if err != nil || forged.Session == sub.Session {
		t.Fatalf("forged sessionId: %+v %v", forged, err)
	}
	fm := foregroundCmdRE.FindStringSubmatch(forged.Addition.Text)
	if fm == nil || fm[1] == cmd || fm[2] == m[2] {
		t.Fatalf("forged's command: %s", forged.Addition.Text)
	}
	emitNote(app, []string{"ci/cora"}, note{Text: "only for cora"})
	if got := foregroundRun(t, shortRound(t, fm[1])); !restarted(got) {
		t.Fatalf("the forged conversation's command got: %v", got)
	}
	keyless, _ := subscribe(app, cora, "ci/cora", subOpts{})
	if keyless.Session == sub.Session {
		t.Fatal("a call without the key found cora's conversation by its sessionId")
	}
	if km := foregroundCmdRE.FindStringSubmatch(keyless.Addition.Text); km == nil || km[1] == cmd {
		t.Fatalf("the keyless call's command: %s", keyless.Addition.Text)
	}
	stolen, _ := subscribe(app, withKey(mallory, sub.Session), "ci/mallory", subOpts{})
	if stolen.Session == sub.Session || !strings.Contains(stolen.Addition.Text, "isn't known") || strings.Contains(stolen.Addition.Text, link) {
		t.Fatalf("stolen key: %+v", stolen)
	}
	// Cora's message waits for her own next run, and comes once.
	if got := foregroundRun(t, run); len(got) != 1 || got[0]["subject"] != "only for cora" || got[0]["redelivered"] == true {
		t.Fatalf("cora's: %v", got)
	}
	if got := foregroundRun(t, run); !restarted(got) {
		t.Fatalf("cora's again: %v", got)
	}
}

// A Codex thread calling the app through ChatGPT's hosted connector
// (Codex Desktop's codex_apps server, mcp__codex_apps__<app>_watch, as
// observed) passes the mark of
// Wakeshift's Codex plugin, but its call is ChatGPT's (client "openai-mcp
// (Codex)", the desktop app's capabilities, no x-codex-turn-metadata), and
// the plugin's hook can't connect it from there. Its result never says the
// plugin connects it and carries no beacon: it's the foreground wait, then
// how to get background wakes (add the app to Codex directly). Without the
// desktop app's capabilities, ChatGPT's can't-wake note, then the same.
// No mark, or a mark from a connector that trusts another Wakeshift or
// that the core no longer serves: no such sentence. A Codex call of its
// own keeps the plugin's note.
func TestChatGPTCodexAppsPluginMark(t *testing.T) {
	c := coretest.StartWith(t, func(o *core.Options) {
		o.Connectors = connectorversion.Minimums{"wakeshift": "2026.10.20"}
	})
	app := noteApp(t, c, "ci")
	mark := pluginmark.Value("codex", "2026.11.10.0730", []string{c.URL})
	const via = "can't connect this thread through ChatGPT's app connector (the mcp__codex_apps__ tools)"
	const local = "add Ci's MCP server to Codex directly"
	desk := func(user, sid, harness string) caller {
		return caller{User: user, Client: "openai-mcp (Codex)", UserAgent: "openai-mcp/1.0.0 (Codex)", Tool: "watch", Meta: chatgptCodexMeta(sid)}.WithHarness(harness)
	}
	promises := []string{"wakeshift-beacon", "plugin for Codex is installed here", "will wake", "wakes this thread", "from this result"}
	noPromise := func(what, text string) {
		t.Helper()
		for _, no := range promises {
			if strings.Contains(text, no) {
				t.Fatalf("%s says %q: %s", what, no, text)
			}
		}
	}

	sub, err := subscribe(app, desk("cody", "01a1c8ab-ccda-71f1-a736-000000000001", mark), "ci/cody", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	text := sub.Addition.Text
	if sub.Addition.Agent.Harness != "chatgpt-codex" || sub.Addition.Agent.Claimed != "codex" {
		t.Fatalf("agent: %+v", sub.Addition.Agent)
	}
	if foregroundCmdRE.FindStringSubmatch(text) == nil || !strings.Contains(text, "ChatGPT (Codex mode) can't be woken by a command in the background") ||
		!strings.Contains(text, via) || !strings.Contains(text, local) || !strings.Contains(text, sub.Session) {
		t.Fatalf("marked desktop: %s", text)
	}
	// The foreground wait first, then the codex_apps note, then the key.
	if i, j := strings.Index(text, "run this command now"), strings.Index(text, via); i < 0 || j < i || strings.LastIndex(text, sub.Session) < j {
		t.Fatalf("order: %s", text)
	}
	noPromise("the marked desktop result", text)
	if sub.Addition.Meta[beacon.MetaKey] != nil {
		t.Fatalf("a beacon nothing can redeem: %+v", sub.Addition.Meta)
	}

	// Without the desktop app's capabilities (a codex_apps call from
	// another Codex host): ChatGPT's can't-wake note, then the same.
	web := chatgptCodexMeta("01a1c8ab-ccda-71f1-a736-000000000002")
	web["io.modelcontextprotocol/clientCapabilities"] = capsJSON(webCaps)
	other := caller{User: "wade", Client: "openai-mcp (Codex)", UserAgent: "openai-mcp/1.0.0 (Codex)", Tool: "watch", Meta: web}.WithHarness(mark)
	ws, err := subscribe(app, other, "ci/wade", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if ws.Addition.Agent.Harness != "chatgpt" || !strings.HasPrefix(ws.Addition.Text, "ChatGPT can't be woken for this subscription") || !strings.Contains(ws.Addition.Text, via) ||
		strings.Contains(ws.Addition.Text, "curl") || ws.Addition.Meta[beacon.MetaKey] != nil {
		t.Fatalf("marked, not the desktop app: %+v", ws)
	}
	noPromise("the marked ChatGPT result", ws.Addition.Text)

	// No mark, another Wakeshift's, or an outdated connector's: no
	// codex_apps sentence.
	for i, h := range []string{"", "codex",
		pluginmark.Value("codex", "2026.11.10.0730", []string{"https://other.example"}),
		pluginmark.Value("codex", "2026.10.19.0900", []string{c.URL}),
	} {
		sid := "01a1c8ab-ccda-71f1-a736-00000000001" + strconv.Itoa(i)
		r, err := subscribe(app, desk("u"+strconv.Itoa(i), sid, h), "ci/u"+strconv.Itoa(i), subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(r.Addition.Text, "codex_apps") || foregroundCmdRE.FindStringSubmatch(r.Addition.Text) == nil {
			t.Fatalf("harness %q: %s", h, r.Addition.Text)
		}
	}

	// Codex's own MCP client, with a thread: the plugin's note and a beacon.
	direct := caller{User: "dex", Client: "codex-mcp-client", Tool: "watch",
		Meta: map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": "01a1c8bb-8084-72d2-83eb-000000000001"}}}.WithHarness(mark)
	d, err := subscribe(app, direct, "ci/dex", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.Addition.Text, "codex_apps") || !strings.HasPrefix(d.Addition.Text, "Subscribed. Wakeshift's plugin for Codex is installed here") || d.Addition.Meta[beacon.MetaKey] == nil {
		t.Fatalf("direct Codex: %+v", d.Addition)
	}
}

// Through an app on the Go SDK: ChatGPT's desktop app (by its capabilities) is subscribed by the app's tool and told to wait in the
// foreground; a Work chat on the web, with the same client name, User-Agent
// and _meta keys but the web's capabilities, stays native (the MCP Events
// line, no subscription made).
func TestWrappedToolChatGPTDesktopAndWeb(t *testing.T) {
	c := coretest.Start(t)
	w := newWrappedApp(t, c, "cards")
	const ua = "openai-mcp/1.0.0 (Codex)"
	// Without the protocol version, which needs its header on this server.
	meta := func(sid string) map[string]any {
		m := chatgptCodexMeta(sid)
		delete(m, "io.modelcontextprotocol/protocolVersion")
		return m
	}

	desk := w.call(t, "u:dora", ua, "join", map[string]any{"table": "oak"}, meta("6f1d2c3b-4a59-4e8f-9d7c-0b1a2c3d4e5f"))
	if desk.IsError || !strings.Contains(desk.text(), "ChatGPT (Codex mode) can't be woken by a command in the background") ||
		foregroundCmdRE.FindStringSubmatch(desk.text()) == nil || strings.Contains(desk.text(), "(MCP Events)") {
		t.Fatalf("desktop: %+v", desk)
	}
	if subs := w.subsOf("u:dora"); len(subs) != 2 || subs[0].State != wakeshift.SubscriptionCreated || subs[0].Recognized.Harness != "chatgpt-codex" {
		t.Fatalf("desktop subscriptions: %+v", subs)
	}

	web := meta("0a1b2c3d-0000-4000-8000-000000000001")
	web["io.modelcontextprotocol/clientCapabilities"] = capsJSON(webCaps)
	web["traceparent"] = "00-x"
	res := w.call(t, "u:wes", ua, "join", map[string]any{"table": "oak"}, web)
	if res.IsError || !strings.Contains(res.text(), `subscribe to it (MCP Events).`) || strings.Contains(res.text(), "curl") {
		t.Fatalf("web: %+v", res)
	}
	if subs := w.subsOf("u:wes"); len(subs) != 2 || subs[0].State != wakeshift.SubscriptionNative {
		t.Fatalf("web subscriptions: %+v", subs)
	}
	if list, err := w.app.EventSubscriptions(ctx, "wes"); err != nil || len(list) != 0 {
		t.Fatalf("a Work chat was subscribed through the tool: %+v %v", list, err)
	}
	// The desktop app's regular chat (observed): the classic client, which can't subscribe natively and has
	// no shell. It's told plainly it can't be woken: no MCP Events line, no
	// command.
	regular := map[string]any{"openai/session": "v1/chat-r", "openai/userAgent": "Codex Desktop/26.1103.60933 (Mac OS; arm64)",
		"openai/locale": "en-US", "timezone": "UTC", "timezone_offset_minutes": 0,
		"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "openai-mcp", "version": "1.0.0"},
		"io.modelcontextprotocol/clientCapabilities": capsJSON(`{"experimental":{"openai/visibility":{"enabled":true}},"extensions":{"io.modelcontextprotocol/ui":{"mimeTypes":["text/html;profile=mcp-app"]}}}`)}
	reg := w.call(t, "u:rex", "openai-mcp/1.0.0", "join", map[string]any{"table": "oak"}, regular)
	if reg.IsError || !strings.Contains(reg.text(), "ChatGPT can't be woken for this subscription") || strings.Contains(reg.text(), "(MCP Events)") || strings.Contains(reg.text(), "curl") {
		t.Fatalf("desktop regular chat: %+v", reg)
	}
	if subs := w.subsOf("u:rex"); len(subs) != 2 || subs[0].State == wakeshift.SubscriptionNative || subs[0].Recognized.Harness != "chatgpt" {
		t.Fatalf("desktop regular chat subscriptions: %+v", subs)
	}
}

// The capabilities ChatGPT's desktop app and a Work chat on the web sent
// on tools/call, as observed.
const (
	desktopCaps = `{"experimental":{"openai/visibility":{"enabled":true}},"elicitation":{"form":{},"url":{}},"extensions":{"io.modelcontextprotocol/ui":{"mimeTypes":["text/html;profile=mcp-app","text/html+skybridge"]},"openai/elicitation":{"form":{},"userVerification":{}},"openai/form":{}}}`
	webCaps     = `{"experimental":{"openai/visibility":{"enabled":true}},"extensions":{"io.modelcontextprotocol/ui":{"mimeTypes":["text/html;profile=mcp-app"]},"openai/elicitation":{"userVerification":{}}}}`
)

func capsJSON(s string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		panic(err)
	}
	return m
}
