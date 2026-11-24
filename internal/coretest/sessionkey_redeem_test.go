package coretest_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/connector"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// keylessChat is an app without users (no Identify): each conversation has
// a workspace of its own, named by the session_key its results give
// (wsc_…), which the agent passes back in its calls' arguments.
func keylessChat(t *testing.T, c *coretest.Core) string {
	t.Helper()
	app := bareApp(t, c, "keyless")
	msg, err := wakeshift.DefineEvent(app, "keyless.message", wakeshift.EventDef[relayArgs, map[string]string]{
		Description: "A message in a channel.", Topic: "keyless/{channel}",
		Text: func(m map[string]string, _ relayArgs) string { return m["text"] },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.PublishManifest(ctx, wakeshift.Manifest{Description: "Keyless chat."}); err != nil {
		t.Fatal(err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "keyless", Version: "1"}, nil)
	at := app.Attach(srv, wakeshift.AttachOptions{})
	type watchIn struct{}
	wakeshift.AddTool(at, &mcp.Tool{Name: "watch"}, func(ctx context.Context, call *wakeshift.ToolCall, in watchIn) (*mcp.CallToolResult, any, error) {
		if _, err := msg.Subscribe(ctx, call, relayArgs{Channel: "general"}); err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Watching #general."}}}, nil, nil
	}, wakeshift.Subscribes(msg))
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	hs := httptest.NewServer(at.Handler(h))
	t.Cleanup(hs.Close)
	return hs.URL
}

func keylessAgent(t *testing.T, url string) *relayAgent {
	t.Helper()
	s, err := mcp.NewClient(&mcp.Implementation{Name: "Cursor"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return &relayAgent{t: t, s: s}
}

var sessionKeyRE = regexp.MustCompile(`wsc_[A-Za-z0-9_-]+`)

// A Cursor conversation's session_key alone doesn't give its messages
// away (#1152): a call with the key gets a result whose beacon names the
// conversation, but once the conversation's hooks are connected, a redeem
// that doesn't show their link is refused (conversation_taken), and the
// hooks keep the conversation. The hooks' own redeems, which show it,
// still work; once their link has expired (a hook that lost it,
// reinstalled), the hooks connect again.
func TestSessionKeyAloneDoesntRedeem(t *testing.T) {
	c := coretest.Start(t)
	url := keylessChat(t, c)
	const conv = "5d0b5a64-8a4e-4f0a-9d55-2b8a2c6f1e10"
	victim := keylessAgent(t, url)
	hook := localHook(t, c, "cursor")

	res := victim.call("watch", map[string]any{"harness": "cursor", "conversation_id": conv})
	key := sessionKeyRE.FindString(allText(res))
	if key == "" {
		t.Fatalf("no session_key in the result: %q", allText(res))
	}
	runLocalHook(hook, "", cursorPost(t, conv, "watch", res))
	held := hook.Links.Load(conv)
	if held == "" {
		t.Fatal("the victim's hooks didn't connect")
	}

	// Mallory has the key and the conversation ID (both were in the dialog).
	mallory := keylessAgent(t, url)
	keys := coreKeys(t, c)
	stolen := func() beacon.Claims {
		t.Helper()
		mres := mallory.call("watch", map[string]any{"harness": "cursor", "conversation_id": conv, "session_key": key})
		tok := beacon.Trailer(lastText(mres))
		if tok == "" {
			t.Fatalf("no beacon for the key's holder: %q", lastText(mres))
		}
		claims, err := beacon.Verify(tok, conv, []string{c.URL}, keys, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return claims
	}
	claims := stolen()
	if code, out := redeemTicket(t, claims.Redeem, claims.Ticket, conv); code != http.StatusConflict {
		t.Fatalf("redeemed with the key alone: %d %v", code, out)
	}
	if hook.Links.Load(conv) != held {
		t.Fatal("the victim's link changed")
	}

	// The victim's own hooks redeem the next beacon (they show their link).
	res = victim.call("watch", map[string]any{"harness": "cursor", "conversation_id": conv, "session_key": key})
	if out := runLocalHook(hook, "", cursorPost(t, conv, "watch", res)); strings.Contains(out, connector.NoteTaken) || strings.Contains(out, connector.NoteLinkNotHeld) {
		t.Fatalf("the victim's hooks were refused: %s", out)
	}
	if now := hook.Links.Load(conv); now == "" || now == held {
		t.Fatalf("the victim's hooks didn't reconnect: %q", now)
	}

	// The session's first link, revoked (or expired): found in an old log
	// with the key, it proves nothing while the hooks' current one works.
	if err := c.DB.InTenant(ctx, sessionTenant(t, c, key), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE stream_links SET revoked_at = now() WHERE id = (SELECT id FROM stream_links
			WHERE coalesce(connector, '') <> '' ORDER BY created_at LIMIT 1)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if code, out := redeemHeld(t, stolen(), conv, held); code != http.StatusConflict {
		t.Fatalf("redeemed with a revoked link of the session: %d %+v", code, out)
	}

	// Their links expired (nothing has used them: the hooks were removed):
	// a redeem without one connects the conversation again.
	if err := c.DB.InTenant(ctx, sessionTenant(t, c, key), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE stream_links SET expires_at = now() - interval '1 second' WHERE coalesce(connector, '') <> ''`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	claims = stolen()
	if code, out := redeemTicket(t, claims.Redeem, claims.Ticket, conv); code != http.StatusOK {
		t.Fatalf("after the hooks' links expired: %d %v", code, out)
	}
}

// sessionTenant is the workspace a conversation's session_key names.
func sessionTenant(t *testing.T, c *coretest.Core, key string) string {
	t.Helper()
	var id string
	if err := c.DB.Pool.QueryRow(ctx, `SELECT id::text FROM tenants WHERE external_id = '~conversation:' || $1`, strings.TrimPrefix(key, "wsc_")).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// Two redeems for a conversation nothing has connected yet, at once: they
// take turns, so only one connects it and the other finds its link and is
// refused.
func TestFirstRedeemsTakeTurns(t *testing.T) {
	c := coretest.Start(t)
	url := keylessChat(t, c)
	keys := coreKeys(t, c)
	for i := range 5 {
		conv := fmt.Sprintf("5d0b5a64-8a4e-4f0a-9d55-2b8a2c6f1e%02d", 20+i)
		victim := keylessAgent(t, url)
		res := victim.call("watch", map[string]any{"harness": "cursor", "conversation_id": conv})
		key := sessionKeyRE.FindString(allText(res))
		if key == "" {
			t.Fatalf("no session_key in the result: %q", allText(res))
		}
		var claims [2]beacon.Claims
		for j := range claims {
			r := victim.call("watch", map[string]any{"harness": "cursor", "conversation_id": conv, "session_key": key})
			cl, err := beacon.Verify(beacon.Trailer(lastText(r)), conv, []string{c.URL}, keys, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			claims[j] = cl
		}
		var codes [2]int
		var wg sync.WaitGroup
		for j := range claims {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes[j], _ = redeemTicket(t, claims[j].Redeem, claims[j].Ticket, conv)
			}()
		}
		wg.Wait()
		ok := 0
		for _, code := range codes {
			switch code {
			case http.StatusOK:
				ok++
			case http.StatusConflict:
			default:
				t.Fatalf("round %d: %v", i, codes)
			}
		}
		if ok != 1 {
			t.Fatalf("round %d: %d of 2 concurrent first redeems connected it (%v)", i, ok, codes)
		}
	}
}
