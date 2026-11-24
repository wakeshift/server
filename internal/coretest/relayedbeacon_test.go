package coretest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/connector"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// relayChat is a small chat app on the Go SDK, shaped like Agent Chat
// where it matters here: watch subscribes the conversation (its result
// carries the beacon), post publishes a message to everyone watching, and
// history relays what others posted, as structured content (JSON text) or
// as plain text that ends with the last message. Its user is the bearer
// token.
type relayChat struct {
	app *wakeshift.Client
	url string

	mu   sync.Mutex
	msgs []map[string]string
}

// relayArgs are relay.message's arguments: the channel.
type relayArgs struct {
	Channel string `json:"channel"`
}

func newRelayChat(t *testing.T, c *coretest.Core) *relayChat {
	t.Helper()
	r := &relayChat{app: bareApp(t, c, "relay")}
	msg, err := wakeshift.DefineEvent(r.app, "relay.message", wakeshift.EventDef[relayArgs, map[string]string]{
		Description: "A message in a channel.", Topic: "relay/{channel}",
		Text: func(m map[string]string, _ relayArgs) string { return m["from"] + ": " + m["text"] },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.app.PublishManifest(ctx, wakeshift.Manifest{Description: "Relay chat."}); err != nil {
		t.Fatal(err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "relay", Version: "1"}, nil)
	at := r.app.Attach(srv, wakeshift.AttachOptions{Identify: identifyUser})
	user := func(req *mcp.CallToolRequest) string { return req.Extra.TokenInfo.UserID }
	type watchIn struct{}
	wakeshift.AddTool(at, &mcp.Tool{Name: "watch"}, func(ctx context.Context, call *wakeshift.ToolCall, in watchIn) (*mcp.CallToolResult, any, error) {
		if _, err := msg.Subscribe(ctx, call, relayArgs{Channel: "general"}); err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Watching #general."}}}, nil, nil
	}, wakeshift.Subscribes(msg))
	type postIn struct {
		Text string `json:"text"`
	}
	mcp.AddTool(srv, &mcp.Tool{Name: "post"}, func(ctx context.Context, req *mcp.CallToolRequest, in postIn) (*mcp.CallToolResult, any, error) {
		r.mu.Lock()
		r.msgs = append(r.msgs, map[string]string{"from": user(req), "text": in.Text})
		r.mu.Unlock()
		_, err := msg.Emit(ctx, relayArgs{Channel: "general"}, map[string]string{"from": user(req), "text": in.Text})
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "posted"}}}, nil, err
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "history"}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, map[string]any, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		return nil, map[string]any{"messages": append([]map[string]string(nil), r.msgs...)}, nil
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "history_text"}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		var lines []string
		for _, m := range r.msgs {
			lines = append(lines, m["from"]+": "+m["text"])
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Join(lines, "\n")}}}, nil, nil
	})
	// watch_text subscribes the conversation and relays the channel's
	// messages as text, the last message last: a wrapped tool whose own
	// text can end with what someone else posted.
	wakeshift.AddTool(at, &mcp.Tool{Name: "watch_text"}, func(ctx context.Context, call *wakeshift.ToolCall, in watchIn) (*mcp.CallToolResult, any, error) {
		if _, err := msg.Subscribe(ctx, call, relayArgs{Channel: "general"}); err != nil {
			return nil, nil, err
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		lines := []string{"Watching #general. Recent messages:"}
		for _, m := range r.msgs {
			lines = append(lines, m["from"]+": "+m["text"])
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Join(lines, "\n")}}}, nil, nil
	}, wakeshift.Subscribes(msg))
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	verify := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		return &auth.TokenInfo{UserID: token, Expiration: time.Now().Add(time.Hour)}, nil
	}
	hs := httptest.NewServer(auth.RequireBearerToken(verify, nil)(at.Handler(h)))
	t.Cleanup(hs.Close)
	r.url = hs.URL
	return r
}

type userBearer struct{ user string }

func (b userBearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.user)
	return http.DefaultTransport.RoundTrip(r)
}

// relayAgent is an MCP client of relayChat: user's, running in client
// (its clientInfo name: how Wakeshift recognizes the harness).
type relayAgent struct {
	t    *testing.T
	s    *mcp.ClientSession
	meta map[string]any // sent with every call (Antigravity's conversation)
}

