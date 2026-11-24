package coretest_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// The Go SDK's wrapper (wakeshift.Events) end to end, against a real
// core: a wrapped tool subscribes the calling conversation, its reply
// gets one block, a failed call is rolled back, native callers are told
// what to subscribe to, and the app reaches the conversations it stored
// with a multi-recipient Emit.

type wrappedIn struct {
	Table string `json:"table"`
}

type wrappedApp struct {
	app  *wakeshift.Client
	turn *wakeshift.Event[tableArgs, turn]
	url  string

	mu   sync.Mutex
	subs map[string][]wakeshift.Subscription // by bearer token
}

// newWrappedApp serves an app with a personal game.turn whose tools are
// wrapped. A bearer token "u:<user>" is the app's user; "a:<workspace>.<agent>"
// is an agent the app acts for.
func newWrappedApp(t *testing.T, c *coretest.Core, id string) *wrappedApp {
	t.Helper()
	w := &wrappedApp{app: bareApp(t, c, id), subs: map[string][]wakeshift.Subscription{}}
	var err error
	if w.turn, err = wakeshift.DefineEvent(w.app, "game.turn", wakeshift.EventDef[tableArgs, turn]{
		Description: "It's your move.", Personal: true,
		Text: func(tu turn, a tableArgs) string { return "Your move at " + a.Table + ": " + tu.ToCall + " to call" },
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.app.PublishManifest(ctx, wakeshift.Manifest{Description: "Cards.", Agent: &wakeshift.AgentScope{TTLDays: 30}}); err != nil {
		t.Fatal(err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: id, Version: "1"}, nil)
	at := w.app.Attach(srv, wakeshift.AttachOptions{Identify: func(ctx context.Context, r wakeshift.Request) (wakeshift.Identity, error) {
		ti := r.Token
		if ti == nil {
			return wakeshift.Identity{}, nil
		}
		if ws, ok := ti.Extra["workspace"].(string); ok {
			ag, err := w.app.ActAs(ctx, ws, ti.Extra["agent"].(string))
			return wakeshift.Identity{User: ti.UserID, Agent: ag}, err
		}
		return wakeshift.Identity{User: ti.UserID}, nil
	}})
	// join subscribes to its table and to "lobby": two subscribes, one block.
	wakeshift.AddTool(at, &mcp.Tool{Name: "join"}, func(ctx context.Context, call *wakeshift.ToolCall, in wrappedIn) (*mcp.CallToolResult, any, error) {
		var got []wakeshift.Subscription
		for _, table := range []string{in.Table, "lobby"} {
			sub, err := w.turn.Subscribe(ctx, call, tableArgs{Table: table}, wakeshift.Reason("Playing at "+table))
			if err != nil {
				return nil, nil, err
			}
			got = append(got, sub)
		}
		w.mu.Lock()
		w.subs[tokenOf(call)] = got
		w.mu.Unlock()
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Seated at " + in.Table + "."}}}, nil, nil
	}, wakeshift.Subscribes(w.turn))
	// leave ends the conversation's subscription to its table.
	wakeshift.AddTool(at, &mcp.Tool{Name: "leave"}, func(ctx context.Context, call *wakeshift.ToolCall, in wrappedIn) (*mcp.CallToolResult, any, error) {
		ended, err := w.turn.Unsubscribe(ctx, call, tableArgs{Table: in.Table})
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("ended: %v", ended)}}}, nil, nil
	}, wakeshift.Subscribes(w.turn))
	// fails subscribes, then fails: the subscription is rolled back.
	wakeshift.AddTool(at, &mcp.Tool{Name: "fails"}, func(ctx context.Context, call *wakeshift.ToolCall, in wrappedIn) (*mcp.CallToolResult, any, error) {
		if _, err := w.turn.Subscribe(ctx, call, tableArgs{Table: in.Table}); err != nil {
			return nil, nil, err
		}
		return nil, nil, errors.New("that table is full")
	}, wakeshift.Subscribes(w.turn))
	verify := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		ti := &auth.TokenInfo{Expiration: time.Now().Add(time.Hour), Extra: map[string]any{"token": token}}
		switch kind, rest, _ := strings.Cut(token, ":"); kind {
		case "u":
			ti.UserID = rest
		case "a":
			ws, ag, _ := strings.Cut(rest, ".")
			ti.UserID, ti.Extra["workspace"], ti.Extra["agent"] = ws+"/"+ag, ws, ag
		default:
			return nil, auth.ErrInvalidToken
		}
		return ti, nil
	}
	w.url = serveAttached(t, srv, at, verify)
	return w
}

