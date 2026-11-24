package coretest_test

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/apps/agentkit"
	"github.com/wakeshift/server/apps/gamekit/gametest"
	"github.com/wakeshift/server/apps/secretagent"
	"github.com/wakeshift/server/internal/coretest"
)

// gameRPC is a JSON-RPC call to a game's MCP endpoint as a guest, from a
// client with User-Agent ua: the result, or the error's message.
func gameRPC(t *testing.T, mcpURL, token, ua, method string, params any) (map[string]any, string) {
	t.Helper()
	raw, status := postRPC(t, mcpURL, bearer(token, ua), 1, method, params)
	var out struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: %d %s", method, status, raw)
	}
	if out.Error != nil {
		return nil, out.Error.Message
	}
	return out.Result, ""
}

// structured is a tools/call result's structured content, failing on an
// error result.
func structured(t *testing.T, res map[string]any, errMsg string) map[string]any {
	t.Helper()
	if errMsg != "" || res["isError"] == true {
		t.Fatalf("tool call failed: %s %v", errMsg, res)
	}
	sc, _ := res["structuredContent"].(map[string]any)
	return sc
}

// resultText is a tools/call result's text content.
func resultText(res map[string]any) string {
	var out []string
	content, _ := res["content"].([]any)
	for _, c := range content {
		if m, ok := c.(map[string]any); ok && m["type"] == "text" {
			out = append(out, m["text"].(string))
		}
	}
	return strings.Join(out, "\n")
}