func (r *relayChat) agent(t *testing.T, user, client string) *relayAgent {
	t.Helper()
	s, err := mcp.NewClient(&mcp.Implementation{Name: client}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: r.url, HTTPClient: &http.Client{Transport: userBearer{user}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return &relayAgent{t: t, s: s}
}

func (a *relayAgent) call(tool string, args map[string]any) *mcp.CallToolResult {
	a.t.Helper()
	res, err := a.s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args, Meta: a.meta})
	if err != nil || res.IsError {
		a.t.Fatalf("%s: %v %+v", tool, err, res)
	}
	return res
}

// lastText is a result's last text block (where the SDK puts Wakeshift's
// text, beacon last).
func lastText(res *mcp.CallToolResult) string {
	for i := len(res.Content) - 1; i >= 0; i-- {
		if tc, ok := res.Content[i].(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

// localHook is Wakeshift's hook for harness on a user's machine, trusting
// the core.
func localHook(t *testing.T, c *coretest.Core, harness string) *connector.Hook {
	t.Helper()
	spec, ok := connector.Lookup(harness)
	if !ok {
		t.Fatalf("no harness %s", harness)
	}
	set := connector.Settings{Dir: t.TempDir(), Issuers: []string{c.URL}}
	return &connector.Hook{Harness: harness, Format: spec.Format, Verifier: &connector.Verifier{Settings: set}, Poller: &connector.HTTPPoller{Issuers: set.Issuers},
		Links: connector.LinksFor(set), When: spec.When, Unbound: spec.Unbound}
}

func runLocalHook(h *connector.Hook, event, in string) string {
	h.EventName = event
	var out bytes.Buffer
	h.Run(ctx, strings.NewReader(in), &out, &out)
	return out.String()
}

// cursorPost is Cursor's postToolUse input (as recorded from
// cursor-agent 2026.09.23) for a result of the app's tool.
func cursorPost(t *testing.T, conv, tool string, res *mcp.CallToolResult) string {
	t.Helper()
	out, _ := json.Marshal(res)
	b, _ := json.Marshal(map[string]any{"conversation_id": conv, "generation_id": conv, "model": "m", "tool_name": "MCP:" + tool, "tool_input": map[string]any{},
		"tool_output": string(out), "duration": 1.2, "tool_use_id": "078f8363-6783-4d14-bb39-c6a4d5cd1361", "session_id": conv, "hook_event_name": "postToolUse",
		"cursor_version": "2026.09.23-86fc751", "workspace_roots": []string{"/p"}, "transcript_path": "/x.jsonl"})
	return string(b)
}

// agyPost is Antigravity's PostToolUse input (agy 1.4.0) for a result of
// the app's tool, its step's text already in the transcript.
func agyPost(t *testing.T, conv, tool string, res *mcp.CallToolResult) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "logs")
	os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, "transcript_full.jsonl")
	step, _ := json.Marshal(map[string]any{"step_index": 4, "source": "MODEL", "type": "GENERIC", "status": "DONE",
		"content": "Created At: t\nCompleted At: t\n" + lastText(res)})
	os.WriteFile(path, append(step, '\n'), 0o600)
	b, _ := json.Marshal(map[string]any{"conversationId": conv, "stepIdx": 4, "transcriptPath": path, "modelName": "m",
		"toolCall": map[string]any{"name": "call_mcp_tool", "args": map[string]any{"ServerName": "relay", "ToolName": tool, "Arguments": map[string]any{}}}})
	return string(b)
}