func tokenOf(call *wakeshift.ToolCall) string {
	if call.Request.Extra == nil || call.Request.Extra.TokenInfo == nil {
		return ""
	}
	tok, _ := call.Request.Extra.TokenInfo.Extra["token"].(string)
	return tok
}

func (w *wrappedApp) subsOf(token string) []wakeshift.Subscription {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.subs[token]
}

type wrappedResult struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	Meta    map[string]any `json:"_meta"`
	IsError bool           `json:"isError"`
}

func (r wrappedResult) text() string {
	var parts []string
	for _, c := range r.Content {
		parts = append(parts, c.Text)
	}
	return strings.Join(parts, "\n\n")
}

// call calls a tool with token, as the client ua names, with _meta.
func (w *wrappedApp) call(t *testing.T, token, ua, tool string, args, meta map[string]any) wrappedResult {
	t.Helper()
	params := map[string]any{"name": tool, "arguments": args}
	if meta != nil {
		params["_meta"] = meta
	}
	raw, status := postRPC(t, w.url, bearer(token, ua), 1, "tools/call", params)
	var out struct {
		Result *wrappedResult `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Result == nil {
		t.Fatalf("%s: %d %s", tool, status, raw)
	}
	return *out.Result
}

// codexMeta is a Codex thread's _meta.
func codexMeta(thread string) map[string]any {
	return map[string]any{"io.modelcontextprotocol/clientInfo": map[string]any{"name": "codex-mcp-client", "version": "0.50.0"},
		"x-codex-turn-metadata": map[string]any{"thread_id": thread}}
}

// A wrapped tool that subscribes twice gets one block: the full
// instructions with one beacon the first time, the short form the next.
// Both subscriptions are the conversation's, with the reason recorded.
func TestWrappedToolOneBlock(t *testing.T) {
	c := coretest.Start(t)
	w := newWrappedApp(t, c, "cards")
	res := w.call(t, "u:ada", "codex_exec/0.50.0", "join", map[string]any{"table": "oak"}, codexMeta("thread-1"))
	if res.IsError || !strings.HasPrefix(res.text(), "Seated at oak.\n\n") {
		t.Fatalf("join: %+v", res)
	}
	tenant := userTenant(t, c, "cards", "ada")
	if res.Meta[beacon.MetaKey] == nil || tickets(t, c, tenant) != 1 {
		t.Fatalf("one beacon: meta %v, %d tickets", res.Meta, tickets(t, c, tenant))
	}
	if strings.Count(res.text(), "wakeshift-beacon") > 1 {
		t.Fatalf("two beacons in the text: %s", res.text())
	}
	subs := w.subsOf("u:ada")
	if len(subs) != 2 || subs[0].State != wakeshift.SubscriptionCreated || subs[1].State != wakeshift.SubscriptionCreated || subs[0].Session == "" || subs[0].Session != subs[1].Session ||
		subs[0].User != "ada" || subs[0].Recognized.Harness != "codex" {
		t.Fatalf("subscriptions: %+v", subs)
	}
	list, err := w.app.EventSubscriptions(ctx, "ada")
	if err != nil || len(list) != 2 {
		t.Fatalf("listed: %+v %v", list, err)
	}
	first := res.text()
	// The next call in the thread: renewals, and the short form.
	res = w.call(t, "u:ada", "codex_exec/0.50.0", "join", map[string]any{"table": "oak"}, codexMeta("thread-1"))
	if res.IsError || res.text() == first || len(res.text()) >= len(first) {
		t.Fatalf("the next block isn't shorter:\n%s\n---\n%s", first, res.text())
	}
	if again := w.subsOf("u:ada"); again[0].State != wakeshift.SubscriptionRenewed || again[0].ID != subs[0].ID {
		t.Fatalf("renewal: %+v", again)
	}
}

// A conversation that joins again renews its subscription; leave ends it
// (Unsubscribe), so an emit no longer reaches it; End ends one by its ID,
// only for its own user. A personal event needs a user.
func TestWrappedToolRenewLeaveEnd(t *testing.T) {
	c := coretest.Start(t)
	w := newWrappedApp(t, c, "cards")
	const ua = "claude-code/2.1.280"
	if res := w.call(t, "u:ada", ua, "join", map[string]any{"table": "oak"}, nil); res.IsError {
		t.Fatalf("join: %s", res.text())
	}
	first := w.subsOf("u:ada")
	if len(first) != 2 || first[0].State != wakeshift.SubscriptionCreated || first[0].Session == "" {
		t.Fatalf("join: %+v", first)
	}
	if res := w.call(t, "u:ada", ua, "join", map[string]any{"table": "oak", "session_key": string(first[0].Session)}, nil); res.IsError {
		t.Fatalf("join again: %s", res.text())
	}
	if again := w.subsOf("u:ada"); again[0].State != wakeshift.SubscriptionRenewed || again[0].ID != first[0].ID {
		t.Fatalf("join again: %+v", again)
	}
	tenant := userTenant(t, c, "cards", "ada")
	if got, err := w.turn.Emit(ctx, tableArgs{Table: "oak"}, turn{ToCall: "2"}, wakeshift.To("ada")); err != nil || got.Matched != 1 {
		t.Fatalf("emit: %+v %v", got, err)
	}
	waitFor(t, "the turn delivered", func() bool { return deliveryCount(t, c, tenant) == 1 })

	if res := w.call(t, "u:ada", ua, "leave", map[string]any{"table": "oak", "session_key": string(first[0].Session)}, nil); res.IsError || !strings.HasPrefix(res.text(), "ended: true") {
		t.Fatalf("leave: %s", res.text())
	}
	if got, err := w.turn.Emit(ctx, tableArgs{Table: "oak"}, turn{ToCall: "4"}, wakeshift.To("ada")); err != nil || got.Matched != 0 {
		t.Fatalf("an emit after leave reached %+v %v", got, err)
	}

	// End, by ID: only the subscription's own user can.
	lobby := first[1]
	if ended, err := endSub(w.app, "bo", lobby.ID); err != nil || ended {
		t.Fatalf("bo ended ada's subscription: %v %v", ended, err)
	}
	if ended, err := endSub(w.app, "ada", lobby.ID); err != nil || !ended {
		t.Fatalf("End: %v %v", ended, err)
	}

	// A personal event belongs to someone.
	if res := w.call(t, "u:", ua, "join", map[string]any{"table": "elm"}, nil); !res.IsError {
		t.Fatalf("joined a personal event's table with no user: %s", res.text())
	}
}

// A wrapped tool that fails after subscribing ends the subscription it
// made (rolled_back, audited), and its reply carries no block.
func TestWrappedToolRollback(t *testing.T) {
	c := coretest.Start(t)
	w := newWrappedApp(t, c, "cards")
	res := w.call(t, "u:bo", "claude-code/2.1.280", "fails", map[string]any{"table": "oak"}, nil)
	if !res.IsError || strings.Contains(res.text(), "Wakeshift") || !strings.Contains(res.text(), "that table is full") {
		t.Fatalf("fails: %+v", res)
	}
	tenant := userTenant(t, c, "cards", "bo")
	if list, err := w.app.EventSubscriptions(ctx, "bo"); err != nil || len(list) != 0 {
		t.Fatalf("still subscribed: %+v %v", list, err)
	}
	if n := count(t, c, tenant, `SELECT count(*) FROM event_subscriptions WHERE ended_reason = 'rolled_back'`); n != 1 {
		t.Fatalf("rolled back: %d", n)
	}
	if !contains(auditActions(t, c, tenant), "event_subscription.rolled_back") {
		t.Fatalf("audit: %v", auditActions(t, c, tenant))
	}
}

// A native caller (ChatGPT) isn't subscribed: the block tells it to
// subscribe to each event itself, once per event and arguments.
func TestWrappedToolNative(t *testing.T) {
	c := coretest.Start(t)
	w := newWrappedApp(t, c, "cards")
	res := w.call(t, "u:cy", "openai-mcp/1.0.0", "join", map[string]any{"table": "oak"}, map[string]any{"openai/session": "chat-1"})
	if res.IsError {
		t.Fatalf("join: %+v", res)
	}
	for _, table := range []string{"oak", "lobby"} {
		if want := `To be woken by game.turn with {"table":"` + table + `"}, subscribe to it (MCP Events).`; !strings.Contains(res.text(), want) {
			t.Fatalf("no line for %s: %s", table, res.text())
		}
	}
	if subs := w.subsOf("u:cy"); len(subs) != 2 || subs[0].State != wakeshift.SubscriptionNative || subs[0].ID != "" {
		t.Fatalf("subscriptions: %+v", subs)
	}
	if list, err := w.app.EventSubscriptions(ctx, "cy"); err != nil || len(list) != 0 {
		t.Fatalf("a native caller was subscribed: %+v %v", list, err)
	}
}

// The app stores what Subscribe returned and reaches those
// conversations with one multi-recipient Emit: by subscription (for an app
// user and for an agent the app acts for), by user, and natively; a
// subscription that ended comes back in Ended, a user with nothing in
// Unmatched.
func TestWrappedToolMultiRecipientEmit(t *testing.T) {
	c := coretest.Start(t)
	w := newWrappedApp(t, c, "cards")
	ws, err := w.app.Workspace(ctx, "dee", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.app.AddAgent(ctx, ws, "codex", ""); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{"u:ada", "u:bo", "a:" + ws + ".codex"} {
		if res := w.call(t, tok, "claude-code/2.1.280", "join", map[string]any{"table": "oak"}, nil); res.IsError {
			t.Fatalf("join as %s: %s", tok, res.text())
		}
	}
	ada, bo, agent := w.subsOf("u:ada")[0], w.subsOf("u:bo")[0], w.subsOf("a:" + ws + ".codex")[0]
	if agent.User != ws+"/codex" || ada.User != "ada" {
		t.Fatalf("users: %q %q", agent.User, ada.User)
	}
	got, err := w.turn.Emit(ctx, tableArgs{Table: "oak"}, turn{ToCall: "5", Hand: "AK"}, wakeshift.ToSubscription(ada.User, ada.ID),
		wakeshift.ToSubscription(agent.User, agent.ID), wakeshift.To("nobody"), wakeshift.ToNative("ada"), wakeshift.WithID("hand-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Matched != 2 || len(got.Ended) != 0 || len(got.Unmatched) != 2 ||
		got.Unmatched[0] != (wakeshift.Recipient{User: "nobody"}) || got.Unmatched[1] != (wakeshift.Recipient{User: "ada", Native: true}) {
		t.Fatalf("emitted: %+v", got)
	}
	adaTenant, boTenant := userTenant(t, c, "cards", "ada"), userTenant(t, c, "cards", "bo")
	waitFor(t, "ada's delivery", func() bool { return deliveryCount(t, c, adaTenant) == 1 })
	waitFor(t, "the agent's delivery", func() bool { return deliveryCount(t, c, ws) == 1 })
	if n := deliveryCount(t, c, boTenant); n != 0 {
		t.Fatalf("bo wasn't named, and got %d", n)
	}
	// bo's subscription ends; an emit naming it hears so.
	if ended, err := endSub(w.app, bo.User, bo.ID); err != nil || !ended {
		t.Fatalf("end: %v %v", ended, err)
	}
	got, err = w.turn.Emit(ctx, tableArgs{Table: "oak"}, turn{ToCall: "10"}, wakeshift.ToSubscription(bo.User, bo.ID), wakeshift.ToSubscription(ada.User, ada.ID), wakeshift.WithID("hand-2"))
	if err != nil || got.Matched != 1 || len(got.Ended) != 1 || got.Ended[0] != (wakeshift.Recipient{User: "bo", Subscription: bo.ID}) {
		t.Fatalf("emit to an ended subscription: %+v %v", got, err)
	}
	// A retry with the same ID is a duplicate, with the first's ended.
	got, err = w.turn.Emit(ctx, tableArgs{Table: "oak"}, turn{ToCall: "10"}, wakeshift.ToSubscription(bo.User, bo.ID), wakeshift.ToSubscription(ada.User, ada.ID), wakeshift.WithID("hand-2"))
	if err != nil || !got.Duplicate || len(got.Ended) != 1 {
		t.Fatalf("the retry: %+v %v", got, err)
	}
}
