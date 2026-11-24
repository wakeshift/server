package coretest_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/apps/agentkit"
	"github.com/wakeshift/server/apps/gamekit/gametest"
	"github.com/wakeshift/server/apps/secretagent"
	"github.com/wakeshift/server/internal/coretest"
)

// A game signs its players in itself (apps/gamekit/oauth.go): each
// Authorize is a session, which Wakeshift knows as an ordinary app user
// (its workspace's external ID). A registration with a redirect URI that
// isn't a player's own agent's is refused. A session idle for 31 days is
// forgotten: the game has Wakeshift forget the user, and its token stops
// working.
func TestGameSignIn(t *testing.T) {
	c := coretest.Start(t)
	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	mcpURL := srv.URL + "/mcp"
	key := c.RegisterApp("secretagent", "Secret Agent", mcpURL)
	newApp := func() *secretagent.App {
		return &secretagent.App{Kit: &agentkit.App{Client: coretest.SDKClient(t, "secretagent", key, c.URL), ID: "secretagent", Key: key, MCPURL: mcpURL}}
	}
	app := newApp()
	store, storeURL := gametest.Store(t)
	app.Store = store
	srv.Config.Handler = app.Handler()
	if err := app.Start(ctx); err != nil {
		t.Fatal(err)
	}

	for _, redirect := range []string{"https://evil.example/cb", "https://claude.ai.evil.example/api/mcp/auth_callback"} {
		b, _ := json.Marshal(map[string]any{"client_name": "Evil", "redirect_uris": []string{redirect}})
		resp, err := http.Post(srv.URL+"/oauth/register", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		var e struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&e)
		resp.Body.Close()
		if resp.StatusCode != 400 || e.Error != "invalid_redirect_uri" {
			t.Fatalf("registering %s: %d %s", redirect, resp.StatusCode, e.Error)
		}
	}

	token := c.AppSignIn(srv.URL, "Claude Code")
	p := gametest.Connect(t, mcpURL, token)
	p.Try("game", nil) // acts for the player's agent
	var player string
	if err := store.Pool.QueryRow(ctx, `SELECT id FROM player_sessions`).Scan(&player); err != nil {
		t.Fatal(err)
	}
	var users []string
	rows, _ := c.DB.Pool.Query(ctx, `SELECT external_id FROM tenants WHERE app_id = 'secretagent'`)
	for rows.Next() {
		var u string
		rows.Scan(&u)
		users = append(users, u)
	}
	if !strings.HasPrefix(player, "p_") || len(users) != 1 || users[0] != player {
		t.Fatalf("the player %q; Wakeshift's users of the app %v", player, users)
	}

	// Idle for 31 days: another instance's first hourly pass forgets it.
	if _, err := store.Pool.Exec(ctx, `UPDATE player_sessions SET active_at = now() - interval '32 days'`); err != nil {
		t.Fatal(err)
	}
	other := newApp()
	other.Store = gametest.Open(t, storeURL)
	gametest.Run(t, other.Run)
	waitFor(t, "the idle session forgotten in Wakeshift", func() bool {
		var n int
		c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE app_id = 'secretagent'`).Scan(&n)
		return n == 0
	})
	waitFor(t, "the idle session deleted", func() bool {
		var n int
		store.Pool.QueryRow(ctx, `SELECT count(*) FROM player_sessions`).Scan(&n)
		return n == 0
	})
	// Its token's player is gone: the game can't act for it. (A fresh
	// connection, so nothing is cached for it.)
	time.Sleep(10 * time.Millisecond)
	if _, errText, err := gametest.Connect(t, mcpURL, token).Call("join", map[string]any{}); errText == "" && err == nil {
		t.Fatal("a forgotten player's token still plays")
	}
}