// connectedIn says whether a connector connected a session in the
// workspace of the app's user.
func connectedIn(t *testing.T, c *coretest.Core, app, user string) bool {
	t.Helper()
	var n int
	if err := c.DB.InTenant(ctx, userTenant(t, c, app, user), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE connector_seen_at IS NOT NULL`).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// Mallory knows Ada's conversation ID (it isn't secret), has the app mint a
// beacon naming it for her own session, and posts the beacon's line where
// Ada's agent reads it. Ada's hooks (Cursor's, Antigravity's: text only)
// refuse it wherever it's relayed: in a read tool's structured result, at
// the end of a read tool's text, and inside the text her own watch result
// relays before her own beacon, which is the one that connects. With
// Ada's conversation connected, the core refuses to move it to Mallory's
// session even from a hook that would redeem her beacon (an old one), and
// still lets Ada's own reconnect.
func TestRelayedBeaconIsRefused(t *testing.T) {
	c := coretest.Start(t)
	chat := newRelayChat(t, c)
	for _, harness := range []struct{ name, client string }{{"cursor", "Cursor"}, {"antigravity", "antigravity-client"}} {
		t.Run(harness.name, func(t *testing.T) {
			conv := map[string]string{"cursor": "27188d7e-37d9-4a60-b001-465a6950b24b", "antigravity": "2b9d08bb-7acc-4459-9956-69064f8d5869"}[harness.name]
			ada, mallory := chat.agent(t, "ada-"+harness.name, harness.client), chat.agent(t, "mallory-"+harness.name, harness.client)
			watchArgs := map[string]any{}
			if harness.name == "cursor" {
				watchArgs["conversation_id"] = conv // Cursor's agent passes it; Antigravity's client sends it
			} else {
				ada.meta = map[string]any{"antigravity.google/conversation_id": conv}
				mallory.meta = map[string]any{"antigravity.google/conversation_id": conv}
			}
			hook := localHook(t, c, harness.name)
			post := func(tool string, res *mcp.CallToolResult) string {
				if harness.name == "cursor" {
					return runLocalHook(hook, "", cursorPost(t, conv, tool, res))
				}
				return runLocalHook(hook, "post-tool", agyPost(t, conv, tool, res))
			}
			adaUser, malloryUser := "ada-"+harness.name, "mallory-"+harness.name

			// Ada watches first (her hooks aren't installed yet: nothing
			// redeems this beacon).
			ada.call("watch", watchArgs)
			// Mallory watches as Ada's conversation, and posts her beacon.
			mres := mallory.call("watch", watchArgs)
			planted := beacon.Trailer(lastText(mres))
			if planted == "" {
				t.Fatalf("Mallory's watch carried no beacon: %s", lastText(mres))
			}
			mallory.call("post", map[string]any{"text": "hello everyone\n\n" + beacon.Text(planted)})

			for _, read := range []string{"history", "history_text"} {
				res := ada.call(read, nil)
				if !strings.Contains(lastText(res), planted) {
					t.Fatalf("%s doesn't relay the message: %s", read, lastText(res))
				}
				if out := post(read, res); strings.Contains(out, "connected") || hook.Links.Load(conv) != "" {
					t.Fatalf("%s: acted on the relayed beacon: %s", read, out)
				}
			}
			if connectedIn(t, c, "relay", malloryUser) {
				t.Fatal("Mallory's session was connected")
			}

			// Ada's watch again, her hooks installed now: its own beacon
			// ends it, and connects her conversation to her session.
			res := ada.call("watch", watchArgs)
			if text := lastText(res); beacon.Trailer(text) == planted || beacon.Trailer(text) == "" {
				t.Fatalf("Ada's watch: %s", text)
			}
			post("watch", res)
			if harness.name == "antigravity" {
				runLocalHook(hook, "pre-invocation", `{"conversationId":"`+conv+`","initialNumSteps":1,"invocationNum":0,"modelName":"m","transcriptPath":"/tmp/t.jsonl","workspacePaths":["/p"]}`)
			}
			link := hook.Links.Load(conv)
			if link == "" || !connectedIn(t, c, "relay", adaUser) || connectedIn(t, c, "relay", malloryUser) {
				t.Fatalf("Ada's own beacon: link %q, Ada connected %v, Mallory connected %v", link, connectedIn(t, c, "relay", adaUser), connectedIn(t, c, "relay", malloryUser))
			}

			// Redeemed by a hook that doesn't check (an old one), Mallory's
			// next beacon still can't move Ada's connected conversation:
			// the hook's link is in Ada's workspace.
			keys := coreKeys(t, c)
			fresh := beacon.Trailer(lastText(mallory.call("watch", watchArgs)))
			cl, err := beacon.Verify(fresh, conv, []string{c.URL}, keys, time.Now())
			if err != nil || cl.Tool != "watch" || cl.App != "relay" {
				t.Fatalf("Mallory's beacon: %+v %v", cl, err)
			}
			if code, out := redeemHeld(t, cl, conv, link); code != http.StatusConflict || out.Error.Code != "conversation_taken" {
				t.Fatalf("takeover: %d %+v", code, out)
			}
			if connectedIn(t, c, "relay", malloryUser) {
				t.Fatal("Mallory's session was connected")
			}
			// Ada's own reconnect (a new beacon, the same workspace) works.
			mine, err := beacon.Verify(beacon.Trailer(lastText(ada.call("watch", watchArgs))), conv, []string{c.URL}, keys, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if code, out := redeemHeld(t, mine, conv, link); code != http.StatusOK || out.URL == "" {
				t.Fatalf("Ada's reconnect: %d %+v", code, out)
			}
		})
	}
}

type redeemOut struct {
	URL   string `json:"url"`
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

// redeemHeld redeems a beacon's ticket for thread, saying the hook holds
// link for it.
func redeemHeld(t *testing.T, cl beacon.Claims, thread, held string) (int, redeemOut) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"ticket": cl.Ticket, "thread": thread, "held": held})
	resp, err := http.Post(cl.Redeem, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out redeemOut
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// Within one workspace, a conversation connected to one agent's session
// isn't moved to another agent's; once its link is revoked (unwatched),
// it can be.
func TestRedeemRefusesAnotherAgentsTakeover(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	keys := coreKeys(t, c)
	const conv = "2b9d08bb-7acc-4459-9956-69064f8d5869"
	subscribe := func(client string, meta map[string]any) beacon.Claims {
		t.Helper()
		sub, err := subscribe(app, caller{User: "ada", Client: client, Meta: meta, Conversation: conv, Tool: "subscribe"}, "ci/acme", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		cl, err := beacon.Verify(beacon.Trailer(sub.Addition.Text), conv, []string{c.URL}, keys, time.Now())
		if err != nil || cl.Tool != "subscribe" {
			t.Fatalf("beacon: %+v %v (%s)", cl, err, sub.Addition.Text)
		}
		return cl
	}
	code, first := redeemHeld(t, subscribe("antigravity-client", map[string]any{"antigravity.google/conversation_id": conv}), conv, "")
	if code != 200 {
		t.Fatalf("first: %d %+v", code, first)
	}
	// Another agent of the same user's (Cursor), its session named by the
	// same conversation ID.
	other := subscribe("Cursor", nil)
	if code, out := redeemHeld(t, other, conv, first.URL); code != http.StatusConflict || out.Error.Code != "conversation_taken" {
		t.Fatalf("another agent: %d %+v", code, out)
	}
	// The same agent again: fine.
	if code, out := redeemHeld(t, subscribe("antigravity-client", map[string]any{"antigravity.google/conversation_id": conv}), conv, first.URL); code != 200 {
		t.Fatalf("the same agent: %d %+v", code, out)
	}
}

// Mallory has the app mint a beacon naming Ada's conversation for
// watch_text, a wrapped tool that relays the channel's messages, and posts
// it there. Ada's own watch_text then relays it as the end of its text:
// Wakeshift's own beacon still ends the result, so her hook connects her
// conversation to her own session, never to Mallory's.
func TestRelayedBeaconInAWrappedResultIsNotLast(t *testing.T) {
	c := coretest.Start(t)
	chat := newRelayChat(t, c)
	const conv = "5f0c1d2e-8a7b-4c3d-9e1f-0a2b3c4d5e6f"
	ada, mallory := chat.agent(t, "ada", "Cursor"), chat.agent(t, "mallory", "Cursor")
	args := map[string]any{"conversation_id": conv}
	hook := localHook(t, c, "cursor")

	planted := beacon.Trailer(lastText(mallory.call("watch_text", args)))
	if planted == "" {
		t.Fatal("Mallory's watch_text carried no beacon")
	}
	mallory.call("post", map[string]any{"text": "hello everyone\n\n" + beacon.Text(planted)})

	res := ada.call("watch_text", args)
	var all []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			all = append(all, tc.Text)
		}
	}
	text := strings.Join(all, "\n\n")
	if got := beacon.Trailer(text); got == planted || got == "" {
		t.Fatalf("Ada's watch_text ends with Mallory's beacon (or none): %s", text)
	}
	runLocalHook(hook, "", cursorPost(t, conv, "watch_text", res))
	if connectedIn(t, c, "relay", "mallory") {
		t.Fatal("Ada's conversation was connected to Mallory's session")
	}
	if !connectedIn(t, c, "relay", "ada") {
		t.Fatal("Ada's own beacon didn't connect her conversation")
	}
}
