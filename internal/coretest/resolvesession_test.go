package coretest_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// ResolveSession on a tool that subscribes to nothing, end to end against a
// real core, in the shape a price tracker's unwatch_product has: watch subscribes
// the conversation with one tool call; unwatch, a later call of another
// tool, ends it with Unsubscribe and, when nothing ended, asks
// ResolveSession whether Wakeshift knew the conversation at all. Whatever
// names the conversation, ResolveSession is the session watch's Subscribe
// used, or empty: never a session Wakeshift starts for the occasion, and
// never another conversation's.

type watchArgs struct {
	WatchID string `json:"watch_id"`
}

type watchIn struct {
	WatchID string `json:"watch_id"`
}

type price struct {
	Price string `json:"price"`
}

type watchApp struct {
	app   *wakeshift.Client
	price *wakeshift.Event[watchArgs, price]
	url   string

	mu sync.Mutex
	// What the last watch and unwatch saw, by bearer token.
	watched  map[string]wakeshift.Subscription
	resolved map[string]wakeshift.SessionKey
	ended    map[string]bool
}

// newWatchApp serves a price-tracker app: a tools-only product.price,
// watch (subscribes) and unwatch (registered through the attachment, but
// subscribes to nothing). A bearer token "u:<user>" is the app's user;
// "anon" is a caller identify names no user for, as in an app without
// accounts.
func newWatchApp(t *testing.T, c *coretest.Core, id string) *watchApp {
	t.Helper()
	w := &watchApp{app: bareApp(t, c, id), watched: map[string]wakeshift.Subscription{}, resolved: map[string]wakeshift.SessionKey{}, ended: map[string]bool{}}
	var err error
	if w.price, err = wakeshift.DefineEvent(w.app, "product.price", wakeshift.EventDef[watchArgs, price]{
		Description: "A watched product's price changed.",
		Text:        func(p price, a watchArgs) string { return a.WatchID + " is now " + p.Price },
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.app.PublishManifest(ctx, wakeshift.Manifest{Description: "Products.", Agent: &wakeshift.AgentScope{TTLDays: 30}}); err != nil {
		t.Fatal(err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: id, Version: "1"}, nil)
	at := w.app.Attach(srv, wakeshift.AttachOptions{Identify: func(_ context.Context, r wakeshift.Request) (wakeshift.Identity, error) {
		if r.Token == nil {
			return wakeshift.Identity{}, nil
		}
		return wakeshift.Identity{User: r.Token.UserID}, nil
	}})
	wakeshift.AddTool(at, &mcp.Tool{Name: "watch"}, func(ctx context.Context, call *wakeshift.ToolCall, in watchIn) (*mcp.CallToolResult, any, error) {
		sub, err := w.price.Subscribe(ctx, call, watchArgs{WatchID: in.WatchID})
		if err != nil {
			return nil, nil, err
		}
		w.mu.Lock()
		w.watched[tokenOf(call)] = sub
		w.mu.Unlock()
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Watching " + in.WatchID + "."}}}, nil, nil
	}, wakeshift.Subscribes(w.price))
	wakeshift.AddTool(at, &mcp.Tool{Name: "unwatch"}, func(ctx context.Context, call *wakeshift.ToolCall, in watchIn) (*mcp.CallToolResult, any, error) {
		ended, err := w.price.Unsubscribe(ctx, call, watchArgs{WatchID: in.WatchID})
		if err != nil {
			return nil, nil, err
		}
		s, err := call.ResolveSession(ctx)
		if err != nil {
			return nil, nil, err
		}
		w.mu.Lock()
		w.resolved[tokenOf(call)], w.ended[tokenOf(call)] = s, ended
		w.mu.Unlock()
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("ended: %v", ended)}}}, nil, nil
	})
	verify := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		ti := &auth.TokenInfo{Expiration: time.Now().Add(time.Hour), Extra: map[string]any{"token": token}}
		switch kind, rest, _ := strings.Cut(token, ":"); {
		case kind == "u" && rest != "":
			ti.UserID = rest
		case token == "anon":
		default:
			return nil, auth.ErrInvalidToken
		}
		return ti, nil
	}
	w.url = serveAttached(t, srv, at, verify)
	return w
}