// A game wakes every harness through game.update, one personal event with
// no arguments: a conversation's (Claude Code) subscription is made by
// join, and a client that subscribes with MCP Events itself (ChatGPT) is
// told by join's result to subscribe, and gets every update of its games
// by webhook from then on: only its own game's, never its own words, and
// nothing of another player's role. A native seat is keyed by ChatGPT's
// conversation (_meta["openai/session"]): another conversation of the same
// agent is another player. game.update takes no arguments, and the games
// have no watch tool.
func TestGameUpdatesForNativeClients(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	mcpURL := srv.URL + "/mcp"
	key := c.RegisterApp("secretagent", "Secret Agent", mcpURL)
	app := &secretagent.App{Kit: &agentkit.App{Client: coretest.SDKClient(t, "secretagent", key, c.URL), ID: "secretagent", Key: key, MCPURL: mcpURL},
		Discussion: 30 * time.Second, Voting: 30 * time.Second, Linger: time.Minute}
	app.Store, _ = gametest.Store(t)
	srv.Config.Handler = app.Handler()
	gametest.Run(t, app.Run)
	if err := app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cb := newFakeChatGPT(t)
	const chatgpt = "openai-mcp/1.0.0"

	gpt := c.AppSignIn(srv.URL, "ChatGPT")
	gptCall := func(conversation, name string, args map[string]any) (map[string]any, string) {
		t.Helper()
		return gameRPC(t, mcpURL, gpt, chatgpt, "tools/call", map[string]any{"name": name, "arguments": args, "_meta": map[string]any{"openai/session": conversation}})
	}
	gptTool := func(name string, args map[string]any) map[string]any {
		t.Helper()
		res, msg := gptCall("conv-1", name, args)
		return structured(t, res, msg)
	}
	sub := func(args map[string]any) string {
		_, msg := gameRPC(t, mcpURL, gpt, chatgpt, "events/subscribe", map[string]any{"name": "game.update", "arguments": args,
			"delivery": map[string]any{"mode": "webhook", "url": cb.url(), "secret": cb.secret()}, "ttlMs": 3600000,
			"_meta": map[string]any{"io.modelcontextprotocol/clientInfo": map[string]any{"name": "openai-mcp", "version": "1.0.0"}}})
		return msg
	}
	unsub := func(args map[string]any) string {
		_, msg := gameRPC(t, mcpURL, gpt, chatgpt, "events/unsubscribe", map[string]any{"name": "game.update", "arguments": args,
			"delivery": map[string]any{"mode": "webhook", "url": cb.url()},
			"_meta":    map[string]any{"io.modelcontextprotocol/clientInfo": map[string]any{"name": "openai-mcp", "version": "1.0.0"}}})
		return msg
	}
	if msg := sub(map[string]any{}); msg == "" {
		t.Fatal("subscribed with no seat")
	}

	// ChatGPT joins: no session of its own, and the result tells it to
	// subscribe to game.update.
	res, msg := gptCall("conv-1", "join", map[string]any{})
	joined := structured(t, res, msg)
	game, _ := joined["game"].(string)
	if game == "" || joined["session_key"] != nil || !strings.Contains(resultText(res), "game.update") {
		t.Fatalf("join for ChatGPT: %v %q", joined, resultText(res))
	}
	// It subscribes, with no arguments.
	if msg := sub(map[string]any{}); msg != "" {
		t.Fatalf("subscribing with {}: %s", msg)
	}

	// Two Claude Code players: join subscribes their conversations; they
	// aren't offered subscribe_events.
	var ps []*gametest.Agent
	var nexts []func() gametest.Update
	for range 2 {
		p := gametest.Connect(t, mcpURL, c.AppSignIn(srv.URL, "Claude Code"))
		ps, nexts = append(ps, p), append(nexts, p.Updates())
	}
	if text, failed := ps[0].RawText("join", map[string]any{"harness": "claude-code"}); failed || strings.Contains(text, "subscribe to game.update") {
		t.Fatalf("a Claude Code join: %q", text)
	}
	tools, err := ps[0].S.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "subscribe_events" {
			t.Fatal("subscribe_events offered: join subscribes")
		}
	}
	ps[1].Must("join", nil) // the third player: the game starts
	if u := nexts[0](); u.Subject != "The game has started" || u.From != "" || u.Game != game {
		t.Fatalf("Claude Code woken with %+v", u)
	}
	waitFor(t, "ChatGPT's game.update", func() bool { return len(cb.got()) >= 1 })
	e := cb.got()[0]
	if e.Body.Name != "game.update" || e.Body.Data["game"] != game || e.Body.Data["subject"] != "The game has started" || e.Body.Data["from"] != "" {
		t.Fatalf("delivered: %+v", e.Body)
	}
	if line := e.Body.Meta["ai.wakeshift/text"]; !strings.Contains(line, "at table "+game+" has started") || !strings.Contains(line, "trust=") {
		t.Fatalf("rendered line: %q", line)
	}

	// A Claude Code player talks, with the old argument name for its
	// session: ChatGPT gets it, from that player, labeled. What ChatGPT
	// says (its seat found by its conversation) reaches the Claude Code
	// players, labeled, and doesn't come back to it.
	g := ps[0].Must("game", map[string]any{"session_key": ps[0].Session()})
	speaker, _ := g["you"].(string)
	ps[0].Must("say", map[string]any{"text": "I have a feeling about this one.", "session_key": ps[0].Session()})
	waitFor(t, "the player's words", func() bool { return len(cb.got()) >= 2 })
	if e := cb.got()[1]; e.Body.Data["from"] != speaker || e.Body.Data["text"] != "I have a feeling about this one." ||
		!strings.Contains(e.Body.Meta["ai.wakeshift/text"], speaker+" (a player) said: I have a feeling about this one.") {
		t.Fatalf("delivered: %+v", e.Body)
	}
	gptTool("say", map[string]any{"text": "Interesting."})
	gptHandle, _ := joined["you"].(string)
	for {
		u := nexts[0]()
		if u.Text == "Interesting." {
			if u.From != gptHandle || u.Line != gptHandle+" (a player) said: Interesting." {
				t.Fatalf("Claude Code heard ChatGPT as %+v", u)
			}
			break
		}
	}
	time.Sleep(300 * time.Millisecond)
	ids := map[string]int{}
	for _, e := range cb.got() {
		ids[e.Body.Data["id"]]++
		if e.Body.Data["text"] == "Interesting." {
			t.Fatal("ChatGPT got its own words back")
		}
		// Only what was said: no one's role or codeword.
		for k := range e.Body.Data {
			switch k {
			case "id", "game", "from", "subject", "text", "mentions", "mentioned", "at":
			default:
				t.Fatalf("game.update carries %q: %s", k, e.Raw)
			}
		}
	}
	for id, n := range ids {
		if n != 1 {
			t.Fatalf("ChatGPT heard update %s %d times", id, n)
		}
	}

	// Another ChatGPT conversation of the same agent is another player: it
	// isn't seated in the first one's game, and joining seats it elsewhere.
	if _, msg := gptCall("conv-2", "game", map[string]any{}); msg != "" {
		t.Fatalf("game from another conversation: %s", msg)
	} else if res, _ := gptCall("conv-2", "game", map[string]any{}); res["isError"] != true || !strings.Contains(resultText(res), "not in a game") {
		t.Fatalf("another conversation found the first's seat: %v", res)
	}
	res, msg = gptCall("conv-2", "join", map[string]any{})
	if other := structured(t, res, msg); other["game"] == game {
		t.Fatalf("both conversations seated in game %s", game)
	}

	// The games have no watch tool: join subscribes.
	for _, tool := range tools.Tools {
		if tool.Name == "watch" {
			t.Fatal("watch offered")
		}
	}

	// While subscribed, ChatGPT is a player the game waits for: the other
	// two voting doesn't end it. Once it unsubscribes it's away, like a
	// dropped stream, and the next vote ends the game.
	ps[0].Must("vote", map[string]any{"player": gptHandle, "session_key": ps[0].Session()})
	if out := ps[1].Must("vote", map[string]any{"player": gptHandle}); out["phase"] == "next game" {
		t.Fatalf("the game ended without the subscribed ChatGPT player: %v", out)
	}
	if msg := unsub(map[string]any{}); msg != "" {
		t.Fatalf("events/unsubscribe: %s", msg)
	}
	if out := ps[1].Must("vote", map[string]any{"player": gptHandle}); out["phase"] != "next game" {
		t.Fatalf("the game waited for an unsubscribed player: %v", out)
	}
}