func (w *watchApp) call(t *testing.T, token, ua, tool string, args, meta map[string]any) {
	t.Helper()
	params := map[string]any{"name": tool, "arguments": args}
	if meta != nil {
		params["_meta"] = meta
	}
	raw, status := postRPC(t, w.url, bearer(token, ua), 1, "tools/call", params)
	if status != http.StatusOK || strings.Contains(string(raw), `"isError":true`) || !strings.Contains(string(raw), `"result"`) {
		t.Fatalf("%s: %d %s", tool, status, raw)
	}
}

// watch subscribes the token's conversation to id, returning the
// subscription (created or renewed).
func (w *watchApp) watch(t *testing.T, token, ua, id string, args, meta map[string]any) wakeshift.Subscription {
	t.Helper()
	a := map[string]any{"watch_id": id}
	for k, v := range args {
		a[k] = v
	}
	w.call(t, token, ua, "watch", a, meta)
	w.mu.Lock()
	defer w.mu.Unlock()
	sub := w.watched[token]
	if sub.ID == "" || sub.Session == "" {
		t.Fatalf("watch %s: %+v", id, sub)
	}
	return sub
}

// unwatch calls unwatch for id, returning whether it ended a subscription
// and what ResolveSession gave.
func (w *watchApp) unwatch(t *testing.T, token, ua, id string, args, meta map[string]any) (bool, wakeshift.SessionKey) {
	t.Helper()
	a := map[string]any{"watch_id": id}
	for k, v := range args {
		a[k] = v
	}
	w.call(t, token, ua, "unwatch", a, meta)
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ended[token], w.resolved[token]
}

func appTenants(t *testing.T, c *coretest.Core, app string) int {
	t.Helper()
	var n int
	if err := c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE app_id = $1`, app).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

const codexUA = "codex-mcp-client/0.130.0"

// An app without accounts: Codex, no user, the conversation named only by its
// thread. The thread isn't a credential (every MCP server the user has
// sees it), so without the session key nothing finds the conversation:
// ResolveSession is empty, with no session or workspace started for it, the
// watch stays live, and the session key then ends it. A 0.11.0 SDK, which
// asks the core anyway, is answered with no session, and nothing is made.
func TestResolveSessionCodexThreadWithoutUser(t *testing.T) {
	c := coretest.Start(t)
	w := newWatchApp(t, c, "shop")
	sub := w.watch(t, "anon", codexUA, "w1", nil, codexMeta("thr-1"))
	before := appTenants(t, c, "shop")

	ended, resolved := w.unwatch(t, "anon", codexUA, "w1", nil, codexMeta("thr-1"))
	if ended || resolved != "" {
		t.Fatalf("by the thread alone: ended %v, resolved a session: %v", ended, resolved != "")
	}
	if !noteLive(t, c, sub.ID) {
		t.Fatal("the watch ended without the key")
	}
	if n := appTenants(t, c, "shop"); n != before {
		t.Fatalf("resolving by the thread alone made %d workspaces", n-before)
	}

	// The core, asked as a 0.11.0 SDK asks it: no session, nothing made.
	var out struct {
		Session *string `json:"session"`
		Pending string  `json:"pending"`
		Agent   struct {
			Harness *string `json:"harness"`
		} `json:"agent"`
	}
	call := map[string]any{"client": "codex-mcp-client", "meta": codexMeta("thr-1"), "user_agent": codexUA, "tool": "unwatch"}
	if err := coretest.RawApp(w.app, "POST", "/api/apps/v1/calls/session", map[string]any{"call": call}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Session != nil || out.Pending != "" || out.Agent.Harness == nil || *out.Agent.Harness != "codex" {
		t.Fatalf("/calls/session by the thread alone: session %v, pending %q, harness %v", out.Session != nil, out.Pending, out.Agent.Harness)
	}
	if n := appTenants(t, c, "shop"); n != before {
		t.Fatalf("/calls/session by the thread alone made %d workspaces", n-before)
	}

	// The session key finds it: the watch's own session, and it ends.
	ended, resolved = w.unwatch(t, "anon", codexUA, "w1", map[string]any{"session_key": string(sub.Session)}, codexMeta("thr-1"))
	if !ended || resolved != sub.Session {
		t.Fatalf("with the key: ended %v, the watch's session %v", ended, resolved == sub.Session)
	}
	if noteLive(t, c, sub.ID) {
		t.Fatal("the watch is still live")
	}
}

// With a user, a Codex thread names the user's conversation: ResolveSession
// on unwatch, without the session key, is the session watch's Subscribe
// used in that thread, and Unsubscribe ends the watch. Another thread of
// the same user, and the same thread ID under another user, are other
// conversations: neither resolves to the first, nor ends its watch.
func TestResolveSessionCodexThreadWithUser(t *testing.T) {
	c := coretest.Start(t)
	w := newWatchApp(t, c, "shop")
	ada1 := w.watch(t, "u:ada", codexUA, "w1", nil, codexMeta("thr-1"))
	ada2 := w.watch(t, "u:ada", codexUA, "w1", nil, codexMeta("thr-2"))
	bo1 := w.watch(t, "u:bo", codexUA, "w1", nil, codexMeta("thr-1"))
	if ada1.Session == ada2.Session || ada1.Session == bo1.Session || ada1.ID == ada2.ID || ada1.ID == bo1.ID {
		t.Fatal("two conversations share a session or a subscription")
	}

	// Ada's second thread: its own session, and only its own watch ends.
	ended, resolved := w.unwatch(t, "u:ada", codexUA, "w1", nil, codexMeta("thr-2"))
	if !ended || resolved != ada2.Session {
		t.Fatalf("ada's thr-2: ended %v, its own session %v", ended, resolved == ada2.Session)
	}
	if !noteLive(t, c, ada1.ID) || !noteLive(t, c, bo1.ID) || noteLive(t, c, ada2.ID) {
		t.Fatal("thr-2's unwatch ended another conversation's watch, or not its own")
	}
	// Bo, with ada's thread ID: bo's conversation.
	ended, resolved = w.unwatch(t, "u:bo", codexUA, "w1", nil, codexMeta("thr-1"))
	if !ended || resolved != bo1.Session {
		t.Fatalf("bo's thr-1: ended %v, bo's own session %v", ended, resolved == bo1.Session)
	}
	if !noteLive(t, c, ada1.ID) {
		t.Fatal("bo's unwatch ended ada's watch")
	}
	// Ada's first thread, without the key: the watch's session, and it ends.
	ended, resolved = w.unwatch(t, "u:ada", codexUA, "w1", nil, codexMeta("thr-1"))
	if !ended || resolved != ada1.Session {
		t.Fatalf("ada's thr-1: ended %v, the watch's session %v", ended, resolved == ada1.Session)
	}
	if noteLive(t, c, ada1.ID) {
		t.Fatal("ada's thr-1 watch is still live")
	}
	// Resolving again, with nothing left to end: still the same session.
	if ended, resolved = w.unwatch(t, "u:ada", codexUA, "w1", nil, codexMeta("thr-1")); ended || resolved != ada1.Session {
		t.Fatalf("again: ended %v, the same session %v", ended, resolved == ada1.Session)
	}
}

// Claude Code names its conversation only by the session key, with a user
// or without: unwatch without it resolves nothing and ends nothing; with
// it, the watch's own session, and it ends. Another conversation's key
// never resolves to the first's.
func TestResolveSessionClaudeCode(t *testing.T) {
	c := coretest.Start(t)
	w := newWatchApp(t, c, "shop")
	const ua = "claude-code/2.1.280"
	for token, id := range map[string]string{"u:ada": "w-ada", "anon": "w-anon"} {
		t.Run(token, func(t *testing.T) {
			sub := w.watch(t, token, ua, id, nil, nil)
			other := w.watch(t, token, ua, id, nil, nil) // no key: a second conversation
			if other.Session == sub.Session {
				t.Fatal("a call without the key joined the first conversation")
			}
			if ended, resolved := w.unwatch(t, token, ua, id, nil, nil); ended || resolved != "" {
				t.Fatalf("without the key: ended %v, resolved a session %v", ended, resolved != "")
			}
			if !noteLive(t, c, sub.ID) || !noteLive(t, c, other.ID) {
				t.Fatal("a watch ended without the key")
			}
			ended, resolved := w.unwatch(t, token, ua, id, map[string]any{"session_key": string(other.Session)}, nil)
			if !ended || resolved != other.Session {
				t.Fatalf("the second's key: ended %v, its own session %v", ended, resolved == other.Session)
			}
			if !noteLive(t, c, sub.ID) {
				t.Fatal("the second conversation's key ended the first's watch")
			}
			ended, resolved = w.unwatch(t, token, ua, id, map[string]any{"session_key": string(sub.Session)}, nil)
			if !ended || resolved != sub.Session || noteLive(t, c, sub.ID) {
				t.Fatalf("the first's key: ended %v, its own session %v", ended, resolved == sub.Session)
			}
		})
	}
}
